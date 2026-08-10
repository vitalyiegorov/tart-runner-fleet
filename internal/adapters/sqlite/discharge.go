package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/lifecycle"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// DeadLetters names the individual operations that have stopped retrying, so an
// operator can discharge one by identity. The failure aggregate deliberately
// publishes only counts; identity is what an actionable remedy needs. Persisted
// failure text never leaves the store: each message is reduced to one closed
// lifecycle code exactly as the aggregate does.
func (s *Store) DeadLetters(ctx context.Context) ([]operations.DeadLetter, error) {
	rows, err := s.dbQuery(ctx, "operations.deadletters.query", `SELECT dead.id,dead.kind,dead.resource_id,dead.last_error,dead.attempts,
		EXISTS(SELECT 1 FROM operations live WHERE live.resource_id=dead.resource_id AND live.status IN (?,?))
		FROM operations dead WHERE dead.status=? ORDER BY dead.id`,
		operations.OperationPending, operations.OperationClaimed, operations.OperationDead)
	if err != nil {
		return nil, fmt.Errorf("list dead letters: %w", err)
	}
	defer func() { _ = rows.Close() }()
	letters := make([]operations.DeadLetter, 0)
	for rows.Next() {
		var letter operations.DeadLetter
		var lastError string
		if err := rows.Scan(&letter.OperationID, &letter.Kind, &letter.ResourceID, &lastError,
			&letter.Attempts, &letter.ResourceProgressing); err != nil {
			return nil, fmt.Errorf("scan dead letter: %w", err)
		}
		letter.Code = lifecycle.FailureCode(lastError)
		letters = append(letters, letter)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate dead letters: %w", err)
	}
	return letters, nil
}

// cleanupStates are the durable instance states that mean "this row is being
// torn down". An instance that has been in one of them for a long time is
// holding its vector for no reason anybody can see, which is the 2026-08-10
// incident: `deregistering` for 4939 seconds while 67 identical stop attempts
// failed.
var cleanupStates = []operations.State{operations.StateDraining, operations.StateDeregistering,
	operations.StateStopping}

