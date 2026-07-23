package integration

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/sqlite"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/lifecycle"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/reconcile"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

// demandA models the production incident: the fleet spawned a builder VM for a
// single suuudokuuu job attempt. GitHub's scale-set broker handed that runner a
// different matching job; the VM finished the other job and was torn down,
// leaving a state='deleted' instance tombstone plus its durable clone operation
// while demand A stayed JobAvailable. The next respawn tick must succeed.
var demandA = domain.DemandKey{Repo: "suuudokuuu", RunID: 100, Attempt: 1, JobID: 200}

func respawnController(store *sqlite.Store) reconcile.Controller {
	return reconcile.Controller{Store: store, ControllerID: "tart-runner-fleet", Mode: reconcile.Authority,
		Profiles: map[domain.ProfileID]domain.Profile{
			"builder": {ID: "builder", Route: "linux-builder", Platform: domain.PlatformLinux, Resources: domain.Resources{CPU: 4, MemoryMB: 8192, Slots: 1}},
		}}
}

func spawnPlan(planID string) scheduler.Plan {
	// The planner content-addresses the operation ID from the demand, so every
	// tick that still sees demand A emits the byte-identical spawn operation.
	// Only the plan ID differs across scheduler generations.
	spawn := scheduler.Operation{ID: "spawn-demand-a", Kind: scheduler.OperationSpawn,
		Demand: demandA, Profile: "builder", Route: "linux-builder"}
	return scheduler.Plan{ID: planID, Status: scheduler.PlanReady, Operations: []scheduler.Operation{spawn}}
}

// tombstoneInstance drives the sole live instance to the terminal deleted state
// through the real lifecycle edges, reproducing the durable tombstone the torn
// down VM left behind.
func tombstoneInstance(t *testing.T, store *sqlite.Store) {
	t.Helper()
	ctx := context.Background()
	live, err := store.LiveInstances(ctx)
	if err != nil || len(live) != 1 {
		t.Fatalf("expected one live instance: %#v %v", live, err)
	}
	current := live[0]
	for _, next := range []operations.State{operations.StateDraining, operations.StateDeregistering, operations.StateStopping, operations.StateDeleted} {
		advanced, err := store.Advance(ctx, lifecycle.StateChange{InstanceID: current.ID, ExpectedState: current.State, ExpectedVersion: current.Version, NextState: next})
		if err != nil {
			t.Fatalf("advance %s->%s: %v", current.State, next, err)
		}
		current = advanced
	}
}

// TestRespawnAfterTerminalIncarnationSucceeds is the end-to-end reproduction of
// the fleet-wedging incident. Before the fix the second Commit fails closed with
// `UNIQUE constraint failed: instances.id` (layer one) and, once that layer is
// addressed, `UNIQUE constraint failed: operations.idempotency_key` (layer two).
func TestRespawnAfterTerminalIncarnationSucceeds(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	controller := respawnController(store)
	now := time.Unix(10_000, 0).UTC()

	if applied, err := controller.Commit(ctx, spawnPlan("tick-1"), "", now); err != nil || !applied {
		t.Fatalf("first spawn commit = %v, %v", applied, err)
	}
	live, err := store.LiveInstances(ctx)
	if err != nil || len(live) != 1 {
		t.Fatalf("first spawn inventory: %#v %v", live, err)
	}
	original := live[0].ID

	tombstoneInstance(t, store)

	// Demand A is still JobAvailable, so the planner emits the identical spawn.
	applied, err := controller.Commit(ctx, spawnPlan("tick-2"), "", now.Add(time.Minute))
	if err != nil || !applied {
		t.Fatalf("respawn commit after terminal incarnation = %v, %v", applied, err)
	}
	live, err = store.LiveInstances(ctx)
	if err != nil || len(live) != 1 {
		t.Fatalf("respawn inventory: %#v %v", live, err)
	}
	if live[0].ID == original {
		t.Fatalf("respawn reused terminal instance identity %q", original)
	}
	if live[0].State != operations.StatePlanned || live[0].Demand != demandA {
		t.Fatalf("respawn instance not runnable: %#v", live[0])
	}
	// The respawn's clone operation must be claimable (layer two): a fresh
	// idempotency key that does not collide with the completed prior operation.
	claimed, err := store.Claim(ctx, "worker", 4, now.Add(2*time.Minute), time.Minute)
	if err != nil {
		t.Fatalf("claim respawn operations: %v", err)
	}
	var clone *operations.Operation
	for i := range claimed {
		if claimed[i].Kind == "clone" && claimed[i].ResourceID == live[0].ID {
			clone = &claimed[i]
		}
	}
	if clone == nil {
		t.Fatalf("respawn produced no runnable clone operation: %#v", claimed)
	}
}

// TestRespawnWithLiveIncarnationHardFails proves terminal-only supersession: a
// still-live prior incarnation is a genuine conflict, so the respawn must fail
// closed rather than silently double-spawn onto the busy demand.
func TestRespawnWithLiveIncarnationHardFails(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	controller := respawnController(store)
	now := time.Unix(20_000, 0).UTC()

	if applied, err := controller.Commit(ctx, spawnPlan("live-1"), "", now); err != nil || !applied {
		t.Fatalf("first spawn commit = %v, %v", applied, err)
	}
	// The instance is still live (planned). Re-spawning the same demand must not
	// be admitted.
	if applied, err := controller.Commit(ctx, spawnPlan("live-2"), "", now.Add(time.Minute)); err == nil || applied {
		t.Fatalf("respawn onto live incarnation admitted: applied=%v err=%v", applied, err)
	}
	live, err := store.LiveInstances(ctx)
	if err != nil || len(live) != 1 {
		t.Fatalf("live incarnation count after rejected respawn: %#v %v", live, err)
	}
}
