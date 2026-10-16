package sqlite

import (
	"context"
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
		ResumeStoppedCleanup(context.Context, string, string, time.Time) error
	})
	if !ok {
		t.Fatal("store has no durable cleanup recovery")
	}
	if err := recovery.ResumeStoppedCleanup(context.Background(), op, id, now); err != nil {
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
	if err := recovery.ResumeStoppedCleanup(context.Background(), op, id, now); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupRecoveryRollsBackEveryFailedDurableStep(t *testing.T) {
	for _, point := range []string{"cleanup_recovery.begin", "cleanup_recovery.operation", "cleanup_recovery.instance", "cleanup_recovery.progress", "cleanup_recovery.enqueue", "cleanup_recovery.dependencies", "cleanup_recovery.supersede", "cleanup_recovery.phase", "cleanup_recovery.commit"} {
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
			if err := store.ResumeStoppedCleanup(context.Background(), op, id, now); err == nil {
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
			if err := store.ResumeStoppedCleanup(context.Background(), op, id, now); (err == nil) != success {
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
	if err := store.ResumeStoppedCleanup(context.Background(), op, id, now); err != nil {
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