// StalledOperations reports the operations that are still retrying and the
// instances still held in a cleanup state, with the two facts joined so a caller
// can say "operation X has failed N times at step S, and instance I has been
// held in state T for D" in one sentence.
//
// The second half of the union is not redundant: an instance whose drain has
// already dead-lettered has no retrying operation at all, and is precisely the
// row an operator most needs named. Persisted failure text never leaves the
// store; each message is reduced to one closed lifecycle code exactly as the
// aggregate and the dead-letter set do.
func (s *Store) StalledOperations(ctx context.Context, now time.Time) ([]operations.StalledOperation, error) {
	rows, err := s.dbQuery(ctx, "operations.stalled.query", `SELECT operation.id,operation.kind,operation.last_error,
			operation.attempts,operation.created_at,operation.resource_id,
			COALESCE(instance.state,''),COALESCE(instance.updated_at,0)
		FROM operations operation
		LEFT JOIN instances instance ON instance.id=operation.resource_id AND instance.state IN (?,?,?)
		WHERE operation.status IN (?,?) AND operation.attempts>0
		UNION ALL
		SELECT '','','',0,0,held.id,held.state,held.updated_at
		FROM instances held
		WHERE held.state IN (?,?,?) AND NOT EXISTS (
			SELECT 1 FROM operations progressing WHERE progressing.resource_id=held.id
			AND progressing.status IN (?,?) AND progressing.attempts>0)
		ORDER BY 6,1`,
		cleanupStates[0], cleanupStates[1], cleanupStates[2],
		operations.OperationPending, operations.OperationClaimed,
		cleanupStates[0], cleanupStates[1], cleanupStates[2],
		operations.OperationPending, operations.OperationClaimed)
	if err != nil {
		return nil, fmt.Errorf("list stalled operations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	stalled := make([]operations.StalledOperation, 0)
	for rows.Next() {
		var row operations.StalledOperation
		var lastError string
		var createdAt, updatedAt int64
		if err := rows.Scan(&row.OperationID, &row.Kind, &lastError, &row.Attempts, &createdAt,
			&row.Instance, &row.DrainState, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan stalled operation: %w", err)
		}
		if row.OperationID != "" {
			row.Code = lifecycle.FailureCode(lastError)
			row.Retrying = since(now, createdAt)
		}
		if row.DrainState != "" {
			row.Held = since(now, updatedAt)
		}
		stalled = append(stalled, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate stalled operations: %w", err)
	}
	return stalled, nil
}

// since is the non-negative age of a durable nanosecond timestamp. A zero or
// future timestamp yields zero rather than a negative or enormous duration: an
// unreadable age must never be published as an alarming one.
func since(now time.Time, at int64) time.Duration {
	if at <= 0 {
		return 0
	}
	age := now.Sub(fromNanos(at))
	if age < 0 {
		return 0
	}
	return age
}

// reapableStates are the only instance states an operator discharge may retire.
// A planned, cloning, booting, reachable, registering, idle, assigned, or running
// instance is deliberately excluded: retiring its row would abandon a runner
// GitHub may still hand a job to, and a fresh "VM is not running" observation is
// not sufficient authority for that. The list is applied as the UPDATE predicate
// rather than a prior check, so the durable write itself is the guard.
var reapableStates = []operations.State{operations.StateDraining, operations.StateDeregistering,
	operations.StateStopping, operations.StateFailed}

// DischargeDeadLetter closes one dead-lettered operation, and optionally retires
// its owning instance row, in a single transaction.
//
// The safety rules are all here rather than in the caller, so no future surface
// can reach a weaker version of them:
//
//   - the operation must exist, and its resource must be exactly the instance the
//     operator named — a typo can never discharge somebody else's wedge;
//   - only a dead operation may be discharged. A pending or claimed operation is
//     still making progress and a completed one already succeeded;
//   - no other operation for the same resource may be pending or claimed. Parked
//     means nothing can advance without an operator, and this is that check;
//   - the instance row is retired only from a cleanup or terminal state.
//
// Re-applying an already-applied discharge reports no changes instead of failing,
// which is what lets an operator retry safely after the VM removal that follows
// the transaction fails. Ownership is always returned so that retry can still
// prove the VM belongs to this controller.
func (s *Store) DischargeDeadLetter(ctx context.Context, discharge operations.Discharge) (operations.DischargeOutcome, error) {
	if !discharge.Valid() {
		return operations.DischargeOutcome{}, operations.ErrInvalid
	}
	tx, err := s.beginTx(ctx, "discharge.begin")
	if err != nil {
		return operations.DischargeOutcome{}, fmt.Errorf("begin discharge: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var status, resource string
	err = s.txRow(ctx, tx, "discharge.operation.load", `SELECT status,resource_id FROM operations WHERE id=?`,
		discharge.OperationID).Scan(&status, &resource)
	if errors.Is(err, sql.ErrNoRows) {
		return operations.DischargeOutcome{}, operations.ErrNotFound
	}
	if err != nil {
		return operations.DischargeOutcome{}, fmt.Errorf("load dead letter: %w", err)
	}
	if resource != discharge.InstanceID {
		return operations.DischargeOutcome{}, operations.ErrResourceMismatch
	}
	switch operations.OperationStatus(status) {
	case operations.OperationDead, operations.OperationDischarged:
		// Dead is the discharge case; discharged is the idempotent repeat.
	default:
		return operations.DischargeOutcome{}, operations.ErrNotDeadLettered
	}
	var progressing int
	if err := s.txRow(ctx, tx, "discharge.progress.check", `SELECT COUNT(*) FROM operations WHERE resource_id=? AND status IN (?,?)`,
		resource, operations.OperationPending, operations.OperationClaimed).Scan(&progressing); err != nil {
		return operations.DischargeOutcome{}, fmt.Errorf("check resource progress: %w", err)
	}
	if progressing != 0 {
		return operations.DischargeOutcome{}, operations.ErrResourceProgressing
	}
	outcome := operations.DischargeOutcome{}
	if operations.OperationStatus(status) == operations.OperationDead {
		if _, err := s.txExec(ctx, tx, "discharge.operation.update", `UPDATE operations
			SET status=?,lease_owner='',lease_until=0,updated_at=? WHERE id=? AND status=?`,
			operations.OperationDischarged, discharge.At.UTC().UnixNano(), discharge.OperationID,
			operations.OperationDead); err != nil {
			return operations.DischargeOutcome{}, fmt.Errorf("discharge dead letter: %w", err)
		}
		outcome.OperationDischarged = true
	}
	instance, err := scanInstance(s.txRow(ctx, tx, "discharge.instance.load", `SELECT id,state,version,drain_phase,ownership,scheduling_metadata,last_error,created_at,updated_at FROM instances WHERE id=?`, discharge.InstanceID))
	if err != nil {
		return operations.DischargeOutcome{}, err
	}
	outcome.Ownership = instance.Ownership
	if discharge.ReapInstance && instance.State != operations.StateDeleted {
		if err := s.reapInstance(ctx, tx, instance, discharge.At.UTC().UnixNano()); err != nil {
			return operations.DischargeOutcome{}, err
		}
		outcome.InstanceReaped = true
	}
	if err := s.commit(tx, "discharge.commit"); err != nil {
		return operations.DischargeOutcome{}, fmt.Errorf("commit discharge: %w", err)
	}
	return outcome, nil
}

// reapInstance retires one instance row to Deleted and records the transition.
//
// It deliberately does not consult domain.InstanceState.CanTransitionTo: that
// machine describes what the fleet may do on its own, and it has no edge from a
// cleanup state to Deleted precisely because autonomous code must always walk
// deregister, stop, delete. An operator discharge is the authorized exception,
// and it is recorded in transition_history like every other transition so the
// jump is auditable rather than invisible.
func (s *Store) reapInstance(ctx context.Context, tx *sql.Tx, instance operations.Instance, at int64) error {
	result, err := s.txExec(ctx, tx, "discharge.instance.update", `UPDATE instances
		SET state=?,version=version+1,updated_at=? WHERE id=? AND version=? AND state IN (?,?,?,?)`,
		operations.StateDeleted, at, instance.ID, instance.Version, reapableStates[0], reapableStates[1],
		reapableStates[2], reapableStates[3])
	if err != nil {
		return fmt.Errorf("reap instance: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		return operations.ErrInstanceNotReapable
	}
	if _, err := s.txExec(ctx, tx, "discharge.instance.history", `INSERT INTO transition_history(instance_id,from_state,to_state,version,operation_id,created_at) VALUES(?,?,?,?,NULL,?)`,
		instance.ID, instance.State, operations.StateDeleted, instance.Version+1, at); err != nil {
		return fmt.Errorf("record reap: %w", err)
	}
	return nil
}
