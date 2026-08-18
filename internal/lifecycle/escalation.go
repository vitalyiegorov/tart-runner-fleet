package lifecycle

import "github.com/vitalyiegorov/tart-runner-fleet/internal/operations"

// stopsItsGuestFirst names the drain phases that power the guest off BEFORE
// deregistering it, because GitHub will not remove a runner it considers to be
// executing a job and both of these reclaims act on a job GitHub still believes
// is running. They are also the phases a `runner_busy` refusal must not abort:
// busy evidence is their premise rather than a refutation of it.
func stopsItsGuestFirst(phase int) bool {
	return phase == operations.DrainPhaseOccupancyBudget || phase == operations.DrainPhaseGuestUnresponsive
}

// derivesInactivityFromRunnerAbsence names the drain phases whose deletion
// confirmation reads runner absence rather than waiting for a job-completion
// event. Each of them ends, or has already lost, a job that will never publish
// one: the job never started, its completion already passed, or the machine
// executing it stopped answering.
func derivesInactivityFromRunnerAbsence(phase int) bool {
	switch phase {
	case operations.DrainPhaseStoppedRecovery, operations.DrainPhaseStalledAssignment,
		operations.DrainPhaseLingeringRunner, operations.DrainPhaseOccupancyBudget,
		operations.DrainPhaseGuestUnresponsive:
		return true
	default:
		return false
	}
}

// StopForce is how hard one already-decided drain asks its guest to stop.
//
// It exists because repetition is not escalation. On 2026-08-10 a drain whose
// job had finished in two minutes ran the identical graceful stop 67 times over
// 90 minutes, holding a whole node's 6 CPU / 12288 MiB while twelve jobs queued
// behind it. Every attempt was the same request to the same wedged guest, so
// every attempt failed the same way. A stop that has failed three times will not
// succeed on the sixty-eighth by being asked again; it will succeed by being
// asked harder (ADR 0039).
type StopForce int

const (
	// StopGraceful asks the guest to power itself down and waits, under the
	// backend's own deadline.
	StopGraceful StopForce = iota
	// StopForced powers the guest off without waiting for it to agree.
	StopForced
	// StopDestructive powers the guest off and removes it in one step, on exactly
	// the evidence an ordinary delete requires.
	StopDestructive
	// StopExhausted is the end of the ladder: every rung has been tried and every
	// rung has failed, so no further attempt can differ from the last one. The
	// drain dead-letters at once instead of retrying invisibly, which is what
	// makes `fleet operations discharge` reachable in the situation it exists for.
	StopExhausted
)

// String renders the rung for a durable-safe diagnostic. It is a closed
// vocabulary, never derived from anything a child process said.
func (f StopForce) String() string {
	switch f {
	case StopGraceful:
		return "graceful"
	case StopForced:
		return "forced"
	case StopDestructive:
		return "destructive"
	default:
		return "exhausted"
	}
}

// The rungs are three attempts each, and the numbers are chosen against the
// incident's own arithmetic rather than picked round. One failed attempt costs
// the backend's command deadline (45s on this fleet) plus the drain retry
// ceiling (30s), so a rung is roughly four minutes: the fleet stops being polite
// at ~4 minutes, removes the guest at ~8, and publishes a dischargeable dead
// letter at ~12. The incident ran 90 minutes and was still climbing.
const (
	// GracefulStopAttempts is how many times a drain asks the guest nicely.
	GracefulStopAttempts = 3
	// ForcedStopAttempts is how many times it then powers the guest off without
	// asking, before it removes the guest outright.
	ForcedStopAttempts = 3
	// DestructiveStopAttempts is how many removals it attempts before declaring
	// the ladder exhausted.
	DestructiveStopAttempts = 3
)

// StopEscalation chooses the rung for the next stop from how many attempts this
// drain has already spent failing AT THE STOP STEP.
func StopEscalation(stopAttempts int) StopForce {
	switch {
	case stopAttempts < GracefulStopAttempts:
		return StopGraceful
	case stopAttempts < GracefulStopAttempts+ForcedStopAttempts:
		return StopForced
	case stopAttempts < GracefulStopAttempts+ForcedStopAttempts+DestructiveStopAttempts:
		return StopDestructive
	default:
		return StopExhausted
	}
}

// StopAttempts reports how many attempts a durable drain operation has spent
// failing at the stop step, from the two durable facts the operation row keeps:
// its attempt count and the closed code of its last failure.
//
// The last-failure gate is what keeps the ladder honest. A drain that has spent
// forty attempts being refused by GitHub at the deregister step has not yet
// asked its guest to stop even once, and must not open at a forceful rung on the
// strength of failures that happened somewhere else.
func StopAttempts(operation operations.Operation) int {
	if FailureCode(operation.LastError) != string(StageStop) {
		return 0
	}
	return operation.Attempts
}
