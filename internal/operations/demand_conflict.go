package operations

import (
	"fmt"
	"log/slog"
)

// DemandConflictCode names a durable rejecting check. Values are defined here,
// never derived from message payloads or upstream errors.
type DemandConflictCode string

const (
	ConflictDemandJobIdentity         DemandConflictCode = "demand_job_identity"
	ConflictReservationDuplicate      DemandConflictCode = "reservation_duplicate"
	ConflictBindingShapeMismatch      DemandConflictCode = "binding_shape_mismatch"
	ConflictBindingMetadataInvalid    DemandConflictCode = "binding_metadata_invalid"
	ConflictBindingSourceCAS          DemandConflictCode = "binding_source_cas"
	ConflictBindingTargetCAS          DemandConflictCode = "binding_target_cas"
	ConflictBindingRebindCAS          DemandConflictCode = "binding_rebind_cas"
	ConflictInstanceStateTransition   DemandConflictCode = "instance_state_transition"
	ConflictTransitionInstanceCAS     DemandConflictCode = "transition_instance_cas"
	ConflictTransitionIdempotentState DemandConflictCode = "transition_idempotent_state"
)

// DemandConflict identifies the rejecting durable check without carrying a
// demand payload or an upstream error. It preserves ErrConflict identity.
// Message fields are attached by the projection boundary; state fields belong
// to the compare-and-set that refused the write.
type DemandConflict struct {
	Check       DemandConflictCode
	MessageType DemandEventKind
	RequestID   int64
	JobID       string
	Runner      string
	Instance    string
	Other       string
	From, To    State
}

func (e *DemandConflict) Unwrap() error { return ErrConflict }
func (e *DemandConflict) Error() string { return fmt.Sprintf("%s: %s", ErrConflict, e.LogValue()) }

// LogValue is the structured diagnostic shared by errors and session warnings. Identifiers
// are bounded and structured: no raw message, scheduling metadata or payload
// can be rendered through this type.
func (e *DemandConflict) LogValue() slog.Value {
	bounded := func(s string) string {
		if len(s) > 256 {
			return s[:256]
		}
		return s
	}
	return slog.GroupValue(slog.String("check", string(e.Check)), slog.String("messageType", bounded(string(e.MessageType))),
		slog.Int64("runnerRequestId", e.RequestID), slog.String("jobId", bounded(e.JobID)),
		slog.String("runner", bounded(e.Runner)), slog.String("instance", bounded(e.Instance)), slog.String("other", bounded(e.Other)),
		slog.String("from", bounded(string(e.From))), slog.String("to", bounded(string(e.To))))
}
