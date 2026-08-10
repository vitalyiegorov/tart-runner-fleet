package operations

import (
	"context"
	"time"
)

// StalledOperation is one durable operation that is still retrying, or one
// instance still held in a cleanup state, or — in the shape that matters — both
// at once.
//
// It exists because on 2026-08-10 nothing named the wedge. `fleet doctor`
// reported `queue_incident,queue_slo_breached`, which is the symptom, and
// `PASS occupancy` and `PASS reservation`, which are true and irrelevant.
// Finding the cause took an SSH session, a hand-copied SQLite file, and a read
// of the operations table. Everything below was already in that table; none of
// it was published.
//
// Code is the same closed lifecycle vocabulary the failure aggregate and the
// dead-letter set publish, never stored upstream text, so it is safe to render
// through the operator API. DrainState is a durable instance state and equally
// closed.
type StalledOperation struct {
	// OperationID and Kind are empty for an instance held in a cleanup state with
	// no operation still retrying on it — a drain that has already dead-lettered,
	// which is exactly when the instance is most stuck and least explained.
	OperationID string
	Kind        string
	Code        string
	// Instance is the resource the operation names, which for every lifecycle
	// operation is the instance holding the vector.
	Instance string
	Attempts int
	// Retrying is how long the operation has existed, which for an operation that
	// has never completed is how long it has been failing.
	Retrying time.Duration
	// DrainState is the cleanup state the instance is in (draining,
	// deregistering, stopping), or empty when it is not tearing down. Held is how
	// long it has been in that state.
	DrainState string
	Held       time.Duration
}

// StalledOperationStore is the durable port for the read above. It is separate
// from Store for the same reason DeadLetterStore is: this is aggregate
// telemetry, not something a planner or an executor may reach.
type StalledOperationStore interface {
	StalledOperations(context.Context, time.Time) ([]StalledOperation, error)
}
