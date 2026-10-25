package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

func TestParkedStopRecoveryIsDurableAndDoesNotRetireTheInstance(t *testing.T) {
	store := testStore(t)
	now := time.Unix(1000, 0).UTC()
	id, op := phantom(t, store, now)
	if _, err := store.db.Exec(`UPDATE operations SET last_error='runner lifecycle failed at stop' WHERE id=?`, op); err != nil {
		t.Fatal(err)
	}
	recovery, ok := any(store).(interface {
		ResumeCleanup(context.Context, string, string, bool, time.Time) error
	})
	if !ok {
		t.Fatal("store has no durable cleanup recovery")
	}
	if err := recovery.ResumeCleanup(context.Background(), op, id, true, now); err != nil {
		t.Fatal(err)
	}
	if got := instanceState(t, store, id); got != operations.StateDraining {
		t.Fatalf("observer prematurely retired instance: %s", got)
	}
	if got := operationStatus(t, store, op); got != string(operations.OperationDischarged) {
		t.Fatalf("old operation=%s", got)
	}
	claimed, err := store.Claim(context.Background(), "worker", 10, now, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claimed=%v err=%v", claimed, err)
	}
	if claimed[0].Attempts != 0 || claimed[0].ResourceID != id {
		t.Fatalf("replacement=%v", claimed[0])
	}
	if err := recovery.ResumeCleanup(context.Background(), op, id, true, now); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupRecoveryRollsBackEveryFailedDurableStep(t *testing.T) {
	for _, point := range []string{"cleanup_recovery.begin", "cleanup_recovery.operation", "cleanup_recovery.instance", "cleanup_recovery.progress", "cleanup_recovery.resumptions", "cleanup_recovery.enqueue", "cleanup_recovery.dependencies", "cleanup_recovery.supersede", "cleanup_recovery.phase", "cleanup_recovery.commit"} {
		t.Run(point, func(t *testing.T) {
			store := testStore(t)
			now := time.Unix(1000, 0).UTC()
			id, op := phantom(t, store, now)
			if _, err := store.db.Exec(`UPDATE operations SET last_error='runner lifecycle failed at stop' WHERE id=?`, op); err != nil {
				t.Fatal(err)
			}
			store.injectFault = func(p string) error {
				if p == point {
					return operations.ErrUncertain
				}
				return nil
			}
			if err := store.ResumeCleanup(context.Background(), op, id, true, now); err == nil {
				t.Fatal("store fault ignored")
			}
			store.injectFault = nil
			if operationStatus(t, store, op) != string(operations.OperationDead) {
				t.Fatal("failed transaction lost old retry record")
			}
		})
	}
}

func TestCleanupRecoveryRefusesUnrelatedLiveOrUnownedWork(t *testing.T) {
	for _, name := range []string{"empty", "wrong resource", "missing", "live", "wrong kind", "wrong failure", "running", "progressing", "delete", "stopping"} {
		t.Run(name, func(t *testing.T) {
			store := testStore(t)
			now := time.Unix(1000, 0).UTC()
			id, op := phantom(t, store, now)
			if _, err := store.db.Exec(`UPDATE operations SET last_error='runner lifecycle failed at stop' WHERE id=?`, op); err != nil {
				t.Fatal(err)
			}
			success := false
			switch name {
			case "empty":
				op = ""
			case "wrong resource":
				id = "other"
			case "missing":
				op = "missing"
			case "live":
				_, _ = store.db.Exec(`UPDATE operations SET status='pending' WHERE id=?`, op)
			case "wrong kind":
				_, _ = store.db.Exec(`UPDATE operations SET kind='clone' WHERE id=?`, op)
			case "wrong failure":
				_, _ = store.db.Exec(`UPDATE operations SET last_error='runner lifecycle failed at drain_guard' WHERE id=?`, op)
			case "running":
				_, _ = store.db.Exec(`UPDATE instances SET state='running' WHERE id=?`, id)
			case "progressing":
				insertOperationRow(t, store, now, "other", "deregister", id, operations.OperationPending, 0, "")
			case "delete":
				_, _ = store.db.Exec(`UPDATE operations SET last_error='runner lifecycle failed at delete' WHERE id=?`, op)
				success = true
			case "stopping":
				_, _ = store.db.Exec(`UPDATE instances SET state='stopping' WHERE id=?`, id)
				success = true
			}
			if err := store.ResumeCleanup(context.Background(), op, id, true, now); (err == nil) != success {
				t.Fatalf("error=%v success=%v", err, success)
			}
		})
	}
}

func TestRecoveredCleanupKeepsSuccessorsBehindItsRealCompletion(t *testing.T) {
	store := testStore(t)
	now := time.Unix(1000, 0).UTC()
	id, op := phantom(t, store, now)
	_, err := store.db.Exec(`UPDATE operations SET last_error='runner lifecycle failed at stop' WHERE id=?`, op)
	if err != nil {
		t.Fatal(err)
	}
	insertOperationRow(t, store, now, "successor", "clone", "trf-other", operations.OperationPending, 0, "")
	if _, err := store.db.Exec(`INSERT INTO operation_dependencies(operation_id,depends_on) VALUES('successor',?)`, op); err != nil {
		t.Fatal(err)
	}
	if err := store.ResumeCleanup(context.Background(), op, id, true, now); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.Claim(context.Background(), "worker", 10, now, time.Minute)
	if err != nil || len(claimed) != 1 || claimed[0].ID == "successor" {
		t.Fatalf("premature successor: %v %v", claimed, err)
	}
	if _, err := store.Complete(context.Background(), claimed[0].ID, "worker", claimed[0].EffectKey, now); err != nil {
		t.Fatal(err)
	}
	claimed, err = store.Claim(context.Background(), "worker", 10, now, time.Minute)
	if err != nil || len(claimed) != 1 || claimed[0].ID != "successor" {
		t.Fatalf("successor stayed blocked: %v %v", claimed, err)
	}
}

// A running VM's successor keeps its drain phase: the stopped-recovery guard
// would abort a VM that is running by design back to Running (ADR 0081).
func TestRunningCleanupRecoveryKeepsTheDrainPhase(t *testing.T) {
	store := testStore(t)
	now := time.Unix(1000, 0).UTC()
	id, op := phantom(t, store, now)
	if _, err := store.db.Exec(`UPDATE operations SET last_error='runner lifecycle failed at stop' WHERE id=?`, op); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE instances SET drain_phase=? WHERE id=?`, operations.DrainPhaseGuestUnresponsive, id); err != nil {
		t.Fatal(err)
	}
	if err := store.ResumeCleanup(context.Background(), op, id, false, now); err != nil {
		t.Fatal(err)
	}
	instance, err := store.Instance(context.Background(), id)
	if err != nil || instance.DrainPhase != operations.DrainPhaseGuestUnresponsive {
		t.Fatalf("instance=%#v err=%v", instance, err)
	}
}

// The transaction itself enforces the resumption bound, so two racing ticks
// can never spend more than the rule allows, and the dead-letter projection
// reports the same count the transaction decides on.
func TestCleanupRecoveryIsBoundedInsideTheTransaction(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0).UTC()
	id, op := phantom(t, store, now)
	if _, err := store.db.Exec(`UPDATE operations SET last_error='runner lifecycle failed at stop' WHERE id=?`, op); err != nil {
		t.Fatal(err)
	}
	for resumed := range operations.MaxCleanupResumptions + 1 {
		letters, err := store.DeadLetters(ctx)
		if err != nil || len(letters) != 1 || letters[0].Resumptions != resumed || !letters[0].DeadAt.Equal(now) {
			t.Fatalf("resumption %d: letters=%#v err=%v", resumed, letters, err)
		}
		op = letters[0].OperationID
		if resumed > 0 {
			if err := store.ResumeCleanup(ctx, op, id, true, now); !errors.Is(err, operations.ErrResumptionNotDue) {
				t.Fatalf("resumption %d skipped its backoff: %v", resumed, err)
			}
		}
		now = now.Add(time.Duration(1<<resumed) * operations.CleanupResumptionBackoff)
		err = store.ResumeCleanup(ctx, op, id, true, now)
		if resumed == operations.MaxCleanupResumptions {
			if !errors.Is(err, operations.ErrResumptionNotDue) {
				t.Fatalf("resumed past the bound: %v", err)
			}
			return
		}
		if err != nil {
			t.Fatalf("resumption %d: %v", resumed, err)
		}
		claimed, err := store.Claim(ctx, "worker", 1, now, time.Minute)
		if err != nil || len(claimed) != 1 {
			t.Fatalf("claimed=%v err=%v", claimed, err)
		}
		if _, err := store.db.Exec(`UPDATE operations SET status='dead',lease_owner='',last_error='runner lifecycle failed at stop',updated_at=? WHERE id=?`, now.UnixNano(), claimed[0].ID); err != nil {
			t.Fatal(err)
		}
	}
}
