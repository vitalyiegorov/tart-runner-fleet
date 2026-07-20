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

// Premise disproven → abort. Case one replays the incident: a stopped-recovery
// drain planned from a glitched power reading targets a VM that is actually
// running a job, while the registration lookup flakes to "absent" — the exact
// glitch that released the kill on 2026-07-20. Case two is the inactive
// variant: a runner still registered on GitHub may be executing a job, so
// retrying the kill order until a lookup flake lets it through is forbidden.
func TestRecoveryDrainAbortsWhenPremiseIsDisproven(t *testing.T) {
	for _, test := range []struct {
		name      string
		phase     int
		vm        fakeVM
		control   fakeDrainControl
		wantCalls []string
	}{
		{name: "stopped recovery with VM actually running and flaky registration lookup",
			phase:     operations.DrainPhaseStoppedRecovery,
			vm:        fakeVM{running: true},
			control:   fakeDrainControl{safe: true, registered: false},
			wantCalls: []string{"power:trf-small-1"}},
		{name: "inactive recovery with runner still registered",
			phase:     operations.DrainPhaseInactiveRecovery,
			vm:        fakeVM{running: true},
			control:   fakeDrainControl{safe: true, registered: true},
			wantCalls: []string{"registered:trf-small-1"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := []string{}
			test.vm.calls, test.control.calls = &calls, &calls
			state := &memoryState{instance: recoveryInstance(test.phase)}
			executor := drainExecutor(state, test.vm, &test.control)

			err := executor.Execute(context.Background(), operations.Operation{Kind: OperationDrain, ResourceID: state.instance.ID})

			if err != nil {
				t.Fatalf("abort must acknowledge the operation as a no-op, got %v", err)
			}
			if state.instance.State != operations.StateRunning {
				t.Fatalf("instance must roll back to running, got %s", state.instance.State)
			}
			if !reflect.DeepEqual(calls, test.wantCalls) {
				t.Fatalf("only the premise re-verification may run before an abort; calls=%#v", calls)
			}
		})
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
// demand completed — proceeds to reclaim as before, and the demand-state
// guard still applies after the registration re-verification.
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

// Premise re-verification must fail closed: if the probe errors — or the
// port is missing entirely — the executor may neither kill nor abort on a
// guess, and the demand-state guard refusal keeps retrying as before.
func TestRecoveryDrainRetriesWhenPremiseCannotBeVerified(t *testing.T) {
	for _, test := range []struct {
		name    string
		phase   int
		noVM    bool
		vm      fakeVM
		control fakeDrainControl
	}{
		{name: "power probe error", phase: operations.DrainPhaseStoppedRecovery,
			vm: fakeVM{runningErr: context.DeadlineExceeded}, control: fakeDrainControl{safe: true}},
		{name: "missing VM port", phase: operations.DrainPhaseStoppedRecovery, noVM: true,
			control: fakeDrainControl{safe: true}},
		{name: "registration probe error", phase: operations.DrainPhaseInactiveRecovery,
			control: fakeDrainControl{safe: true, registeredErr: context.DeadlineExceeded}},
		{name: "inactive premise holds but demand guard refuses", phase: operations.DrainPhaseInactiveRecovery,
			control: fakeDrainControl{safe: false, registered: false}},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := []string{}
			test.vm.calls, test.control.calls = &calls, &calls
			state := &memoryState{instance: recoveryInstance(test.phase)}
			executor := drainExecutor(state, test.vm, &test.control)
			if test.noVM {
				executor.VM = nil
			}

			err := executor.Execute(context.Background(), operations.Operation{Kind: OperationDrain, ResourceID: state.instance.ID})

			if err == nil || err.Error() != "runner lifecycle failed at drain_guard" {
				t.Fatalf("unverifiable premise must surface as a guard-stage retry, got %v", err)
			}
			if state.instance.State != operations.StateDraining {
				t.Fatalf("instance must stay draining pending fresh evidence, got %s", state.instance.State)
			}
		})
	}
}

// The rollback itself must be durable: if the compare-and-swap back to
// Running fails, the operation reports a persistence failure and retries
// rather than acknowledging an abort that never happened.
func TestRecoveryDrainAbortSurfacesPersistenceFailure(t *testing.T) {
	calls := []string{}
	state := &memoryState{instance: recoveryInstance(operations.DrainPhaseStoppedRecovery), advanceErr: operations.ErrConflict}
	control := &fakeDrainControl{calls: &calls}
	executor := drainExecutor(state, fakeVM{calls: &calls, running: true}, control)

	err := executor.Execute(context.Background(), operations.Operation{Kind: OperationDrain, ResourceID: state.instance.ID})

	if err == nil || err.Error() != "runner lifecycle failed at persist" {
		t.Fatalf("failed rollback must surface as a persist-stage retry, got %v", err)
	}
}
