package daemon

import (
	"context"
	"time"
)

// sessionYieldAction is what the policy asks the daemon to do with the scale-set
// sessions it holds. It is deliberately a decision and not an effect: the state
// machine is pure so the rule can be tested against a clock instead of against
// GitHub.
type sessionYieldAction int

const (
	sessionYieldNone sessionYieldAction = iota
	// sessionYieldWithdraw closes every scale-set session this node owns. The
	// scale sets keep existing; what stops is this node's claim on the work
	// GitHub would otherwise bind to them.
	sessionYieldWithdraw
	// sessionYieldRejoin re-opens them through the ordinary recovery path.
	sessionYieldRejoin
)

func (a sessionYieldAction) String() string {
	switch a {
	case sessionYieldWithdraw:
		return "withdraw"
	case sessionYieldRejoin:
		return "rejoin"
	default:
		return "none"
	}
}

// sessionYieldPolicy bounds how long a node must have been unable to admit
// before it stops holding the sessions that make GitHub bind jobs to it.
//
// GitHub binds a queued job to exactly one scale set, and ADR 0034 gives that
// set exactly one session — so a node that cannot admit is not merely idle, it
// is holding work no sibling is allowed to take. Measured on 2026-08-27, a
// scale set whose session is closed stops attracting new jobs and the sibling
// advertising the same labels picks them up; a job already bound does not
// migrate. The whole value is therefore in withdrawing before a backlog forms,
// which is why the blocked bound is minutes rather than hours.
type sessionYieldPolicy struct {
	Enabled bool
	// BlockedFor is how long admission must have been continuously refused,
	// with the node otherwise idle, before withdrawing.
	BlockedFor time.Duration
	// HealthyFor is how long admission must have been continuously allowed
	// before rejoining. Both bounds exist so a node sitting on the edge of its
	// disk reserve cannot flap sessions.
	HealthyFor time.Duration
}

// sessionYieldFacts is one tick's view of whether this node can serve.
type sessionYieldFacts struct {
	At time.Time
	// AdmissionAllowed is the host probe's own decision — the same fact the
	// scheduler admits on and `fleet status` prints, never a second opinion.
	AdmissionAllowed bool
	// LiveInstances and BusyOperations keep a withdrawal from cutting the
	// session that running work still reports its completion through. A session
	// is not only intake.
	LiveInstances  int
	BusyOperations int
}

// sessionYieldState tracks the current streak and whether this node has already
// withdrawn. Zero value is a node that has never observed anything and holds its
// sessions, which is what a fresh daemon should be.
type sessionYieldState struct {
	policy sessionYieldPolicy
	// yielded is the node's own admission that it is not currently serving.
	yielded bool
	// streakSince is when the current blocked/allowed condition began, and
	// streakBlocked is which condition that is. Together they are the hysteresis.
	streakSince   time.Time
	streakBlocked bool
	observed      bool
}

func newSessionYieldState(policy sessionYieldPolicy) *sessionYieldState {
	return &sessionYieldState{policy: policy}
}

// Yielded reports whether this node has withdrawn its sessions. It exists so the
// condition can be published: a node that has stopped taking work must never be
// indistinguishable from a healthy idle one, which was the unreadable half of
// issue #292.
func (s *sessionYieldState) Yielded() bool { return s != nil && s.yielded }

// Observe folds one tick into the state machine and returns the action the
// daemon owes GitHub. It is total: every input produces a decision, and the same
// inputs always produce the same decision.
func (s *sessionYieldState) Observe(facts sessionYieldFacts) sessionYieldAction {
	if s == nil {
		return sessionYieldNone
	}
	if !s.policy.Enabled {
		// Disabling the policy while withdrawn must not strand the node outside
		// the fleet: it rejoins on the next tick and stays.
		if s.yielded {
			s.yielded = false
			s.observed = false
			return sessionYieldRejoin
		}
		return sessionYieldNone
	}
	blocked := !facts.AdmissionAllowed
	// A clock that moved backwards is not a longer streak. Restart rather than
	// let a correction satisfy a bound no elapsed time earned.
	if !s.observed || s.streakBlocked != blocked || facts.At.Before(s.streakSince) {
		s.observed = true
		s.streakBlocked = blocked
		s.streakSince = facts.At
	}
	if s.yielded {
		if !blocked && facts.At.Sub(s.streakSince) >= s.policy.HealthyFor {
			s.yielded = false
			s.streakSince = facts.At
			return sessionYieldRejoin
		}
		return sessionYieldNone
	}
	if !blocked {
		return sessionYieldNone
	}
	// Work in flight is not idleness. Hold the sessions and restart the clock,
	// so the node cannot withdraw the instant the last instance retires.
	if facts.LiveInstances > 0 || facts.BusyOperations > 0 {
		s.streakSince = facts.At
		return sessionYieldNone
	}
	if facts.At.Sub(s.streakSince) >= s.policy.BlockedFor {
		s.yielded = true
		s.streakSince = facts.At
		return sessionYieldWithdraw
	}
	return sessionYieldNone
}

