package sqlite

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/lifecycle"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// phantom replays the durable shape of the 2026-07-25 incident: one instance
// stopped in the second drain phase, and one deregister operation dead-lettered
// against a GitHub registration that answers HTTP 422 "currently running a job"
// forever. It returns the instance ID and the dead letter's operation ID.
func phantom(t *testing.T, store *Store, now time.Time) (string, string) {
	t.Helper()
	ctx := context.Background()
	ownership := operations.Ownership{ControllerID: "tart-runner-fleet", ResourceID: "trf-maestro-096ffcb3a52d8624", OperationID: "op-provision"}
	instance := operations.Instance{ID: "trf-maestro-096ffcb3a52d8624", State: operations.StateDraining,
		Ownership: ownership, CreatedAt: now}
	if err := store.CreateInstance(ctx, instance); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE instances SET state=?,drain_phase=2 WHERE id=?`,
		operations.StateDraining, instance.ID); err != nil {
		t.Fatal(err)
	}
	insertOperationRow(t, store, now, "op-ea9b705d234ad29f14e79b6d", lifecycle.OperationDrain, instance.ID,
		operations.OperationDead, 835, "runner lifecycle failed at deregister (runner_busy)")
	return instance.ID, "op-ea9b705d234ad29f14e79b6d"
}

func insertOperationRow(t *testing.T, store *Store, now time.Time, id, kind, resource string,
	status operations.OperationStatus, attempts int, lastError string) {
	t.Helper()
	if _, err := store.db.ExecContext(context.Background(), `INSERT INTO operations(
		id,idempotency_key,effect_key,kind,resource_id,payload,status,attempts,available_at,
		lease_owner,lease_until,last_error,created_at,updated_at)
		VALUES(?,?,?,?,?,'{}',?,?,?,'',0,?,?,?)`, id, id, kind+":"+resource+":"+id, kind, resource, status,
		attempts, now.UnixNano(), lastError, now.UnixNano(), now.UnixNano()); err != nil {
		t.Fatal(err)
	}
}

func instanceState(t *testing.T, store *Store, id string) operations.State {
	t.Helper()
	instance, err := store.Instance(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return instance.State
}

func operationStatus(t *testing.T, store *Store, id string) string {
	t.Helper()
	var status string
	if err := store.db.QueryRowContext(context.Background(), `SELECT status FROM operations WHERE id=?`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

// A dead letter must be nameable. The failure aggregate publishes counts only,
// and AGENTS.md forbids opening fleet.db while the daemon runs, so without an
// operation identity there is nothing an operator can act on at all.
func TestDeadLettersNameParkedOperationsWithoutStoredText(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0).UTC()

	letters, err := store.DeadLetters(ctx)
	if err != nil || len(letters) != 0 {
		t.Fatalf("healthy fleet dead letters=%#v err=%v", letters, err)
	}

	instanceID, operationID := phantom(t, store, now)
	// A second resource still has a pending operation, so its dead letter is not
	// parked: work can advance without an operator.
	insertOperationRow(t, store, now, "op-dead-2", lifecycle.OperationDrain, "trf-linux-2",
		operations.OperationDead, 720, "runner lifecycle failed at deregister (runner_forbidden)")
	insertOperationRow(t, store, now, "op-live-2", lifecycle.OperationDrain, "trf-linux-2",
		operations.OperationPending, 3, "runner lifecycle failed at deregister (runner_busy)")
	// Completed and pending operations are not dead letters at all.
	insertOperationRow(t, store, now, "op-done", lifecycle.OperationDrain, "trf-linux-3",
		operations.OperationCompleted, 469, "runner lifecycle failed at deregister (runner_busy)")

	letters, err = store.DeadLetters(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []operations.DeadLetter{
		{OperationID: "op-dead-2", Kind: lifecycle.OperationDrain, Code: "deregister:runner_forbidden",
			ResourceID: "trf-linux-2", Attempts: 720, ResourceProgressing: true},
		{OperationID: operationID, Kind: lifecycle.OperationDrain, Code: "deregister:runner_busy",
			ResourceID: instanceID, Attempts: 835, ResourceProgressing: false},
	}
	if !reflect.DeepEqual(letters, want) {
		t.Fatalf("dead letters=%#v", letters)
	}
}

func TestDeadLettersFailClosedOnStoreFaults(t *testing.T) {
	store := testStore(t)
	if err := store.db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeadLetters(context.Background()); err == nil {
		t.Fatal("closed database dead-letter listing succeeded")
	}
	for name, rows := range map[string]*injectedRows{
		"scan":    {next: true, scanErr: errors.New("scan")},
		"iterate": {rowsErr: errors.New("iterate")},
	} {
		t.Run(name, func(t *testing.T) {
			faulty := testStore(t)
			faulty.injectRows = func(point string) rowsScanner {
				if point == "operations.deadletters.query" {
					return rows
				}
				return nil
			}
			if _, err := faulty.DeadLetters(context.Background()); err == nil {
				t.Fatal("row failure ignored")
			}
		})
	}
}

// The whole remedy in one transaction: the parked operation becomes terminal, the
// phantom row leaves the live inventory, and the counts that gate release updates
// both fall to zero.
func TestDischargeClosesTheParkedOperationAndRetiresTheRow(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Unix(2000, 0).UTC()
	instanceID, operationID := phantom(t, store, now)

	retrying, dead, err := store.OperationCounts(ctx)
	if err != nil || retrying != 0 || dead != 1 {
		t.Fatalf("pre-discharge counts retrying=%d dead=%d err=%v", retrying, dead, err)
	}

	outcome, err := store.DischargeDeadLetter(ctx, operations.Discharge{OperationID: operationID,
		InstanceID: instanceID, ReapInstance: true, Reason: "permanent GitHub registration leak", At: now})
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.OperationDischarged || !outcome.InstanceReaped {
		t.Fatalf("outcome=%#v", outcome)
	}
	if outcome.Ownership.ControllerID != "tart-runner-fleet" || outcome.Ownership.ResourceID != instanceID {
		t.Fatalf("ownership=%#v must let the caller prove the VM belongs to this controller", outcome.Ownership)
	}
	if status := operationStatus(t, store, operationID); status != string(operations.OperationDischarged) {
		t.Fatalf("operation status=%q", status)
	}
	if state := instanceState(t, store, instanceID); state != operations.StateDeleted {
		t.Fatalf("instance state=%q", state)
	}
	live, err := store.LiveInstances(ctx)
	if err != nil || len(live) != 0 {
		t.Fatalf("live instances=%#v err=%v", live, err)
	}
	// A discharged operation is neither retrying nor dead. Leaving it in either
	// count is what kept `fleet update` deferring on a closed wedge.
	retrying, dead, err = store.OperationCounts(ctx)
	if err != nil || retrying != 0 || dead != 0 {
		t.Fatalf("post-discharge counts retrying=%d dead=%d err=%v", retrying, dead, err)
	}
	failures, err := store.OperationFailures(ctx)
	if err != nil || len(failures) != 0 {
		t.Fatalf("discharged operation still published as a failure: %#v err=%v", failures, err)
	}
	letters, err := store.DeadLetters(ctx)
	if err != nil || len(letters) != 0 {
		t.Fatalf("discharged operation still published as a dead letter: %#v err=%v", letters, err)
	}
	// A discharged operation is never claimable again.
	claimed, err := store.Claim(ctx, "worker", 10, now.Add(time.Hour), time.Minute)
	if err != nil || len(claimed) != 0 {
		t.Fatalf("claimed=%#v err=%v", claimed, err)
	}
}

// Re-running the same discharge must be safe: the VM removal happens after the
// transaction, so an operator whose second step failed has to be able to retry.
func TestDischargeIsIdempotentSoAPartialRemedyCanBeRetried(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Unix(3000, 0).UTC()
	instanceID, operationID := phantom(t, store, now)
	request := operations.Discharge{OperationID: operationID, InstanceID: instanceID, ReapInstance: true,
		Reason: "permanent GitHub registration leak", At: now}
	if _, err := store.DischargeDeadLetter(ctx, request); err != nil {
		t.Fatal(err)
	}
	outcome, err := store.DischargeDeadLetter(ctx, request)
	if err != nil {
		t.Fatalf("repeat discharge refused: %v", err)
	}
	if outcome.OperationDischarged || outcome.InstanceReaped {
		t.Fatalf("repeat discharge reported new changes: %#v", outcome)
	}
	if outcome.Ownership.ResourceID != instanceID {
		t.Fatalf("repeat discharge lost the ownership a VM retry needs: %#v", outcome.Ownership)
	}
}

// Discharging the operation alone must leave the instance row untouched: the two
// halves are separate because the smaller one is often all that is needed.
func TestDischargeWithoutReapLeavesTheInstanceRowLive(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Unix(4000, 0).UTC()
	instanceID, operationID := phantom(t, store, now)
	outcome, err := store.DischargeDeadLetter(ctx, operations.Discharge{OperationID: operationID,
		InstanceID: instanceID, Reason: "leave the VM in place for forensics", At: now})
	if err != nil || !outcome.OperationDischarged || outcome.InstanceReaped {
		t.Fatalf("outcome=%#v err=%v", outcome, err)
	}
	if state := instanceState(t, store, instanceID); state != operations.StateDraining {
		t.Fatalf("instance state=%q", state)
	}
}

func TestDischargeRefusesEveryUnsafeRequest(t *testing.T) {
	now := time.Unix(5000, 0).UTC()
	for name, testCase := range map[string]struct {
		mutate  func(t *testing.T, store *Store, instanceID string) operations.Discharge
		wantErr error
	}{
		"incomplete request": {
			mutate: func(_ *testing.T, _ *Store, instanceID string) operations.Discharge {
				return operations.Discharge{OperationID: "op-ea9b705d234ad29f14e79b6d", InstanceID: instanceID, At: now}
			},
			wantErr: operations.ErrInvalid,
		},
		"unknown operation": {
			mutate: func(_ *testing.T, _ *Store, instanceID string) operations.Discharge {
				return operations.Discharge{OperationID: "op-typo", InstanceID: instanceID, Reason: "r", At: now}
			},
			wantErr: operations.ErrNotFound,
		},
		"operation belongs to another instance": {
			mutate: func(_ *testing.T, _ *Store, _ string) operations.Discharge {
				return operations.Discharge{OperationID: "op-ea9b705d234ad29f14e79b6d", InstanceID: "trf-somebody-else",
					Reason: "r", At: now}
			},
			wantErr: operations.ErrResourceMismatch,
		},
		"operation is still retrying": {
			mutate: func(t *testing.T, store *Store, instanceID string) operations.Discharge {
				if _, err := store.db.ExecContext(context.Background(), `UPDATE operations SET status=? WHERE id=?`,
					operations.OperationPending, "op-ea9b705d234ad29f14e79b6d"); err != nil {
					t.Fatal(err)
				}
				return operations.Discharge{OperationID: "op-ea9b705d234ad29f14e79b6d", InstanceID: instanceID,
					Reason: "r", At: now}
			},
			wantErr: operations.ErrNotDeadLettered,
		},
		"another operation can still advance the resource": {
			mutate: func(t *testing.T, store *Store, instanceID string) operations.Discharge {
				insertOperationRow(t, store, now, "op-live", lifecycle.OperationDrain, instanceID,
					operations.OperationClaimed, 1, "")
				return operations.Discharge{OperationID: "op-ea9b705d234ad29f14e79b6d", InstanceID: instanceID,
					Reason: "r", At: now}
			},
			wantErr: operations.ErrResourceProgressing,
		},
		"instance is running a job": {
			mutate: func(t *testing.T, store *Store, instanceID string) operations.Discharge {
				if _, err := store.db.ExecContext(context.Background(), `UPDATE instances SET state=? WHERE id=?`,
					operations.StateRunning, instanceID); err != nil {
					t.Fatal(err)
				}
				return operations.Discharge{OperationID: "op-ea9b705d234ad29f14e79b6d", InstanceID: instanceID,
					ReapInstance: true, Reason: "r", At: now}
			},
			wantErr: operations.ErrInstanceNotReapable,
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := testStore(t)
			instanceID, _ := phantom(t, store, now)
			request := testCase.mutate(t, store, instanceID)
			_, err := store.DischargeDeadLetter(context.Background(), request)
			if !errors.Is(err, testCase.wantErr) {
				t.Fatalf("error=%v want %v", err, testCase.wantErr)
			}
			// A refusal must never leave the operation closed.
			if status := operationStatus(t, store, "op-ea9b705d234ad29f14e79b6d"); status == string(operations.OperationDischarged) {
				t.Fatal("refused discharge still closed the operation")
			}
		})
	}
}

// Every refusal above and below wraps ErrConflict, so callers that only branch on
// conflicts keep behaving exactly as they did before the codes were split out.
func TestDischargeRefusalsRemainConflicts(t *testing.T) {
	for _, err := range []error{operations.ErrResourceMismatch, operations.ErrNotDeadLettered,
		operations.ErrResourceProgressing, operations.ErrInstanceNotReapable} {
		if !errors.Is(err, operations.ErrConflict) {
			t.Fatalf("%v does not wrap ErrConflict", err)
		}
	}
}

func TestDischargeFailsClosedOnStoreFaults(t *testing.T) {
	now := time.Unix(6000, 0).UTC()
	for _, point := range []string{"discharge.begin", "discharge.operation.load", "discharge.progress.check",
		"discharge.operation.update", "discharge.instance.load", "discharge.instance.update",
		"discharge.instance.history", "discharge.commit"} {
		t.Run(point, func(t *testing.T) {
			store := testStore(t)
			instanceID, operationID := phantom(t, store, now)
			store.injectFault = func(candidate string) error {
				if candidate == point {
					return errors.New("database fault")
				}
				return nil
			}
			_, err := store.DischargeDeadLetter(context.Background(), operations.Discharge{OperationID: operationID,
				InstanceID: instanceID, ReapInstance: true, Reason: "leak", At: now})
			if err == nil {
				t.Fatalf("fault at %s ignored", point)
			}
		})
	}
}

// The instance row must exist. A discharge that names an operation whose owner is
// already gone from the table is a state the fleet cannot reason about.
func TestDischargeRefusesWhenTheOwningRowIsAbsent(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Unix(7000, 0).UTC()
	insertOperationRow(t, store, now, "op-orphan", lifecycle.OperationDrain, "trf-vanished",
		operations.OperationDead, 720, "runner lifecycle failed at deregister (runner_busy)")
	_, err := store.DischargeDeadLetter(ctx, operations.Discharge{OperationID: "op-orphan",
		InstanceID: "trf-vanished", Reason: "leak", At: now})
	if !errors.Is(err, operations.ErrNotFound) {
		t.Fatalf("error=%v want ErrNotFound", err)
	}
}

// Which instance states may be retired is a safety decision, so pin it through
// the real mutation for every state the machine defines. A state that migrates to
// the wrong side of this list would silently abandon a live runner.
func TestOnlyCleanupAndTerminalStatesAreReapable(t *testing.T) {
	reapable := map[operations.State]bool{
		operations.StateDraining: true, operations.StateDeregistering: true, operations.StateStopping: true,
		operations.StateFailed: true, operations.StateDeleted: true,
		operations.StatePlanned: false, operations.StateCloning: false, operations.StateBooting: false,
		operations.StateReachable: false, operations.StateRegistering: false, operations.StateOnlineIdle: false,
		operations.StateAssigned: false, operations.StateRunning: false,
	}
	now := time.Unix(9000, 0).UTC()
	for state, want := range reapable {
		t.Run(string(state), func(t *testing.T) {
			store := testStore(t)
			instanceID, operationID := phantom(t, store, now)
			if _, err := store.db.ExecContext(context.Background(), `UPDATE instances SET state=? WHERE id=?`,
				state, instanceID); err != nil {
				t.Fatal(err)
			}
			outcome, err := store.DischargeDeadLetter(context.Background(), operations.Discharge{
				OperationID: operationID, InstanceID: instanceID, ReapInstance: true, Reason: "leak", At: now})
			switch {
			case want && err != nil:
				t.Fatalf("state %q must be reapable: %v", state, err)
			case want && outcome.InstanceReaped == (state == operations.StateDeleted):
				t.Fatalf("state %q reaped=%t", state, outcome.InstanceReaped)
			case !want && !errors.Is(err, operations.ErrInstanceNotReapable):
				t.Fatalf("state %q error=%v want ErrInstanceNotReapable", state, err)
			}
		})
	}
}

// The autonomous state machine must keep refusing the shortcut this discharge
// takes. Only an audited operator mutation may jump a cleanup state to Deleted.
func TestAutonomousTransitionsStillRefuseTheReapShortcut(t *testing.T) {
	store := testStore(t)
	now := time.Unix(8000, 0).UTC()
	instanceID, _ := phantom(t, store, now)
	_, err := store.Advance(context.Background(), lifecycle.StateChange{InstanceID: instanceID,
		ExpectedState: operations.StateDraining, ExpectedVersion: 1, NextState: operations.StateDeleted})
	if !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("autonomous draining->deleted error=%v want ErrInvalid", err)
	}
}
