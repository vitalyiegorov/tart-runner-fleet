package app

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"

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
	// Placed is the part of Held that already has a guest on this node. Held
	// beyond it is work waiting for one, which is what the standing offer of
	// one (issue #422) may never exceed.
	Placed int
	// Gated is the node's own guardrails refusing admission for host pressure
	// (hostHealthy is false): memory, swap, CPU, disk or inode. An update drain
	// is not a gate. A gated node is offered no standing slot (issue #428).
	Gated bool
	// Stranded is the set's vector not fitting this node even once every Linux
	// guest here has drained (scheduler.FitsOnceLinuxDrains): macOS guests or
	// queued macOS work hold what it needs. A stranded set is offered no
	// standing slot either (issue #439).
	Stranded bool
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
// Nor does it ever leave a set without one free slot while none of its work is
// waiting for a guest, a standing offer (issue #422). GitHub assigns a job only
// to a set advertising room beyond what it already holds when it queues the
// job, and never looks again, so a job queued while every set matching its
// labels had no free slot -- every node full, draining, or gated at once -- was
// assigned to no set and stayed queued after the nodes had freed. With one
// free slot it is assigned here and waits in this node's queue until admission
// starts it; once it waits, Held exceeds Placed and the slot is spent, so a
// full node gains at most one job per set it cannot start yet. A set
// configured at zero still advertises zero.
//
// A gated node keeps no standing offer (issue #428, ADR 0084). It cannot start
// what the free slot attracts, and a job assigned to it waits there while a
// sibling serving the same labels may be idle. It advertises exactly what it
// holds, so nothing GitHub already gave it is taken away and nothing more is
// invited. That holds for its residual share too: a guardrail that leaves
// memory free still measures a Fit, and neither that nor the residual a brief
// opening measured (issue #483) is offered while the node is gated.
//
// Nor does a set whose vector this node could not start even once all its
// Linux work has drained (issue #439, ADR 0085). The standing offer promises a
// job a wait for this node's own Linux work. On a mixed-platform node, macOS
// guests can hold what a Linux vector needs for hours, and the job would wait
// for them while a sibling has room. Such a set too advertises what it holds.
//
// Before the node has ever computed its headroom it is the configured value: a
// node that has never planned has no better answer, and advertising zero would
// withdraw it from every scope it serves.
func AdvertisedCapacity(configured int, headroom ScaleSetHeadroom, known bool) int {
	if !known {
		return configured
	}
	offered := max(0, headroom.Held)
	if !headroom.Gated {
		offered += max(0, headroom.Fit)
		if !headroom.Stranded {
			offered = max(offered, max(0, headroom.Placed)+1)
		}
	}
	return max(0, min(configured, offered))
}

// ResidualAdvertisement carries the scheduler's latest view of the node's
// residual to every scale-set session, so each long poll advertises only what
// that set holds plus its share of what the node can still start (ADR 0073).
//
// It is one value for the whole node, written by the ticker and read by every
// binding's poll: the envelope is a property of the machine, not of any one
// scale set, and every set must read the same conclusion the scheduler admitted
// on. The zero value is ready to use and knows nothing.
//
// A gate stays closed here until it has stayed open for Reopen (issue #483).
// A host that refuses admission most of the time and reopens for a few seconds
// between refusals cannot start what it is offered, yet each opening used to
// put every set back on the standing offer and the residual that opening
// measured, and a long poll carries the figure it began with for up to a
// minute. Zero follows the gate tick by tick.
type ResidualAdvertisement struct {
	// Reopen is how long the node's own guardrails must have admitted work,
	// continuously, before a gated node is offered more than it holds again.
	// The daemon passes sessionYieldHealthyForSeconds: one bound for "this
	// node can serve again", whether the question is rejoining or advertising.
	Reopen   time.Duration
	mu       sync.Mutex
	headroom map[int64]ScaleSetHeadroom
	gate     RefusalEpisode
}

// RefusalEpisode is a host's refusal to admit with its brief reopenings folded
// in (issue #483). It begins on the first refused tick and ends only once
// admission has been allowed continuously for Reopen, so a gate that opens for
// a few seconds between refusals neither restarts the episode nor ends it.
//
// It is the one definition of "this node can admit again" that the residual
// advertisement and session yield (ADR 0047) both read. They feed it different
// gates: yield counts an update drain as a refusal, the advertisement does not. The zero value is a
// host in no episode, which is what a fresh daemon assumes.
type RefusalEpisode struct {
	// Reopen is how long admission must be allowed, continuously, before the
	// episode ends. Zero ends it on the first allowed tick.
	Reopen time.Duration
	// since is when the episode began, zero outside one; allowedSince is when
	// the current run of allowed ticks inside it began, zero while refused.
	since, allowedSince time.Time
}

// Observe folds one tick into the episode and reports whether it is still on.
// A clock that moved backwards re-anchors the bound it is measuring rather
// than let a correction satisfy a bound no elapsed time earned.
func (e *RefusalEpisode) Observe(at time.Time, refused bool) bool {
	if refused {
		e.allowedSince = time.Time{}
		if e.since.IsZero() || at.Before(e.since) {
			e.since = at
		}
		return true
	}
	if e.since.IsZero() {
		return false
	}
	if e.allowedSince.IsZero() || at.Before(e.allowedSince) {
		e.allowedSince = at
	}
	if at.Sub(e.allowedSince) < e.Reopen {
		return true
	}
	e.since, e.allowedSince = time.Time{}, time.Time{}
	return false
}

// Since is when the current episode began; zero when the host is in none.
func (e *RefusalEpisode) Since() time.Time { return e.since }

// Observe records one tick. Only a tick that produced a ready plan and measured
// at least one scale set replaces the view, and only such a tick moves the gate.
// A tick with no row, as when every binding's statistics read fails, says
// nothing about the host's guardrails. Any other
// tick keeps the last view: a node that cannot plan cannot start what it is
// offered either, and dropping the view used to advertise every set's
// configured maximum exactly when the node was least able to run it (issue
// #397).
func (a *ResidualAdvertisement) Observe(result TickResult, err error) {
	if a == nil || err != nil || result.Plan.Status != scheduler.PlanReady || len(result.Headroom) == 0 {
		return
	}
	// Every row carries the same node-wide gate (hostHealthy), so any one of
	// them says whether this tick's guardrails refused.
	refused := false
	for _, row := range result.Headroom {
		refused = refused || row.Gated
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.gate.Reopen = a.Reopen
	held := a.gate.Observe(result.At, refused)
	view := make(map[int64]ScaleSetHeadroom, len(result.Headroom))
	for _, row := range result.Headroom {
		row.Gated = held
		view[row.StoreKey] = row
	}
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
	gated := !hostHealthy(in.Host)
	rows := make([]ScaleSetHeadroom, 0, len(bindings))
	claims := make([]scheduler.Claim, 0, len(bindings))
	for _, binding := range bindings {
		broker, err := c.brokerHeld(ctx, binding)
		if err != nil {
			continue
		}
		placed := 0
		for _, instance := range in.Instances.Value {
			if instance.HoldsAssignment() && instance.Profile == binding.Profile.ID && binding.accepts(instance.Repo) {
				placed++
			}
		}
		local := held[binding.durableKey()] + placed
		row := ScaleSetHeadroom{StoreKey: binding.durableKey(), Held: max(local, broker), Placed: placed,
			Gated: gated, Stranded: !scheduler.FitsOnceLinuxDrains(in, binding.Profile.ID)}
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
