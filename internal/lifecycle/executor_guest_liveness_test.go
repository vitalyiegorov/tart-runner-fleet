package lifecycle

import (
	"context"
	"errors"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// This file is the 2026-08-16 incident (issue #236) at the drain edge.
//
// A `--privileged` container panicked its guest's kernel. `tart list` went on
// reporting the VM running, GitHub went on reporting the job in_progress, and
// the vector was held until GitHub's grace timer expired sixteen to eighteen
// minutes later. The reclaim below is what ends that, and every test here is
// about the two ways it could be wrong: acting on a guest that came back, or
// failing to act at all.

type fixedGuestProbe struct {
	answer domain.GuestLiveness
	asked  int
}

func (p *fixedGuestProbe) Probe(context.Context, string) domain.GuestLiveness {
	p.asked++
	return p.answer
}

func guestDrainFixture(answer domain.GuestLiveness) (DrainExecutor, *memoryState, *fakeVM, *fixedGuestProbe) {
	executor, state, vm, _ := drainFixture(operations.StateDraining)
	state.instance.DrainPhase = operations.DrainPhaseGuestUnresponsive
	probe := &fixedGuestProbe{answer: answer}
	executor.Guest = probe
	return executor, state, vm, probe
}

// The reclaim, end to end: the guest is powered off before deregistration --
// GitHub will not remove a runner it considers busy -- and the vector is
// released without waiting for GitHub to notice anything.
func TestADeadGuestIsStoppedBeforeItsRunnerIsDeregistered(t *testing.T) {
	executor, state, vm, probe := guestDrainFixture(domain.GuestLivenessRefused)
	if err := executor.Execute(context.Background(), operations.Operation{Kind: OperationDrain, ResourceID: "trf-small-1"}); err != nil {
		t.Fatal(err)
	}
	if probe.asked != 1 {
		t.Fatalf("the drain must re-verify its premise exactly once per attempt; asked %d", probe.asked)
	}
	if state.instance.State != operations.StateDeleted {
		t.Fatalf("state = %s, want the reclaim to complete", state.instance.State)
	}
	calls := *vm.calls
	stopAt, deregisterAt := -1, -1
	// First occurrence only: the deregistering arm stops the guest again, and an
	// idempotent second stop says nothing about the ordering under test.
	for index, call := range calls {
		if call == "stop:trf-small-1" && stopAt < 0 {
			stopAt = index
		}
		if call == "deregister:trf-small-1" && deregisterAt < 0 {
			deregisterAt = index
		}
	}
	if stopAt < 0 || deregisterAt < 0 || stopAt > deregisterAt {
		t.Fatalf("the guest must be stopped before its runner is deregistered; calls = %v", calls)
	}
}

// The false-positive guard at the last possible moment. A guest that answers the
// fresh probe is alive, whatever the accumulator said a tick ago, and the drain
// returns the instance to Running with its job untouched.
func TestAGuestThatAnswersTheFreshProbeIsNotCut(t *testing.T) {
	executor, state, vm, probe := guestDrainFixture(domain.GuestLivenessAlive)
	if err := executor.Execute(context.Background(), operations.Operation{Kind: OperationDrain, ResourceID: "trf-small-1"}); err != nil {
		t.Fatal(err)
	}
	if probe.asked != 1 || state.instance.State != operations.StateRunning {
		t.Fatalf("a guest that came back must abort the drain; state = %s asked = %d", state.instance.State, probe.asked)
	}
	if len(*vm.calls) != 0 {
		t.Fatalf("nothing may be done to a live guest; calls = %v", *vm.calls)
	}
}

// Evidence the fleet could not read is never permission to end a job. Both an
// inconclusive probe and a missing one fail the guard and retry.
func TestAPremiseThatCannotBeReVerifiedFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name  string
		build func() (DrainExecutor, *memoryState, *fakeVM)
	}{
		{name: "the probe established nothing", build: func() (DrainExecutor, *memoryState, *fakeVM) {
			executor, state, vm, _ := guestDrainFixture(domain.GuestLivenessUnknown)
			return executor, state, vm
		}},
		{name: "the node wires no probe at all", build: func() (DrainExecutor, *memoryState, *fakeVM) {
			executor, state, vm, _ := guestDrainFixture(domain.GuestLivenessRefused)
			executor.Guest = nil
			return executor, state, vm
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			executor, state, vm := test.build()
			err := executor.Execute(context.Background(), operations.Operation{Kind: OperationDrain, ResourceID: "trf-small-1"})
			if err == nil || err.Error() != safeError(StageGuard).Error() {
				t.Fatalf("error = %v, want a retryable guard failure", err)
			}
			if state.instance.State != operations.StateDraining || len(*vm.calls) != 0 {
				t.Fatalf("nothing may be done without fresh evidence; state = %s calls = %v", state.instance.State, *vm.calls)
			}
		})
	}
}