// yieldableSource is the part of a scale-set source the yield controller needs.
// Keeping it narrow is what lets the controller be tested without a broker.
type yieldableSource interface {
	Suspend(context.Context) error
	Resume(context.Context) error
	Suspended() bool
}

// sessionYieldController turns the policy's decision into the sessions this node
// actually holds. It owns the retry: a withdrawal whose close GitHub refused is
// not a withdrawal, and a rejoin whose open failed must be attempted again
// rather than leaving the node silently outside the fleet it believes it is in.
type sessionYieldController struct {
	state   *sessionYieldState
	sources []yieldableSource
	// report names what happened, once per transition, so an operator reading
	// stderr learns the node withdrew and why rather than inferring it from an
	// empty queue.
	report func(action sessionYieldAction, reason string, failures int)
}

// Apply folds one tick's facts into the policy and reconciles the sessions with
// the conclusion. It returns whether this node is currently withdrawn.
//
// Reconciliation is level-triggered, not edge-triggered: every tick drives the
// sources toward the state the policy holds, so a partial failure heals on the
// next tick instead of leaving half this node's bindings serving.
func (c *sessionYieldController) Apply(ctx context.Context, facts sessionYieldFacts, reason string) bool {
	if c == nil || c.state == nil {
		return false
	}
	action := c.state.Observe(facts)
	want := c.state.Yielded()
	failures := 0
	for _, source := range c.sources {
		if source == nil || source.Suspended() == want {
			continue
		}
		var err error
		if want {
			err = source.Suspend(ctx)
		} else {
			err = source.Resume(ctx)
		}
		if err != nil {
			failures++
		}
	}
	if action != sessionYieldNone && c.report != nil {
		c.report(action, reason, failures)
	}
	return want
}

// Bindings reports how many scale-set sessions this node owns and how many are
// actually released. They differ only while a release GitHub refused is being
// retried, and publishing both is what keeps "yielded" from overstating a
// withdrawal that has not finished.
func (c *sessionYieldController) Bindings() (total, withdrawn int) {
	if c == nil {
		return 0, 0
	}
	for _, source := range c.sources {
		if source == nil {
			continue
		}
		total++
		if source.Suspended() {
			withdrawn++
		}
	}
	return total, withdrawn
}

// Since is when the current condition began, so an operator reads how long this
// node has been withdrawn rather than only that it is.
func (c *sessionYieldController) Since() time.Time {
	if c == nil || c.state == nil {
		return time.Time{}
	}
	return c.state.streakSince
}

// newSessionYieldController binds the policy to the sessions this node opened.
// A node with no sources — every mode that owns none — gets a controller that
// decides nothing, so the caller never has to special-case observe mode.
func newSessionYieldController(state *sessionYieldState, sources []scaleSetSource, reporter *failureReporter) *sessionYieldController {
	if state == nil || len(sources) == 0 {
		return nil
	}
	yieldable := make([]yieldableSource, 0, len(sources))
	for _, source := range sources {
		if candidate, ok := source.(yieldableSource); ok {
			yieldable = append(yieldable, candidate)
		}
	}
	if len(yieldable) == 0 {
		return nil
	}
	controller := &sessionYieldController{state: state, sources: yieldable}
	if reporter != nil {
		controller.report = func(action sessionYieldAction, reason string, failures int) {
			reporter.reportSessionYield(action.String(), reason, failures)
		}
	}
	return controller
}
