package integration

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/sqlite"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/lifecycle"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

// lingeringRecoveryPlan is the plan every tick emits once a Running instance has
// held the idle-runner deadline with no active job on its bound demand. The
// scheduler content-addresses the operation from that decision alone, so the ID
// is byte-identical on every recurrence.
func lingeringRecoveryPlan(planID, instance string) scheduler.Plan {
	drain := scheduler.Operation{ID: "drain-lingering-runner", Kind: scheduler.OperationDrain, Instance: instance,
		Profile: "builder", Route: "linux-builder", Recovery: true, LingeringRunner: true}
	return scheduler.Plan{ID: planID, Status: scheduler.PlanReady, Operations: []scheduler.Operation{drain}}
}

// advanceTo drives the sole live instance along real lifecycle edges.
func advanceTo(t *testing.T, store *sqlite.Store, id string, states ...operations.State) operations.Instance {
	t.Helper()
	ctx := context.Background()
	current, err := store.Instance(ctx, id)
	if err != nil {
		t.Fatalf("load %s: %v", id, err)
	}
	for _, next := range states {
		current, err = store.Advance(ctx, lifecycle.StateChange{InstanceID: current.ID,
			ExpectedState: current.State, ExpectedVersion: current.Version, NextState: next})
		if err != nil {
			t.Fatalf("advance %s -> %s: %v", current.State, next, err)
		}
	}
	return current
}

// abortDrain reproduces DrainExecutor.abort: fresh evidence shows a workflow job
// executing on the runner, so the drain is completed as an acknowledged no-op
// and the instance returns to Running — the conservative busy state.
func abortDrain(t *testing.T, store *sqlite.Store, instance string, now time.Time) {
	t.Helper()
	ctx := context.Background()
	claimed, err := store.Claim(ctx, "worker", 4, now, time.Minute)
	if err != nil {
		t.Fatalf("claim drain: %v", err)
	}
	found := false
	for _, operation := range claimed {
		if operation.Kind != lifecycle.OperationDrain || operation.ResourceID != instance {
			continue
		}
		if _, err := store.Complete(ctx, operation.ID, "worker", operation.EffectKey, now); err != nil {
			t.Fatalf("complete drain %s: %v", operation.ID, err)
		}
		found = true
	}
	if !found {
		t.Fatalf("no claimable drain for %s: %#v", instance, claimed)
	}
	advanceTo(t, store, instance, operations.StateRunning)
}

// TestRecoveryDrainRepeatsAfterAnAbortedAttempt is the end-to-end reproduction of
// the second half of the 2026-08-02 wedge on host vitalii-mac-mini.
//
// GitHub's scale-set broker handed the spawned runner a sibling job from the same
// scale set, so the demand the instance was bound to stayed JobAvailable. The
// scheduler's JobInactive evidence (keyed on the bound demand) therefore reported
// no active job while the executor's RunnerBusy evidence (keyed on the runner
// name) reported one. Every idle-runner deadline planned a recovery drain and
// every drain aborted, returning the instance to Running and resetting the
// deadline.
//
// Before the fix the second commit fails closed with
// `UNIQUE constraint failed: operations.id`, which ApplyPlan wraps bare. That is
// neither ErrConflict nor ErrInvalid, so the tick reports plan_commit_failed —
// and because the planner is pure, every later tick rebuilds the same refused
// plan and the control plane never schedules again.
func TestRecoveryDrainRepeatsAfterAnAbortedAttempt(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	controller := respawnController(store)
	now := time.Unix(30_000, 0).UTC()

	if applied, err := controller.Commit(ctx, spawnPlan("drain-tick-0"), "", now); err != nil || !applied {
		t.Fatalf("spawn commit = %v, %v", applied, err)
	}
	live, err := store.LiveInstances(ctx)
	if err != nil || len(live) != 1 {
		t.Fatalf("spawn inventory: %#v %v", live, err)
	}
	instance := live[0].ID
	advanceTo(t, store, instance, operations.StateCloning, operations.StateBooting, operations.StateReachable,
		operations.StateRegistering, operations.StateAssigned, operations.StateRunning)

	identities := make(map[string]bool, 3)
	for attempt := range 3 {
		at := now.Add(time.Duration(attempt+1) * 15 * time.Minute)
		applied, err := controller.Commit(ctx, lingeringRecoveryPlan("drain-tick-"+string(rune('a'+attempt)), instance), "", at)
		if err != nil || !applied {
			t.Fatalf("recovery drain attempt %d = %v, %v", attempt, applied, err)
		}
		current, err := store.Instance(ctx, instance)
		if err != nil || current.State != operations.StateDraining || current.DrainPhase != operations.DrainPhaseLingeringRunner {
			t.Fatalf("attempt %d durable state = %#v, %v", attempt, current, err)
		}
		claimed, err := store.Claim(ctx, "probe", 4, at, time.Minute)
		if err != nil {
			t.Fatalf("attempt %d claim: %v", attempt, err)
		}
		runnable := ""
		for _, operation := range claimed {
			if operation.Kind == lifecycle.OperationDrain && operation.ResourceID == instance {
				runnable = operation.ID
				if operation.EffectKey != "deregister:"+instance {
					t.Fatalf("attempt %d effect key = %q; the physical effect must not be salted", attempt, operation.EffectKey)
				}
			}
		}
		if runnable == "" {
			t.Fatalf("attempt %d produced no claimable drain: %#v", attempt, claimed)
		}
		if identities[runnable] {
			t.Fatalf("attempt %d reused drain identity %q", attempt, runnable)
		}
		identities[runnable] = true
		abortDrain(t, store, instance, at.Add(time.Minute))
	}
}

// TestRecoveryDrainOfAnInFlightAttemptStillFailsClosed proves terminal-only
// supersession for drains. While the prior attempt is still claimed it can still
// act, so a second drain of the same instance must be refused rather than
// enqueuing the deregistration twice.
func TestRecoveryDrainOfAnInFlightAttemptStillFailsClosed(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	controller := respawnController(store)
	now := time.Unix(40_000, 0).UTC()

	if applied, err := controller.Commit(ctx, spawnPlan("inflight-0"), "", now); err != nil || !applied {
		t.Fatalf("spawn commit = %v, %v", applied, err)
	}
	live, err := store.LiveInstances(ctx)
	if err != nil || len(live) != 1 {
		t.Fatalf("spawn inventory: %#v %v", live, err)
	}
	instance := live[0].ID
	advanceTo(t, store, instance, operations.StateCloning, operations.StateBooting, operations.StateReachable,
		operations.StateRegistering, operations.StateAssigned, operations.StateRunning)

	if applied, err := controller.Commit(ctx, lingeringRecoveryPlan("inflight-a", instance), "", now.Add(time.Minute)); err != nil || !applied {
		t.Fatalf("first recovery drain = %v, %v", applied, err)
	}
	// The drain is in flight and the instance is Draining, so the scheduler could
	// only re-derive this operation from stale observation. It must not be
	// admitted.
	if applied, err := controller.Commit(ctx, lingeringRecoveryPlan("inflight-b", instance), "", now.Add(2*time.Minute)); err == nil || applied {
		t.Fatalf("drain of an in-flight attempt admitted: applied=%v err=%v", applied, err)
	}
}
