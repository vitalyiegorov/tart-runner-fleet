package daemon

import (
	"context"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/membership"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/telemetry"
)

// membershipController is ADR 0063's rungs (a) and (b) on this node: it reads
// the scale-set audit's ADR 0056 verdict for the sets this node serves, folds it
// into the pure ladder, and turns the ladder's decisions into the sessions
// those sets hold.
//
// It adds no mechanism of its own. A heal is the recovery a daemon restart
// performs -- close and reopen one session through the same factory ADR 0009's
// recovery uses -- and an exclusion is ADR 0047's withdrawal applied to one
// binding, carried out by the session-yield controller's level-triggered loop
// rather than by a second one.
type membershipController struct {
	policy  membership.Policy
	keys    []membership.Key
	sources map[membership.Key]yieldableSource
	state   map[membership.Key]membership.Binding
	// excluded is the set of sources the ladder holds released, read by the
	// session-yield controller on the same goroutine every tick.
	excluded map[yieldableSource]bool
	reporter *failureReporter
}

// newMembershipController returns nil for a node that declared no membership or
// owns no session to act on, so the caller never special-cases either.
func newMembershipController(mode membership.Mode, cadence time.Duration, sources map[membership.Key]yieldableSource,
	reporter *failureReporter) *membershipController {
	if !mode.Enabled() || len(sources) == 0 {
		return nil
	}
	keys := make([]membership.Key, 0, len(sources))
	for key := range sources {
		keys = append(keys, key)
	}
	membership.SortKeys(keys)
	return &membershipController{policy: membership.Policy{Mode: mode, Cadence: cadence}, keys: keys,
		sources: sources, state: map[membership.Key]membership.Binding{},
		excluded: map[yieldableSource]bool{}, reporter: reporter}
}

// excludes reports whether the ladder holds this source's session released.
func (c *membershipController) excludes(source yieldableSource) bool {
	return c != nil && c.excluded[source]
}

// Apply folds one tick's evidence into the ladder and performs its heals. A
// node that has withdrawn every session (ADR 0047) has nothing to heal and
// nothing to judge, so its ladder holds still until it rejoins.
func (c *membershipController) Apply(ctx context.Context, snapshot telemetry.Snapshot, now time.Time, nodeYielded bool) membership.Summary {
	if c == nil {
		return membership.Summary{}
	}
	if nodeYielded {
		return membership.Summarize(c.state)
	}
	evidence := membership.Evidence{At: now, AuditedAt: snapshot.ParkedScaleSetsAuditedAt, Bound: c.keys}
	for _, row := range snapshot.StrandedScaleSets {
		evidence.Wedged = append(evidence.Wedged, membership.Key{Scope: row.Scope, ID: row.ScaleSetID})
	}
	next, actions := membership.Step(c.policy, c.state, evidence)
	c.state = next
	for _, key := range c.keys {
		source := c.sources[key]
		c.excluded[source] = next[key].Stage == membership.Excluded
		action, ok := actions[key]
		if !ok {
			continue
		}
		if action == membership.Heal && source.Suspend(ctx) == nil {
			// A failed reopen leaves the binding suspended and not excluded, so
			// the yield controller's next pass reopens it: the heal can never
			// strand a set outside the fleet.
			_ = source.Resume(ctx)
		}
		c.reporter.reportMembership(key, action, next[key])
	}
	return membership.Summarize(next)
}

// metric projects the ladder for publication.
func (c *membershipController) metric(summary membership.Summary) telemetry.MembershipMetric {
	metric := telemetry.MembershipMetric{Mode: string(c.policy.Mode), State: string(summary.State)}
	for _, row := range summary.Bindings {
		metric.Bindings = append(metric.Bindings, telemetry.MembershipBindingMetric{Scope: row.Scope, ScaleSetID: row.ID,
			Stage: string(row.Stage), Since: row.Since, Attempts: row.Attempts, Exclusions: row.Exclusions})
	}
	return metric
}

// reportMembership logs one ladder transition. It is never rate limited: each
// is rare, deliberate, and changes whether GitHub binds work to this node.
func (r *failureReporter) reportMembership(key membership.Key, action membership.Action, binding membership.Binding) {
	if r == nil || r.logger == nil {
		return
	}
	r.logger.Warn("scale-set membership "+string(action), "scope", key.Scope, "scaleSet", key.ID,
		"stage", string(binding.Stage), "attempts", binding.Attempts, "exclusions", binding.Exclusions)
}
