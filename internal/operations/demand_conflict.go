package operations

import (
	"fmt"
	"log/slog"
)

// DemandConflict identifies the rejecting durable check without carrying a
// demand payload or an upstream error. It preserves ErrConflict identity.
// Message fields are attached by the projection boundary; state fields belong
// to the compare-and-set that refused the write.
type DemandConflict struct {
	Check       string
	MessageType DemandEventKind
	RequestID   int64
	JobID       string
	Runner      string
	Instance    string
	From, To    State
}

func (e *DemandConflict) Unwrap() error { return ErrConflict }
func (e *DemandConflict) Error() string { return fmt.Sprintf("%s: %s", ErrConflict, e.LogValue()) }

// LogValue is the allowlist shared by errors and session warnings. Identifiers
// are bounded and structured: no raw message, scheduling metadata or payload
// can be rendered through this type.
func (e *DemandConflict) LogValue() slog.Value {
	check := e.Check
	switch check {
	case "demand_job_identity", "reservation_duplicate", "binding_shape_mismatch", "binding_metadata_invalid",
		"binding_source_cas", "binding_target_cas", "binding_rebind_cas", "instance_state_transition",
		"transition_instance_cas", "transition_idempotent_state":
	default:
		check = "unknown"
	}
	bounded := func(s string) string {
		if len(s) > 256 {
			return s[:256]
		}
		return s
	}
	return slog.GroupValue(slog.String("check", check), slog.String("messageType", bounded(string(e.MessageType))),
		slog.Int64("runnerRequestId", e.RequestID), slog.String("jobId", bounded(e.JobID)),
		slog.String("runner", bounded(e.Runner)), slog.String("instance", bounded(e.Instance)),
		slog.String("from", bounded(string(e.From))), slog.String("to", bounded(string(e.To))))
}