// GitHub calls the runner busy for as long as its own grace timer runs, and that
// is exactly the sixteen to eighteen minutes this reclaim exists to beat. A
// busy refusal must not abort it, or the reclaim is unreachable by construction.
func TestGitHubCallingTheDeadGuestBusyDoesNotAbortTheReclaim(t *testing.T) {
	executor, state, _, _ := guestDrainFixture(domain.GuestLivenessRefused)
	control := executor.Control.(*fakeDrainControl)
	control.busy = true
	control.deregisterErr = errors.New("runner is busy")
	err := executor.Execute(context.Background(), operations.Operation{Kind: OperationDrain, ResourceID: "trf-small-1"})
	if err == nil {
		t.Fatal("a refused deregistration must still be a failure")
	}
	if state.instance.State != operations.StateDraining {
		t.Fatalf("state = %s, want the reclaim to keep retrying rather than abort to running", state.instance.State)
	}
}

// The stop that ends the job climbs ADR 0039's ladder like every other decided
// drain. Without it the reclaim can be refused forever by the same wedged guest
// it was created to remove, which is the wall issue #233 documented.
func TestADeadGuestReclaimClimbsTheStopLadder(t *testing.T) {
	executor, state, vm, _ := guestDrainFixture(domain.GuestLivenessRefused)
	vm.stopErr = errWedgedGuest
	operation := stopFailure()
	operation.Attempts = GracefulStopAttempts
	if err := executor.Execute(context.Background(), operation); err != nil {
		t.Fatal(err)
	}
	if got := (*vm.calls)[0]; got != "terminate:trf-small-1" {
		t.Fatalf("first call = %q, want the reclaim to force the dead guest off", got)
	}
	if state.instance.State != operations.StateDeleted {
		t.Fatalf("state = %s, want the reclaim to complete", state.instance.State)
	}
}

// Deletion confirmation must not wait for a job-completion event that will never
// arrive: the machine that would have reported it stopped executing minutes ago.
func TestTheReclaimedPhasesDeriveInactivityFromRunnerAbsence(t *testing.T) {
	for phase, want := range map[int]bool{
		operations.DrainPhaseStoppedRecovery:   true,
		operations.DrainPhaseStalledAssignment: true,
		operations.DrainPhaseLingeringRunner:   true,
		operations.DrainPhaseOccupancyBudget:   true,
		operations.DrainPhaseGuestUnresponsive: true,
		operations.DrainPhaseInactiveRecovery:  false,
		1 /* the ordinary event drain */ :      false,
	} {
		if got := derivesInactivityFromRunnerAbsence(phase); got != want {
			t.Fatalf("derivesInactivityFromRunnerAbsence(%d) = %v, want %v", phase, got, want)
		}
	}
	for phase, want := range map[int]bool{
		operations.DrainPhaseOccupancyBudget:   true,
		operations.DrainPhaseGuestUnresponsive: true,
		operations.DrainPhaseLingeringRunner:   false,
		1:                                      false,
	} {
		if got := stopsItsGuestFirst(phase); got != want {
			t.Fatalf("stopsItsGuestFirst(%d) = %v, want %v", phase, got, want)
		}
	}
}
