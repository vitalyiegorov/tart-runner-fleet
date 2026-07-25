package operations

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Discharge refusals. Each wraps ErrConflict so existing callers that only
// distinguish conflicts keep working, while the operator surface can name the
// exact guard that refused instead of printing one undifferentiated conflict.
var (
	// ErrResourceMismatch reports that the named operation does not belong to the
	// named instance. It is a typo guard: one operator mistake must never
	// discharge a different wedge.
	ErrResourceMismatch = fmt.Errorf("%w: operation does not own the named instance", ErrConflict)
	// ErrNotDeadLettered reports an operation that is pending, claimed, or already
	// completed. Only a dead letter may be discharged.
	ErrNotDeadLettered = fmt.Errorf("%w: operation is not dead-lettered", ErrConflict)
	// ErrResourceProgressing reports that another operation for the same resource
	// is pending or claimed, so the resource is not parked.
	ErrResourceProgressing = fmt.Errorf("%w: resource still has progressing operations", ErrConflict)
	// ErrInstanceNotReapable reports an instance row that is not in a cleanup or
	// terminal state, so retiring it would abandon a live runner.
	ErrInstanceNotReapable = fmt.Errorf("%w: instance state is not reapable", ErrConflict)
)

// DeadLetter names one durable operation that has stopped retrying and now waits
// for an operator. Code is the same closed lifecycle vocabulary the failure
// aggregate publishes — never stored upstream text — so a dead letter is safe to
// render through the operator API. It carries the identity the aggregate lacks:
// without an operation ID there is nothing an operator can discharge.
type DeadLetter struct {
	OperationID string
	Kind        string
	Code        string
	ResourceID  string
	Attempts    int
	// ResourceProgressing reports that some other operation for the same resource
	// is pending or claimed. While it is true the resource is not parked on this
	// dead letter: work can still advance without an operator.
	ResourceProgressing bool
}

// Discharge is an operator's authorized closure of one dead-lettered operation.
// It never forces an effect the fleet could have performed itself: it records
// that a human accepted responsibility for an operation the fleet proved it can
// never complete, so the parked resource stops blocking the host.
type Discharge struct {
	OperationID string
	InstanceID  string
	// ReapInstance additionally retires the operation's owning instance row. It is
	// separate because discharging the operation alone is the smaller action and
	// the only one needed when the instance already reached a terminal state.
	ReapInstance bool
	Reason       string
	At           time.Time
}

func (d Discharge) Valid() bool {
	return d.OperationID != "" && d.InstanceID != "" && strings.TrimSpace(d.Reason) != "" && !d.At.IsZero()
}

// DischargeOutcome reports what the durable transaction actually changed. Both
// flags are false on a repeat of an already-applied discharge, which makes the
// call idempotent: an operator may safely retry after a partial failure.
// Ownership is always the owning instance's durable ownership metadata, which the
// caller needs to prove a VM belongs to this controller before removing it.
type DischargeOutcome struct {
	OperationDischarged bool
	InstanceReaped      bool
	Ownership           Ownership
}

// DeadLetterStore is the durable port for observing and discharging dead
// letters. It is deliberately separate from Store: the read is aggregate
// telemetry and the write is an authorized operator mutation, and neither
// belongs in the interface every planner and executor depends on.
type DeadLetterStore interface {
	DeadLetters(context.Context) ([]DeadLetter, error)
	DischargeDeadLetter(context.Context, Discharge) (DischargeOutcome, error)
}
