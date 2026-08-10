package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/lifecycle"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// TestStalledOperationsNamesTheWedgeTheOperatorHadToReadByHand replays the
// 2026-08-10 incident's durable shape (issue #233): instance
// trf-macos-6x12-f458a747883b9a0d held in `deregistering` since 12:55:01Z, and
// its drain still pending after 67 attempts, all failing at the stop step. Every
// field below was in the operations and instances tables the whole time and none
// of it was published.
func TestStalledOperationsNamesTheWedgeTheOperatorHadToReadByHand(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	entered := time.Date(2026, 8, 10, 12, 55, 1, 0, time.UTC)
	read := time.Date(2026, 8, 10, 14, 16, 0, 0, time.UTC)
	instance := "trf-macos-6x12-f458a747883b9a0d"
	if err := store.CreateInstance(ctx, operations.Instance{ID: instance, State: operations.StateDraining,
		Ownership: operations.Ownership{ControllerID: "tart-runner-fleet", ResourceID: instance, OperationID: "op-provision"},
		CreatedAt: entered}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE instances SET state=?,updated_at=? WHERE id=?`,
		operations.StateDeregistering, entered.UnixNano(), instance); err != nil {
		t.Fatal(err)
	}
	insertOperationRow(t, store, entered, "event-drain-"+instance, lifecycle.OperationDrain, instance,
		operations.OperationPending, 67, "runner lifecycle failed at stop")

	stalled, err := store.StalledOperations(ctx, read)
	if err != nil {
		t.Fatal(err)
	}
	if len(stalled) != 1 {
		t.Fatalf("stalled = %#v, want exactly the wedge", stalled)
	}
	row := stalled[0]
	if row.OperationID != "event-drain-"+instance || row.Kind != lifecycle.OperationDrain || row.Code != "stop" ||
		row.Instance != instance || row.Attempts != 67 {
		t.Fatalf("row = %#v", row)
	}
	if row.DrainState != string(operations.StateDeregistering) {
		t.Fatalf("drain state = %q, want the cleanup state holding the vector", row.DrainState)
	}
	want := read.Sub(entered)
	if row.Retrying != want || row.Held != want {
		t.Fatalf("retrying=%s held=%s, want %s for both", row.Retrying, row.Held, want)
	}
}

// TestStalledOperationsNamesAnInstanceWhoseDrainAlreadyDeadLettered is the half
// a join alone would miss: once the drain is parked there is no retrying
// operation left, and the instance is at its most stuck.
func TestStalledOperationsNamesAnInstanceWhoseDrainAlreadyDeadLettered(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 10, 14, 16, 0, 0, time.UTC)
	instance, _ := phantom(t, store, now.Add(-3*time.Hour))
	stalled, err := store.StalledOperations(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(stalled) != 1 {
		t.Fatalf("stalled = %#v", stalled)
	}
	row := stalled[0]
	if row.OperationID != "" || row.Kind != "" {
		t.Fatalf("a dead-lettered drain was reported as still retrying: %#v", row)
	}
	if row.Instance != instance || row.DrainState != string(operations.StateDraining) {
		t.Fatalf("row = %#v", row)
	}
	if row.Held <= 0 {
		t.Fatalf("held = %s, want the time the instance has been parked", row.Held)
	}
}

// TestStalledOperationsIsSilentWhenEverythingIsProgressing keeps the check from
// crying wolf: a first attempt, a completed operation, and a live instance that
// is not tearing down are all ordinary.
func TestStalledOperationsIsSilentWhenEverythingIsProgressing(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 10, 14, 16, 0, 0, time.UTC)
	if err := store.CreateInstance(ctx, operations.Instance{ID: "trf-small-busy", State: operations.StateRunning,
		Ownership: operations.Ownership{ControllerID: "c", ResourceID: "trf-small-busy", OperationID: "op"},
		CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	insertOperationRow(t, store, now, "op-first-try", lifecycle.OperationDrain, "trf-small-busy",
		operations.OperationPending, 0, "")
	insertOperationRow(t, store, now, "op-done", lifecycle.OperationProvision, "trf-small-busy",
		operations.OperationCompleted, 4, "runner lifecycle failed at start")
	stalled, err := store.StalledOperations(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(stalled) != 0 {
		t.Fatalf("stalled = %#v, want nothing", stalled)
	}
}

// TestStalledOperationsNeverPublishesANegativeOrUnreadableAge keeps a clock skew
// from rendering as an alarming duration.
func TestStalledOperationsNeverPublishesANegativeOrUnreadableAge(t *testing.T) {
	now := time.Date(2026, 8, 10, 14, 16, 0, 0, time.UTC)
	if got := since(now, 0); got != 0 {
		t.Fatalf("since(unreadable) = %s", got)
	}
	if got := since(now, now.Add(time.Hour).UnixNano()); got != 0 {
		t.Fatalf("since(future) = %s", got)
	}
	if got := since(now, now.Add(-time.Minute).UnixNano()); got != time.Minute {
		t.Fatalf("since(a minute ago) = %s", got)
	}
}
