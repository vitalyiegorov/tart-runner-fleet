package app

import (
	"context"
	"errors"
	"math"
	"sync"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

// ScaleSetHeadroom is one scale set's share of the node's residual envelope
// (issues #388 and #397, ADR 0073): what the set already holds, and how many more
// of its own vector it may be offered.
type ScaleSetHeadroom struct {
	StoreKey int64
	// Held is the work this set already owns: its queued, not-yet-launched demand
	// and the guests still holding an assignment for it, or GitHub's own assigned
	// count for the set when fresh statistics say more. It is deliberately the
	// larger of the two -- an over-count advertises more, while an under-count
	// could advertise below what GitHub has already given the set.
	Held int
	// Fit is this set's share of the node's residual, from
	// scheduler.PartitionHeadroom. The shares of every set on the node add up to
	// what the residual can start once, never once per set.
	Fit int
}

// AdvertisedCapacity is the capacity a scale set sends GitHub on its next long
// poll: the configured truthful capacity, lowered to what the set holds plus
// its share of what the node's residual can still start.
//
// It only ever lowers the configured value, so ADR 0015's truthful capacity
// stays the ceiling and `fleet config validate` keeps its meaning. It never
// lowers it below what the set holds (up to that ceiling): GitHub has already
// placed that work here, and an assigned job cannot be moved without a cancel.
//
// Before the node has ever computed its headroom it is the configured value: a
// node that has never planned has no better answer, and advertising zero would
// withdraw it from every scope it serves.
func AdvertisedCapacity(configured int, headroom ScaleSetHeadroom, known bool) int {
	if !known {
		return configured
	}
	return max(0, min(configured, max(0, headroom.Held)+max(0, headroom.Fit)))
}

// ResidualAdvertisement carries the scheduler's latest view of the node's
// residual to every scale-set session, so each long poll advertises only what
// that set holds plus its share of what the node can still start (ADR 0073).
//
// It is one value for the whole node, written by the ticker and read by every
// binding's poll: the envelope is a property of the machine, not of any one
// scale set, and every set must read the same conclusion the scheduler admitted
// on. The zero value is ready to use and knows nothing.
type ResidualAdvertisement struct {
	mu       sync.Mutex
	headroom map[int64]ScaleSetHeadroom
}

// Observe records one tick. Only a tick that produced a ready plan and a
// headroom replaces the view. Any other tick keeps the last one: a node that
// cannot plan cannot start what it is offered either, and dropping the view
// used to advertise every set's configured maximum exactly when the node was
// least able to run it (issue #397).
func (a *ResidualAdvertisement) Observe(result TickResult, err error) {
	if a == nil || err != nil || result.Plan.Status != scheduler.PlanReady || result.Headroom == nil {
		return
	}
	view := make(map[int64]ScaleSetHeadroom, len(result.Headroom))
	for _, row := range result.Headroom {
		view[row.StoreKey] = row
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.headroom = view
}

// Capacity is what one scale set advertises on its next poll.
func (a *ResidualAdvertisement) Capacity(storeKey int64, configured int) int {
	if a == nil {
		return configured
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	row, known := a.headroom[storeKey]
	return AdvertisedCapacity(configured, row, known)
}

// scaleSetHeadroom computes every binding's headroom from the inputs this tick
// planned on. waiting is every plannable demand across the node, before the
// statistics trickle; held is each binding's own share of it, and undelivered
// each binding's REST-observed jobs its session has not been handed. A binding
// whose statistics are unreadable, or whose profile cannot be sized, is left
// out, which is "unknown", never zero.
func (c DemandCoordinator) scaleSetHeadroom(ctx context.Context, bindings []Binding, in scheduler.Input,
	held, undelivered map[int64]int,
) []ScaleSetHeadroom {
	if !in.Instances.Usable() {
		return nil
	}
	rows := make([]ScaleSetHeadroom, 0, len(bindings))
	claims := make([]scheduler.Claim, 0, len(bindings))
	for _, binding := range bindings {
		broker, err := c.brokerHeld(ctx, binding)
		if err != nil {
			continue
		}
		local := held[binding.durableKey()]
		for _, instance := range in.Instances.Value {
			if instance.HoldsAssignment() && instance.Profile == binding.Profile.ID && binding.accepts(instance.Repo) {
				local++
			}
		}
		row := ScaleSetHeadroom{StoreKey: binding.durableKey(), Held: max(local, broker)}
		room := math.MaxInt
		if binding.Capacity > 0 {
			room = max(0, binding.Capacity-row.Held)
		}
		rows = append(rows, row)
		claims = append(claims, scheduler.Claim{Profile: binding.Profile.ID, Room: room,
			Waiting: undelivered[binding.durableKey()]})
	}
	shares, ok := scheduler.PartitionHeadroom(in, claims)
	if !ok {
		return nil
	}
	sized := rows[:0]
	for index, row := range rows {
		if shares[index] == scheduler.Unsized {
			continue
		}
		row.Fit = shares[index]
		sized = append(sized, row)
	}
	return sized
}

// brokerHeld is GitHub's own count of the work it has placed on this set and
// not finished: TotalAssignedJobs, which includes running jobs and is the
// figure the protocol scales by. The other counters are not documented as
// disjoint from it -- one job can be reported both acquired and assigned -- so
// summing them would count one held job twice and over-advertise. It is
// zero when the statistics are absent or stale, because then the set's durable
// demand and live instances are the better record of what it holds; an
// unreadable store is an error, not a zero.
func (c DemandCoordinator) brokerHeld(ctx context.Context, binding Binding) (int, error) {
	store, ok := c.Store.(DemandStatisticsStore)
	if !ok {
		return 0, nil
	}
	statistics, err := store.DemandStatistics(ctx, binding.durableKey())
	if errors.Is(err, operations.ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	now := c.now()
	if !statistics.Valid() || statistics.ObservedAt.IsZero() || statistics.ObservedAt.After(now) ||
		now.Sub(statistics.ObservedAt) > c.statisticsMaxAge() {
		return 0, nil
	}
	return statistics.Assigned, nil
}
