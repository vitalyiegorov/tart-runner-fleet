package lifecycle

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// An occupancy-budget reclaim is the one drain whose premise a busy runner does
// not disprove: the job it ends is executing, and that is why it is being ended
// (ADR 0036, issue #223). Two consequences have to hold at execution time, and
// neither is true of any other phase.
//
// First, the ephemeral guest is powered off BEFORE deregistration. GitHub
// refuses to remove a runner it considers to be executing a job, so
// deregistering first can only retry until the operation dead-letters with the
// vector still held — which is the incident, not the fix.
//
// Second, no busy probe may abort it. Aborting returns the instance to Running
// with its vector intact, and the next tick re-derives the same reap, so an
// abort here is an infinite loop that reclaims nothing.

func TestOccupancyBudgetDrainStopsTheGuestBeforeDeregistering(t *testing.T) {
	calls := []string{}
	confirmed := operations.DeletionConfirmation{Fresh: true, RunnerInactive: true, JobsInactive: true, ObservedAt: time.Unix(1000, 0).UTC()}
	state := &memoryState{instance: recoveryInstance(operations.DrainPhaseOccupancyBudget)}
	control := &fakeDrainControl{calls: &calls, busy: true,
		confirmations: []operations.DeletionConfirmation{confirmed, confirmed}}
	executor := drainExecutor(state, fakeVM{calls: &calls, running: true}, control)

	if err := executor.Execute(context.Background(), operations.Operation{Kind: OperationDrain, ResourceID: state.instance.ID}); err != nil {
		t.Fatal(err)
	}

	if state.instance.State != operations.StateDeleted {
		t.Fatalf("an over-budget instance must be reclaimed, got %s", state.instance.State)
	}
	want := []string{"stop:trf-small-1", "deregister:trf-small-1", "confirm:trf-small-1",
		"stop:trf-small-1", "confirm:trf-small-1", "delete:trf-small-1"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("the guest must be stopped before deregistration; calls=%#v", calls)
	}
	if control.busyCalls != 0 {
		t.Fatalf("a busy runner does not disprove an occupancy budget, so it must not be probed; %d probes", control.busyCalls)
	}
}

func TestOccupancyBudgetDrainNeverAbortsOnABusyRefusal(t *testing.T) {
	calls := []string{}
	state := &memoryState{instance: recoveryInstance(operations.DrainPhaseOccupancyBudget)}
	control := &fakeDrainControl{calls: &calls, busy: true, deregisterErr: errors.New("job still running")}
	executor := drainExecutor(state, fakeVM{calls: &calls, running: true}, control)

	err := executor.Execute(context.Background(), operations.Operation{Kind: OperationDrain, ResourceID: state.instance.ID})

	if err == nil || err.Error() != "runner lifecycle failed at deregister (deregister_failed)" {
		t.Fatalf("a refused deregistration must stay a classified retry, got %v", err)
	}
	if state.instance.State != operations.StateDraining {
		t.Fatalf("a budget reclaim must never roll back to running, got %s", state.instance.State)
	}
	if control.busyCalls != 0 {
		t.Fatalf("the busy abort must not be consulted for this phase; %d probes", control.busyCalls)
	}
}

// Stopping the guest is a destructive step, so an unstoppable VM — or a missing
// VM port — must fail closed as a retryable stage rather than proceeding to
// deregister a runner whose job is still executing.
func TestOccupancyBudgetDrainFailsClosedWhenTheGuestCannotBeStopped(t *testing.T) {
	for _, test := range []struct {
		name string
		noVM bool
		vm   fakeVM
	}{
		{name: "stop refused", vm: fakeVM{stopErr: context.DeadlineExceeded, running: true}},
		{name: "missing VM port", noVM: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := []string{}
			test.vm.calls = &calls
			state := &memoryState{instance: recoveryInstance(operations.DrainPhaseOccupancyBudget)}
			control := &fakeDrainControl{calls: &calls}
			executor := drainExecutor(state, test.vm, control)
			if test.noVM {
				executor.VM = nil
			}

			err := executor.Execute(context.Background(), operations.Operation{Kind: OperationDrain, ResourceID: state.instance.ID})

			if err == nil || err.Error() != "runner lifecycle failed at stop" {
				t.Fatalf("an unstoppable guest must surface as a stop-stage retry, got %v", err)
			}
			if state.instance.State != operations.StateDraining {
				t.Fatalf("instance must stay draining, got %s", state.instance.State)
			}
			for _, call := range calls {
				if call == "deregister:trf-small-1" {
					t.Fatalf("deregistration must not run once the guest could not be stopped; calls=%#v", calls)
				}
			}
		})
	}
}
