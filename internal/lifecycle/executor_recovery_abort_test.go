package lifecycle

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// A recovery drain is planned from a single inventory observation, then
// executed as a durable operation that retries until it succeeds. These tests
// pin the 2026-07-20 incident fix: the executor must re-verify the recovery
// premise against ground truth on every attempt and ABORT the drain — rolling
// the instance back to Running — the moment fresh evidence disproves it. A
// standing kill order that merely retries lets one transient observation
// glitch destroy a VM whose runner is executing a live job.

func recoveryInstance(phase int) operations.Instance {
	instance := lifecycleInstance(operations.StateDraining)
	instance.DrainPhase = phase
	return instance
}

func drainExecutor(state *memoryState, vm fakeVM, control *fakeDrainControl) DrainExecutor {
	now := time.Unix(1000, 0).UTC()
	return DrainExecutor{State: state, VM: vm, Control: control, Now: func() time.Time { return now }, ConfirmationMaxAge: time.Minute}
}

// Incident replay: a stopped-recovery drain planned from a glitched power
// reading targets a VM that is actually running a job. Even with the
// registration lookup flaking to "absent" — the exact glitch that released
// the kill on 2026-07-20 — the executor must abort without touching GitHub
// or the VM, and the instance must return to Running.
func TestStoppedRecoveryDrainAbortsWhenVMIsActuallyRunning(t *testing.T) {
	calls := []string{}
	state := &memoryState{instance: recoveryInstance(operations.DrainPhaseStoppedRecovery)}
	control := &fakeDrainControl{calls: &calls, safe: true, registered: false}
	executor := drainExecutor(state, fakeVM{calls: &calls, running: true}, control)

	err := executor.Execute(context.Background(), operations.Operation{Kind: OperationDrain, ResourceID: state.instance.ID})

	if err != nil {
		t.Fatalf("abort must acknowledge the operation as a no-op, got %v", err)
	}
	if state.instance.State != operations.StateRunning {
		t.Fatalf("instance must roll back to running, got %s", state.instance.State)
	}
	if want := []string{"power:trf-small-1"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("only the power re-verification may run before an abort; calls=%#v", calls)
	}
}

// An inactive-recovery drain whose runner is still registered on GitHub has
// its premise disproven: the runner may be executing a job. Retrying the kill
// order until a lookup flake lets it through is exactly the incident; the
// executor must abort instead.
func TestInactiveRecoveryDrainAbortsWhileRunnerStillRegistered(t *testing.T) {
	calls := []string{}
	state := &memoryState{instance: recoveryInstance(operations.DrainPhaseInactiveRecovery)}
	control := &fakeDrainControl{calls: &calls, safe: true, registered: true}
	executor := drainExecutor(state, fakeVM{calls: &calls, running: true}, control)

	err := executor.Execute(context.Background(), operations.Operation{Kind: OperationDrain, ResourceID: state.instance.ID})

	if err != nil {
		t.Fatalf("abort must acknowledge the operation as a no-op, got %v", err)
	}
	if state.instance.State != operations.StateRunning {
		t.Fatalf("instance must roll back to running, got %s", state.instance.State)
	}
	if want := []string{"registered:trf-small-1"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("only the registration re-verification may run before an abort; calls=%#v", calls)
	}
}

// The counterpart guard: a genuinely powered-off VM must still drain, and a
// lingering GitHub registration must neither delay it (the old guard waited
// on !Registered for up to the GitHub timeout) nor gate it behind a lookup
// that can flake. A powered-off VM cannot be executing work.
func TestStoppedRecoveryDrainReclaimsPoweredOffVMDespiteLingeringRegistration(t *testing.T) {
	calls := []string{}
	now := time.Unix(1000, 0).UTC()
	confirmed := operations.DeletionConfirmation{Fresh: true, RunnerInactive: true, JobsInactive: true, ObservedAt: now}
	state := &memoryState{instance: recoveryInstance(operations.DrainPhaseStoppedRecovery)}
	control := &fakeDrainControl{calls: &calls, safe: false, registered: true,
		confirmations: []operations.DeletionConfirmation{confirmed, confirmed}}
	executor := drainExecutor(state, fakeVM{calls: &calls, running: false}, control)

	err := executor.Execute(context.Background(), operations.Operation{Kind: OperationDrain, ResourceID: state.instance.ID})

	if err != nil {
		t.Fatal(err)
	}
	if state.instance.State != operations.StateDeleted {
		t.Fatalf("powered-off VM must be reclaimed, got %s", state.instance.State)
	}
	want := []string{"power:trf-small-1", "deregister:trf-small-1", "confirm:trf-small-1", "stop:trf-small-1", "confirm:trf-small-1", "delete:trf-small-1"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls=%#v", calls)
	}
}

// An inactive-recovery drain whose premise holds — runner deregistered and
// demand completed — proceeds to reclaim as before.
func TestInactiveRecoveryDrainProceedsWhenPremiseHolds(t *testing.T) {
	calls := []string{}
	now := time.Unix(1000, 0).UTC()
	confirmed := operations.DeletionConfirmation{Fresh: true, RunnerInactive: true, JobsInactive: true, ObservedAt: now}
	state := &memoryState{instance: recoveryInstance(operations.DrainPhaseInactiveRecovery)}
	control := &fakeDrainControl{calls: &calls, safe: true, registered: false,
		confirmations: []operations.DeletionConfirmation{confirmed, confirmed}}
	executor := drainExecutor(state, fakeVM{calls: &calls, running: false}, control)

	err := executor.Execute(context.Background(), operations.Operation{Kind: OperationDrain, ResourceID: state.instance.ID})

	if err != nil {
		t.Fatal(err)
	}
	if state.instance.State != operations.StateDeleted {
		t.Fatalf("confirmed-inactive VM must be reclaimed, got %s", state.instance.State)
	}
}

// Premise re-verification must fail closed: if the power probe errors, the
// executor may neither kill nor abort on a guess — the operation retries.
func TestStoppedRecoveryDrainRetriesWhenPowerProbeFails(t *testing.T) {
	calls := []string{}
	state := &memoryState{instance: recoveryInstance(operations.DrainPhaseStoppedRecovery)}
	control := &fakeDrainControl{calls: &calls, safe: true}
	executor := drainExecutor(state, fakeVM{calls: &calls, runningErr: context.DeadlineExceeded}, control)

	err := executor.Execute(context.Background(), operations.Operation{Kind: OperationDrain, ResourceID: state.instance.ID})

	if err == nil || err.Error() != "runner lifecycle failed at drain_guard" {
		t.Fatalf("probe failure must surface as a guard-stage retry, got %v", err)
	}
	if state.instance.State != operations.StateDraining {
		t.Fatalf("instance must stay draining pending fresh evidence, got %s", state.instance.State)
	}
}
