package lifecycle

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// This file is the 2026-08-10 mac studio incident (issue #233).
//
// Instance trf-macos-6x12-f458a747883b9a0d ran a job that SUCCEEDED in two
// minutes, then sat in `deregistering` from 12:55:01Z to 14:17:20Z — 4939
// seconds — while its drain ran the identical graceful stop 67 times and failed
// identically every time, holding the node's entire 6 CPU / 12288 MiB budget
// with twelve jobs queued behind it, the oldest waiting 3h06m. A manual `tart
// stop` from a shell ended it in about thirty seconds.
//
// The tests below pin the rungs the drain had none of.

// wedgedGuest is a guest that will not stop, however politely it is asked. It is
// the whole incident in one value.
var wedgedGuest = errors.New("guest will not power down")

func stopFailure() operations.Operation {
	return operations.Operation{Kind: OperationDrain, ResourceID: "trf-small-1", LastError: safeError(StageStop).Error()}
}

func TestDrainEscalatesAStopThatKeepsFailing(t *testing.T) {
	tests := []struct {
		name     string
		attempts int
		want     string
	}{
		{"first ask is polite", 0, "stop:trf-small-1"},
		{"still polite at the last graceful attempt", GracefulStopAttempts - 1, "stop:trf-small-1"},
		{"stops asking", GracefulStopAttempts, "terminate:trf-small-1"},
		{"still forcing at the last forced attempt", GracefulStopAttempts + ForcedStopAttempts - 1, "terminate:trf-small-1"},
		{"removes the guest", GracefulStopAttempts + ForcedStopAttempts, "destroy:trf-small-1"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor, state, vm, _ := drainFixture(operations.StateDeregistering)
			vm.stopErr, vm.terminateErr, vm.destroyErr = wedgedGuest, wedgedGuest, wedgedGuest
			operation := stopFailure()
			operation.Attempts = test.attempts
			err := executor.Execute(context.Background(), operation)
			if err == nil || err.Error() != safeError(StageStop).Error() {
				t.Fatalf("error = %v, want a bounded stop-stage failure", err)
			}
			if errors.Is(err, operations.ErrExhausted) {
				t.Fatalf("attempt %d exhausted the ladder before it was climbed", test.attempts)
			}
			if got := *vm.calls; len(got) != 1 || got[0] != test.want {
				t.Fatalf("attempt %d called %v, want [%s]", test.attempts, got, test.want)
			}
			if state.instance.State != operations.StateDeregistering {
				t.Fatalf("state = %s, want the drain to stay where it failed", state.instance.State)
			}
		})
	}
}

// TestDrainStopLadderCompletesTheDrainOnceARungWorks is the outcome that
// mattered: the moment the guest is actually off, the drain finishes and the
// vector is released. It pins that a forced stop and a destroy each complete the
// teardown, not merely that they are called.
func TestDrainStopLadderCompletesTheDrainOnceARungWorks(t *testing.T) {
	for _, test := range []struct {
		name     string
		attempts int
		want     string
	}{
		{"forced stop releases the vector", GracefulStopAttempts, "terminate:trf-small-1"},
		{"destroy releases the vector", GracefulStopAttempts + ForcedStopAttempts, "destroy:trf-small-1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			executor, state, vm, _ := drainFixture(operations.StateDeregistering)
			vm.stopErr = wedgedGuest
			operation := stopFailure()
			operation.Attempts = test.attempts
			if err := executor.Execute(context.Background(), operation); err != nil {
				t.Fatal(err)
			}
			if state.instance.State != operations.StateDeleted {
				t.Fatalf("state = %s, want deleted", state.instance.State)
			}
			if got := *vm.calls; !reflect.DeepEqual(got, []string{test.want, "confirm:trf-small-1", "delete:trf-small-1"}) {
				t.Fatalf("calls = %v", got)
			}
		})
	}
}

