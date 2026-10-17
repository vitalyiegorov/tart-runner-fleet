package lifecycle

import (
	"context"
	"errors"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// This file is the 2026-10-11 Mac Studio incident at the drain edge (ADR 0067):
// a guest whose bootstrap could never succeed is reclaimed so its demand can be
// provisioned on a fresh clone. The instance never became a runner, so the one
// way this drain could be wrong is to treat it as one: aborting it to Running
// would invent a busy runner where there has never been one.

func stalledProvisionDrain() (DrainExecutor, *memoryState, *fakeVM, *fakeDrainControl) {
	executor, state, vm, control := drainFixture(operations.StateDraining)
	state.instance.DrainPhase = operations.DrainPhaseProvisionStalled
	return executor, state, vm, control
}

func TestAStalledProvisioningIsTornDownToDeleted(t *testing.T) {
	executor, state, vm, control := stalledProvisionDrain()
	if err := executor.Execute(context.Background(), operations.Operation{Kind: OperationDrain, ResourceID: "trf-small-1"}); err != nil {
		t.Fatal(err)
	}
	if state.instance.State != operations.StateDeleted {
		t.Fatalf("state = %s, want the stuck VM deleted so the demand can be provisioned again", state.instance.State)
	}
	if control.busyCalls != 1 {
		t.Fatalf("the drain must re-check the runner exactly once before deregistering; asked %d", control.busyCalls)
	}
	deregistered, deleted := false, false
	for _, call := range *vm.calls {
		deregistered = deregistered || call == "deregister:trf-small-1"
		deleted = deleted || call == "delete:trf-small-1"
	}
	if !deregistered || !deleted {
		t.Fatalf("calls = %v, want the runner record deregistered and the VM deleted", *vm.calls)
	}
}

// A runner the stalled provisioning started after all, executing a job, holds
// the drain at the guard until the job is over. It never returns the instance
// to Running, and an unreadable answer is not permission to proceed either.
func TestAStalledProvisioningNeverAbortsToRunning(t *testing.T) {
	for _, test := range []struct {
		name    string
		busy    bool
		busyErr error
	}{
		{name: "the runner is busy", busy: true},
		{name: "busy evidence could not be read", busyErr: errors.New("unreadable")},
	} {
		t.Run(test.name, func(t *testing.T) {
			executor, state, vm, control := stalledProvisionDrain()
			control.busy, control.busyErr = test.busy, test.busyErr
			err := executor.Execute(context.Background(), operations.Operation{Kind: OperationDrain, ResourceID: "trf-small-1"})
			if err == nil || err.Error() != safeError(StageGuard).Error() {
				t.Fatalf("error = %v, want a retryable guard failure", err)
			}
			if state.instance.State != operations.StateDraining || len(*vm.calls) != 0 {
				t.Fatalf("state = %s calls = %v, want the drain held at the guard untouched", state.instance.State, *vm.calls)
			}
		})
	}
}

// GitHub refusing the deregistration while the runner turns busy between the
// guard and the call is the same evidence arriving late, and it must not abort
// either: the deregister-refusal branch consults this predicate.
func TestOnlyTheRunnerPhasesAbortOnABusyRefusal(t *testing.T) {
	for phase, want := range map[int]bool{
		operations.DrainPhaseStalledAssignment: true,
		operations.DrainPhaseLingeringRunner:   true,
		operations.DrainPhaseInactiveRecovery:  true,
		1 /* the ordinary event drain */ :      true,
		operations.DrainPhaseOccupancyBudget:   false,
		operations.DrainPhaseGuestUnresponsive: false,
		operations.DrainPhaseProvisionStalled:  false,
	} {
		if got := abortsOnBusyRefusal(phase); got != want {
			t.Fatalf("abortsOnBusyRefusal(%d) = %v, want %v", phase, got, want)
		}
	}
}
