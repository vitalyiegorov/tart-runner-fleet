package sqlite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/lifecycle"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// ResumeCleanup atomically supersedes a dead stop/delete attempt with a fresh
// durable cleanup. The caller supplies fresh host, power and GitHub proof, and
// stopped reports whether that power proof showed the VM powered off; this
// transaction rechecks identity, ownership, state, absence of live work and the
// resumption bound. The instance is retained, so an external failure remains
// owned and retryable.
func (s *Store) ResumeCleanup(ctx context.Context, id, resource string, stopped bool, at time.Time) error {
	if id == "" || resource == "" || at.IsZero() {
		return operations.ErrInvalid
	}
	tx, err := s.beginTx(ctx, "cleanup_recovery.begin")
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	old, err := scanOperation(s.txRow(ctx, tx, "cleanup_recovery.operation", `SELECT id,idempotency_key,effect_key,kind,resource_id,payload,status,attempts,available_at,lease_owner,lease_until,last_error,created_at,updated_at FROM operations WHERE id=?`, id))
	if err != nil {
		return err
	}
	if old.ResourceID != resource {
		return operations.ErrResourceMismatch
	}
	if old.Status == operations.OperationDischarged {
		return nil
	}
	if old.Status != operations.OperationDead {
		return operations.ErrNotDeadLettered
	}
	code := lifecycle.FailureCode(old.LastError)
	if old.Kind != lifecycle.OperationDrain || (code != "stop" && code != "delete") {
		return operations.ErrInvalid
	}
	instance, err := scanInstance(s.txRow(ctx, tx, "cleanup_recovery.instance", `SELECT id,state,version,drain_phase,ownership,scheduling_metadata,last_error,created_at,updated_at FROM instances WHERE id=?`, resource))
	if err != nil {
		return err
	}
	if !instance.State.TearingDown() || !instance.Ownership.Valid() {
		return operations.ErrInstanceNotReapable
	}
	var progressing int
	if err := s.txRow(ctx, tx, "cleanup_recovery.progress", `SELECT COUNT(*) FROM operations WHERE resource_id=? AND status IN (?,?)`, resource, operations.OperationPending, operations.OperationClaimed).Scan(&progressing); err != nil {
		return err
	}
	if progressing != 0 {
		return operations.ErrResourceProgressing
	}
	// The bound is rechecked here, against the same rows the tick read, so two
	// racing ticks can never spend more resumptions than the rule allows.
	var resumed int
	if err := s.txRow(ctx, tx, "cleanup_recovery.resumptions", `SELECT `+resumptionsOf("dead")+` FROM operations dead WHERE dead.id=?`, operations.OperationDischarged, id).Scan(&resumed); err != nil {
		return err
	}
	if !operations.CleanupResumptionDue(resumed, old.UpdatedAt, at) {
		return operations.ErrResumptionNotDue
	}
	sum := sha256.Sum256([]byte("cleanup-recovery\x00" + old.ID))
	next := "op-" + hex.EncodeToString(sum[:12])
	stamp := at.UTC().UnixNano()
	// The failed attempt already passed its dependencies. Keep its effect key:
	// only the eventual whole-drain completion may acknowledge that effect.
	if _, err := s.txExec(ctx, tx, "cleanup_recovery.enqueue", `INSERT INTO operations(id,idempotency_key,effect_key,kind,resource_id,payload,status,attempts,available_at,lease_owner,lease_until,last_error,created_at,updated_at) VALUES(?,?,?,?,?,?,?,0,?,'',0,'',?,?)`, next, next, old.EffectKey, old.Kind, resource, []byte(old.Payload), operations.OperationPending, stamp, stamp, stamp); err != nil {
		return fmt.Errorf("enqueue cleanup recovery: %w", err)
	}
	// Pending successors must still wait for actual cleanup completion, rather
	// than for the terminal record this replacement supersedes. Already failed
	// successors are not resurrected or dispatched again.
	if _, err := s.txExec(ctx, tx, "cleanup_recovery.dependencies", `UPDATE operation_dependencies SET depends_on=? WHERE depends_on=? AND operation_id IN (SELECT id FROM operations WHERE status=?)`, next, id, operations.OperationPending); err != nil {
		return err
	}
	if _, err := s.txExec(ctx, tx, "cleanup_recovery.supersede", `UPDATE operations SET status=?,updated_at=? WHERE id=?`, operations.OperationDischarged, stamp, id); err != nil {
		return err
	}
	if stopped && instance.State == operations.StateDraining {
		// Restart through the stopped-recovery guard: a VM that came back aborts
		// to Running instead of inheriting an old occupancy/guest-death decision.
		// A running VM keeps its phase, whose own guard re-verifies its premise.
		if _, err := s.txExec(ctx, tx, "cleanup_recovery.phase", `UPDATE instances SET drain_phase=?,version=version+1,updated_at=? WHERE id=?`, operations.DrainPhaseStoppedRecovery, stamp, resource); err != nil {
			return err
		}
	}
	return s.commit(tx, "cleanup_recovery.commit")
}
