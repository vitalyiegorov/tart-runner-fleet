package reconcile

import (
	"context"
	"errors"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

// recoveryDrain is the operation shape the scheduler content-addresses for a
// lingering-runner recovery. Its identity carries no version, timestamp, or
// generation, so every tick that re-derives the same recovery for the same
// instance produces this exact ID.
func recoveryDrain(instance string) scheduler.Operation {
	return scheduler.Operation{ID: "op-recovery", Kind: scheduler.OperationDrain, Instance: instance,
		Profile: "xl", Route: "linux-xl", Recovery: true, LingeringRunner: true}
}

func runningInstance(id string) operations.Instance {
	return operations.Instance{ID: id, State: operations.StateRunning, Version: 9,
		Ownership: operations.Ownership{ControllerID: "controller", ResourceID: "owner/repo/1/1/1", OperationID: "op-prior"}}
}

// TestRecoveryDrainSupersedesATerminalPriorAttempt replays the second half of
// the 2026-08-02 incident on host vitalii-mac-mini.
//
// A recovery drain of a busy runner is aborted by DrainExecutor.abort: fresh
// evidence shows a workflow job executing, so the instance returns to Running
// and the drain operation is completed as a no-op. RunningSince is reset by that
// write, so the lingering-runner deadline elapses again and the scheduler
// re-derives the byte-identical operation — the identity is content-addressed
// over {Kind, Instance, Profile, Route, Recovery, ConfirmedInactive,
// StalledAssignment, LingeringRunner} and none of those changed.
//
// Live evidence: trf-xl-25a374b60f46dafe carried op-3bef38e0bcc57090138000db
// (stalled assignment, 09:44:48Z) and op-e31b41beec0e2f30cc3d30d0 (lingering
// runner, 09:59:51Z), both completed. It escaped a wedge only because the
// recovery FLAGS differed between the two attempts. No third flag combination is
// reachable from Running, so the next lingering-runner abort re-derives
// op-e31b41beec0e2f30cc3d30d0, whose row already exists: insertOperation fails
// the operations primary key, ApplyPlan wraps it bare, and the tick reports
// plan_commit_failed on every later tick forever.
//
// Spawns already solve this with SpawnGeneration; drains must too.
func TestRecoveryDrainSupersedesATerminalPriorAttempt(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		generation int
		wantSame   bool
	}{
		{name: "first attempt keeps the scheduler content address", generation: 0, wantSame: true},
		{name: "one terminal prior attempt is superseded", generation: 1},
		{name: "several terminal prior attempts are superseded", generation: 4},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store := &fakeStore{state: operations.SchedulerState{Version: 4}, drainGeneration: testCase.generation,
				instances: map[string]operations.Instance{"trf-xl-1": runningInstance("trf-xl-1")}}
			controller := Controller{Store: store, ControllerID: "controller", Mode: Authority}

			applied, err := controller.Commit(context.Background(), readyPlan(recoveryDrain("trf-xl-1")), "", controllerNow)
			if err != nil || !applied {
				t.Fatalf("Commit() = %v, %v", applied, err)
			}
			durable := store.applied[0]
			if len(durable.Operations) != 1 {
				t.Fatalf("operations = %#v, want exactly one deregister", durable.Operations)
			}
			got := durable.Operations[0]
			if got.ID != got.IdempotencyKey {
				t.Fatalf("identity = %q, idempotency key = %q; they must not drift", got.ID, got.IdempotencyKey)
			}
			if same := got.ID == "op-recovery"; same != testCase.wantSame {
				t.Fatalf("operation identity = %q (same as content address: %v), want same=%v", got.ID, same, testCase.wantSame)
			}
			// The effect key is the physical effect, not the attempt, so it must stay
			// stable across generations: operation_effects is what makes deregistering
			// the same runner twice idempotent.
			if got.EffectKey != "deregister:trf-xl-1" {
				t.Fatalf("effect key = %q, want the generation-independent physical effect", got.EffectKey)
			}
			if len(durable.Instances) != 1 || durable.Instances[0].Instance.DrainPhase != operations.DrainPhaseLingeringRunner {
				t.Fatalf("instance intent = %#v, want an unchanged lingering-runner drain", durable.Instances)
			}
		})
	}
}

// TestDrainGenerationsAreDistinctPerAttempt pins that consecutive generations
// never collide with each other or with the content address, which is the whole
// point: a superseding identity that repeated itself would wedge exactly as the
// unsalted one did.
func TestDrainGenerationsAreDistinctPerAttempt(t *testing.T) {
	seen := make(map[string]int, 8)
	for generation := range 8 {
		store := &fakeStore{state: operations.SchedulerState{Version: 4}, drainGeneration: generation,
			instances: map[string]operations.Instance{"trf-xl-1": runningInstance("trf-xl-1")}}
		if _, err := (Controller{Store: store, ControllerID: "controller", Mode: Authority}).
			Commit(context.Background(), readyPlan(recoveryDrain("trf-xl-1")), "", controllerNow); err != nil {
			t.Fatalf("Commit() generation %d = %v", generation, err)
		}
		id := store.applied[0].Operations[0].ID
		if previous, repeated := seen[id]; repeated {
			t.Fatalf("generation %d reuses the identity of generation %d (%q)", generation, previous, id)
		}
		seen[id] = generation
	}
}

// TestDrainGenerationFailureRefusesTheCommit keeps the path fail-closed: an
// unreadable generation must never fall back to the unsalted identity, because
// that is precisely the write the durable layer refuses.
func TestDrainGenerationFailureRefusesTheCommit(t *testing.T) {
	sentinel := errors.New("generation unreadable")
	store := &fakeStore{state: operations.SchedulerState{Version: 4}, drainGenerationErr: sentinel,
		instances: map[string]operations.Instance{"trf-xl-1": runningInstance("trf-xl-1")}}

	applied, err := (Controller{Store: store, ControllerID: "controller", Mode: Authority}).
		Commit(context.Background(), readyPlan(recoveryDrain("trf-xl-1")), "", controllerNow)

	if applied || !errors.Is(err, sentinel) {
		t.Fatalf("Commit() = %v, %v; want the generation error surfaced and nothing written", applied, err)
	}
	if len(store.applied) != 0 {
		t.Fatalf("plans written = %d, want none", len(store.applied))
	}
}

// TestShadowModeReadsNoDrainGeneration keeps shadow free of the extra read: it
// translates no operations at all, so it must not touch the store for one.
func TestShadowModeReadsNoDrainGeneration(t *testing.T) {
	store := &fakeStore{state: operations.SchedulerState{Version: 4}, drainGenerationErr: errors.New("must not be read")}

	if _, err := (Controller{Store: store, ControllerID: "controller", Mode: Shadow}).
		Commit(context.Background(), readyPlan(recoveryDrain("trf-xl-1")), "", controllerNow); err != nil {
		t.Fatalf("Commit() = %v", err)
	}
}
