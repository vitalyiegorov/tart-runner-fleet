package app

import (
	"context"
	"errors"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

// ScaleSetHeadroom is one scale set's share of the node's residual envelope
// (issue #388, ADR 0073): what the set already holds, and how many more of its
// own vector the node could start right now.
type ScaleSetHeadroom struct {
	StoreKey int64
	// Held is the work this set already owns: its live instances and its queued,
	// not-yet-launched demand, or GitHub's own assigned count for the set when
	// fresh statistics say more. It is deliberately the
	// larger of the two -- an over-count advertises more, which is today's
	// behaviour, while an under-count could advertise below what GitHub has
	// already given the set.
	Held int
	// Fit is scheduler.Headroom for the set's profile: net of every demand
	// waiting anywhere on this node, so assigned-but-unlaunched work is charged
	// before anything new is advertised.
	Fit int
}

// AdvertisedCapacity is the capacity a scale set sends GitHub on its next long
// poll: the configured truthful capacity, lowered to what the set holds plus
// what the node's residual can still start.
//
// It only ever lowers the configured value, so ADR 0015's truthful capacity
// stays the ceiling and `fleet config validate` keeps its meaning. It never
// lowers it below what the set holds (up to that ceiling): GitHub has already
// placed that work here, and an assigned job cannot be moved without a cancel.
//
// When the headroom is not known -- no tick has completed, the envelope's
// observations were unusable, or the set's statistics could not be read -- it
// is the configured value. That is today's behaviour exactly, and it is the
// safe direction: advertising zero on missing data would silently withdraw a
// node from every scope it serves on a probe hiccup, which is a liveness
// failure the scheduler's own fail-closed tick would then hide, because a node
// that is offered nothing has nothing to report as waiting.
func AdvertisedCapacity(configured int, headroom ScaleSetHeadroom, known bool) int {
	if !known {
		return configured
	}
	return max(0, min(configured, max(0, headroom.Held)+max(0, headroom.Fit)))
}

// scaleSetHeadroom computes every binding's headroom from the inputs this tick
// planned on. waiting is every plannable demand across the node, before the
// statistics trickle, and held is each binding's own share of it. A binding
// whose statistics are unreadable is left out, which is "unknown", never zero.
func (c DemandCoordinator) scaleSetHeadroom(ctx context.Context, bindings []Binding, in scheduler.Input,
	held map[int64]int,
) []ScaleSetHeadroom {
	if !in.Instances.Usable() {
		return nil
	}
	rows := make([]ScaleSetHeadroom, 0, len(bindings))
	for _, binding := range bindings {
		fit, ok := scheduler.Headroom(in, binding.Profile.ID)
		if !ok {
			continue
		}
		broker, err := c.brokerHeld(ctx, binding)
		if err != nil {
			continue
		}
		local := held[binding.durableKey()]
		for _, instance := range in.Instances.Value {
			if instance.Live() && instance.Profile == binding.Profile.ID && binding.accepts(instance.Repo) {
				local++
			}
		}
		rows = append(rows, ScaleSetHeadroom{StoreKey: binding.durableKey(), Held: max(local, broker), Fit: fit})
	}
	return rows
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
