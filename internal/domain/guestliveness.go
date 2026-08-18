package domain

import "time"

// GuestLiveness is what one probe of a running guest established, and it is
// deliberately three-valued.
//
// On 2026-08-16 a `--privileged` container wrote `c` to the guest's
// `/proc/sysrq-trigger` and panicked the guest kernel. With `kernel.panic=0` the
// panicked kernel hung forever: no userspace ran again, the runner agent's TCP
// connection was never closed, `tart list` went on reporting the VM `running`,
// and the fleet held 6 CPU / 12288 MiB until GitHub's own grace timer failed the
// job sixteen to eighteen minutes later. The one host-side signal that changed
// within seconds was that `tart exec` could no longer reach the guest agent at
// all.
//
// The reason there are three values rather than a boolean is the entire safety
// argument of this mechanism. A guest under legitimate heavy load — a monorepo
// Gradle build using every core — is slow, and a slow guest that answers, or a
// probe that runs out of its own deadline before the guest can answer, is not
// evidence of anything. Only a refusal of the transport itself, which is what a
// panicked kernel produces immediately and repeatedly, counts against a guest.
// Collapsing "could not tell" into "dead" is precisely how this class of
// mechanism kills healthy jobs.
type GuestLiveness string

const (
	// GuestLivenessUnknown is the absence of a measurement: the probe did not
	// run, or it ran and could not establish anything before its own deadline. It
	// never counts for or against a guest.
	GuestLivenessUnknown GuestLiveness = ""
	// GuestLivenessAlive means the guest executed a trivial command. However long
	// it took inside the probe's deadline, something in that guest was scheduled.
	GuestLivenessAlive GuestLiveness = "alive"
	// GuestLivenessRefused means the probe could not reach the guest at all: the
	// transport was refused rather than slow. This is the panicked-kernel
	// signature, and it is the only observation that accumulates.
	GuestLivenessRefused GuestLiveness = "refused"
)

// GuestLivenessPolicy is how much refusal, over how long, the fleet requires
// before it will call a guest dead. Both bounds must be met, and they bound
// different mistakes: the count bounds a momentary refusal (a guest agent
// restarting, a control socket replaced), and the window bounds a control loop
// that happens to tick quickly, so a fast tick can never convert seconds of
// silence into a verdict.
//
// A zero policy is disabled and can never declare anything dead. A destructive
// bound is never inferred from a configuration that does not state one.
type GuestLivenessPolicy struct {
	// ConsecutiveRefusals is how many probes in an unbroken run must be refused.
	ConsecutiveRefusals int
	// Window is how long that unbroken run must have lasted.
	Window time.Duration
}

// Enabled reports whether the policy states a bound at all.
func (p GuestLivenessPolicy) Enabled() bool {
	return p.ConsecutiveRefusals > 0 && p.Window > 0
}

// GuestLivenessState is the per-instance accumulator the caller carries between
// probes, so the policy itself stays pure. Every field is a fact about probes,
// never a verdict: the verdict is recomputed from the policy each time it is
// asked for, so changing the bound changes every judgement at once.
type GuestLivenessState struct {
	// Refusals is the length of the current unbroken run of refusals.
	Refusals int
	// RefusedSince is when that run began. Zero when there is no run.
	RefusedSince time.Time
	// LastAlive is the last instant the guest executed the probe. Zero when it
	// never has, which is not itself evidence of death — a guest that has only
	// ever been probed inconclusively has told the fleet nothing.
	LastAlive time.Time
	// LastProbe is the last instant a probe of any outcome completed.
	LastProbe time.Time
}

// Observe folds one probe outcome into the accumulator.
//
// An unknown outcome CLEARS the run rather than extending or preserving it. That
// is the fail-open choice and it is deliberate: an inconclusive probe is not a
// hard failure, and a mechanism that can end a running CI job must not be able
// to reach its threshold through observations that established nothing. The cost
// is that a guest whose probe is permanently inconclusive is never declared
// dead; the occupancy budget (ADR 0036) remains the backstop for that instance,
// exactly as it was before this mechanism existed.
func (p GuestLivenessPolicy) Observe(state GuestLivenessState, liveness GuestLiveness, now time.Time) GuestLivenessState {
	next := state
	next.LastProbe = now
	switch liveness {
	case GuestLivenessRefused:
		next.Refusals = state.Refusals + 1
		if next.RefusedSince.IsZero() {
			next.RefusedSince = now
		}
	case GuestLivenessAlive:
		next.Refusals, next.RefusedSince, next.LastAlive = 0, time.Time{}, now
	default:
		next.Refusals, next.RefusedSince = 0, time.Time{}
	}
	return next
}

// Silence reports how long the current unbroken run of refusals has lasted, and
// whether that is a measurable fact at all. It is false — never a zero duration
// passed off as a measurement — when there is no run, or when the run began
// after the instant it is measured against.
func (s GuestLivenessState) Silence(now time.Time) (time.Duration, bool) {
	if s.RefusedSince.IsZero() || now.Before(s.RefusedSince) {
		return 0, false
	}
	return now.Sub(s.RefusedSince), true
}

// Confirmed reports whether the accumulated hostile observations satisfy both
// bounds — for a guest probe, whether the guest is dead. It is fail-closed on
// every input: a policy that states no bound, a run that was never recorded, and
// a start instant in the future all answer false.
//
// It is named for the general question rather than for guests because a second
// observation reuses this bound unchanged (see corroboration.go).
func (p GuestLivenessPolicy) Confirmed(state GuestLivenessState, now time.Time) bool {
	if !p.Enabled() || state.Refusals < p.ConsecutiveRefusals {
		return false
	}
	silence, measured := state.Silence(now)
	return measured && silence >= p.Window
}
