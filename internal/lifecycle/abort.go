package lifecycle

import "github.com/vitalyiegorov/tart-runner-fleet/internal/operations"

// AbortEvidence names the fresh observation that disproved a drain's premise.
// It is a closed vocabulary, never derived from anything GitHub, the backend, or
// a guest said, so a report built from it cannot carry a payload or a secret.
type AbortEvidence string

const (
	// AbortEvidenceVMRunning is a stopped recovery whose VM reads powered on.
	AbortEvidenceVMRunning AbortEvidence = "vm_running"
	// AbortEvidenceRunnerRegistered is an inactive recovery whose runner is still
	// registered on GitHub.
	AbortEvidenceRunnerRegistered AbortEvidence = "runner_registered"
	// AbortEvidenceRunnerBusy is durable demand evidence that a workflow job is
	// executing on this instance's runner, whichever demand GitHub brokered to it.
	AbortEvidenceRunnerBusy AbortEvidence = "runner_busy"
	// AbortEvidenceGuestAnswered is a guest-liveness reclaim whose guest answered
	// the fresh probe.
	AbortEvidenceGuestAnswered AbortEvidence = "guest_answered"
	// AbortEvidenceDeregisterRefused is GitHub refusing the deregistration,
	// confirmed by fresh runner-busy evidence.
	AbortEvidenceDeregisterRefused AbortEvidence = "deregister_refused_runner_busy"
)

// AbortOutcome is the durable result an aborted drain operation is completed
// with: a closed code naming the evidence, never upstream text, so an operator
// reading the operation can tell an abort from a teardown.
func AbortOutcome(evidence AbortEvidence) string {
	return "aborted:" + string(evidence)
}

// DrainAbort is one drain the executor cancelled, returning its instance to
// Running. It carries the instance row (identity, profile, repository, demand
// key — never JIT configuration or an operation payload), the phase that planned
// the drain, and the evidence that disproved it.
type DrainAbort struct {
	Instance operations.Instance
	Phase    string
	Evidence AbortEvidence
}

// DrainPhaseName renders a durable drain phase for a diagnostic. Unknown phases
// say so rather than borrowing a neighbour's name.
func DrainPhaseName(phase int) string {
	switch phase {
	case operations.DrainPhaseEvent:
		return "event"
	case operations.DrainPhaseStoppedRecovery:
		return "stopped_recovery"
	case operations.DrainPhaseInactiveRecovery:
		return "inactive_recovery"
	case operations.DrainPhaseStalledAssignment:
		return "stalled_assignment"
	case operations.DrainPhaseLingeringRunner:
		return "lingering_runner"
	case operations.DrainPhaseOccupancyBudget:
		return "occupancy_budget"
	case operations.DrainPhaseGuestUnresponsive:
		return "guest_unresponsive"
	case operations.DrainPhaseProvisionStalled:
		return "provision_stalled"
	default:
		return "unknown"
	}
}