// TestDrainStopLadderExhaustionIsTerminal is defect 2's other half. A drain that
// has run out of rungs must dead-letter rather than retry, because
// `fleet operations discharge` refuses anything that is not dead — which is
// exactly what it did, with `operation_not_dead`, after 67 attempts and 90
// minutes.
func TestDrainStopLadderExhaustionIsTerminal(t *testing.T) {
	executor, state, vm, _ := drainFixture(operations.StateDeregistering)
	vm.stopErr, vm.terminateErr, vm.destroyErr = wedgedGuest, wedgedGuest, wedgedGuest
	operation := stopFailure()
	operation.Attempts = GracefulStopAttempts + ForcedStopAttempts + DestructiveStopAttempts
	err := executor.Execute(context.Background(), operation)
	if !errors.Is(err, operations.ErrExhausted) {
		t.Fatalf("error = %v, want an exhausted ladder the worker parks", err)
	}
	if err.Error() != safeError(StageStop).Error() {
		t.Fatalf("persisted text = %q, want the same closed code an ordinary stop failure persists", err.Error())
	}
	if len(*vm.calls) != 0 {
		t.Fatalf("calls = %v, want no further attempt on a guest that has refused every rung", *vm.calls)
	}
	if state.instance.State != operations.StateDeregistering {
		t.Fatalf("state = %s", state.instance.State)
	}
}

// TestDrainOpensAtTheGentlestRungAfterFailuresElsewhere keeps the ladder honest.
// A drain that has been refused forty times by GitHub at the deregister step has
// not yet asked its guest to stop even once, and must not open at a forceful
// rung on the strength of failures that happened somewhere else.
func TestDrainOpensAtTheGentlestRungAfterFailuresElsewhere(t *testing.T) {
	for _, lastError := range []string{"", classifiedError(StageDeregister, errors.New("refused")).Error(),
		safeError(StageGuard).Error(), "executor panic: something unclassified"} {
		executor, _, vm, _ := drainFixture(operations.StateDeregistering)
		vm.stopErr = wedgedGuest
		operation := stopFailure()
		operation.LastError = lastError
		operation.Attempts = 500
		if err := executor.Execute(context.Background(), operation); err == nil {
			t.Fatalf("last error %q: want the graceful stop to fail", lastError)
		}
		if got := *vm.calls; len(got) != 1 || got[0] != "stop:trf-small-1" {
			t.Fatalf("last error %q called %v, want a graceful stop", lastError, got)
		}
	}
}

// TestDrainNeverEscalatesAGuestWhoseJobIsStillRunning is the constraint the
// ladder must never violate. An event drain re-verifies runner-scoped busy
// evidence before every destructive step (ADR 0033); no attempt count may
// overrule it.
func TestDrainNeverEscalatesAGuestWhoseJobIsStillRunning(t *testing.T) {
	executor, state, vm, control := drainFixture(operations.StateDraining)
	control.busy = true
	operation := stopFailure()
	operation.Attempts = GracefulStopAttempts + ForcedStopAttempts + DestructiveStopAttempts
	if err := executor.Execute(context.Background(), operation); err != nil {
		t.Fatal(err)
	}
	if state.instance.State != operations.StateRunning {
		t.Fatalf("state = %s, want the drain aborted back to running", state.instance.State)
	}
	for _, call := range *vm.calls {
		if call == "terminate:trf-small-1" || call == "destroy:trf-small-1" || call == "stop:trf-small-1" {
			t.Fatalf("calls = %v: the ladder cut a guest whose job is still running", *vm.calls)
		}
	}
}

// TestOccupancyBudgetReclaimClimbsTheSameLadder is defect 3. ADR 0036 bounds how
// long an instance may hold its vector, but its only enforcement action is a
// drain, and the drain's stop is the thing that was broken. A budget whose
// remedy can itself hang is not a bound.
func TestOccupancyBudgetReclaimClimbsTheSameLadder(t *testing.T) {
	executor, state, vm, _ := drainFixture(operations.StateDraining)
	state.instance.DrainPhase = operations.DrainPhaseOccupancyBudget
	vm.stopErr = wedgedGuest
	operation := stopFailure()
	operation.Attempts = GracefulStopAttempts
	if err := executor.Execute(context.Background(), operation); err != nil {
		t.Fatal(err)
	}
	if got := (*vm.calls)[0]; got != "terminate:trf-small-1" {
		t.Fatalf("first call = %q, want the budget reclaim to force the guest off", got)
	}
	if state.instance.State != operations.StateDeleted {
		t.Fatalf("state = %s, want the reclaim to complete", state.instance.State)
	}
}

func TestStopEscalationVocabularyIsClosed(t *testing.T) {
	want := map[StopForce]string{StopGraceful: "graceful", StopForced: "forced",
		StopDestructive: "destructive", StopExhausted: "exhausted"}
	for force, name := range want {
		if force.String() != name {
			t.Fatalf("StopForce(%d).String() = %q, want %q", force, force.String(), name)
		}
	}
	if got := StopForce(99).String(); got != "exhausted" {
		t.Fatalf("unknown rung rendered %q", got)
	}
}
