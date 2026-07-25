package lifecycle

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// GitHub's scale-set brokering is DECOUPLED from the fleet's demand-keyed
// spawning: the fleet spawns a VM for demand X, and GitHub may hand that runner
// a different matching job Y. "The demand this VM was spawned for completed"
// therefore does NOT imply "this runner is finished".
//
// 2026-07-25 incident: builder trf-builder-35917ac43a789b33 was spawned for the
// suuudokuuu demand "Build & Submit iOS to App Store"; GitHub assigned it
// "Build & Submit Android to Google Play" instead (durable evidence: a
// runner_demands row with status=JobStarted and runner_name set to that runner).
// When the *iOS* demand later reached JobCompleted the fleet issued an event
// drain (operation event-drain-trf-builder-35917ac43a789b33, kind=deregister,
// drain_phase=1) believing the runner's work was done. The runner was still
// mid-flight on the Android job, so GitHub refused to deregister a busy runner:
// the deregister failed 60+ attempts in an unbounded retry loop and the instance
// sat in draining for 30+ minutes. GitHub's refusal was the ONLY thing that
// stopped the fleet from killing a live App Store submission build.
//
// These tests pin both halves of the repair, extending PR #75's precedent
// (abort a drain whose premise fresh evidence disproves) to the event drain:
//   - the premise is re-verified against RUNNER-scoped evidence before any
//     destructive step, and a busy runner aborts the drain back to Running;
//   - a deregister GitHub refuses because the runner is busy is premise-
//     disproved evidence, not a transient error, so it aborts instead of
//     retrying destructively forever.

// eventDrainInstance is an instance carrying the ordinary event drain phase the
// demand-completion projection enqueues (drain_phase=1).
func eventDrainInstance() operations.Instance {
	instance := lifecycleInstance(operations.StateDraining)
	instance.DrainPhase = 1
	return instance
}

// TestEventDrainAbortsWhenRunnerIsBusy is the incident replay. The bound demand
// is complete (SafeToDeregister passes, exactly as it did in production) yet the
// runner is executing a brokered job, so the drain must abort back to Running
// without touching GitHub or Tart. Re-issuing it later, once the runner is
// genuinely idle, is the guarded lingering-runner recovery's job.
func TestEventDrainAbortsWhenRunnerIsBusy(t *testing.T) {
	for _, test := range []struct {
		name    string
		control fakeDrainControl
	}{
		{name: "brokered job active on the runner while the spawned-for demand completed",
			control: fakeDrainControl{safe: true, busy: true}},
		{name: "busy runner whose deregister GitHub would also refuse",
			control: fakeDrainControl{safe: true, busy: true, deregisterErr: errors.New("runner is busy")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := []string{}
			test.control.calls = &calls
			state := &memoryState{instance: eventDrainInstance()}
			executor := drainExecutor(state, fakeVM{calls: &calls, running: true}, &test.control)

			err := executor.Execute(context.Background(), operations.Operation{Kind: OperationDrain, ResourceID: state.instance.ID})

			if err != nil {
				t.Fatalf("abort must acknowledge the operation as a no-op, got %v", err)
			}
			if state.instance.State != operations.StateRunning {
				t.Fatalf("busy runner must roll back to running, got %s", state.instance.State)
			}
			if test.control.busyCalls == 0 {
				t.Fatal("event drain must re-verify runner-scoped job evidence before any destructive step")
			}
			if len(calls) != 0 {
				t.Fatalf("no GitHub or Tart effect may run once the runner is known busy; calls=%#v", calls)
			}
		})
	}
}

// TestEventDrainProceedsWhenRunnerIsIdle keeps the ordinary completion path
// intact: an idle runner whose demand completed is deregistered, stopped, and
// deleted exactly as before, with the runner-scoped probe added ahead of it.
func TestEventDrainProceedsWhenRunnerIsIdle(t *testing.T) {
	calls := []string{}
	now := time.Unix(1000, 0).UTC()
	confirmed := operations.DeletionConfirmation{Fresh: true, RunnerInactive: true, JobsInactive: true, ObservedAt: now}
	state := &memoryState{instance: eventDrainInstance()}
	control := &fakeDrainControl{calls: &calls, safe: true, busy: false,
		confirmations: []operations.DeletionConfirmation{confirmed, confirmed}}
	executor := drainExecutor(state, fakeVM{calls: &calls}, control)

	if err := executor.Execute(context.Background(), operations.Operation{Kind: OperationDrain, ResourceID: state.instance.ID}); err != nil {
		t.Fatal(err)
	}
	if state.instance.State != operations.StateDeleted {
		t.Fatalf("idle runner must be reclaimed, got %s", state.instance.State)
	}
	want := []string{"guard:trf-small-1", "deregister:trf-small-1", "confirm:trf-small-1", "stop:trf-small-1", "confirm:trf-small-1", "delete:trf-small-1"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls=%#v", calls)
	}
	if control.busyCalls == 0 {
		t.Fatal("the runner-scoped premise must be verified even on the happy path")
	}
}

// TestEventDrainRetriesWhenRunnerBusyEvidenceIsUnusable keeps the probe
// fail-closed: an unreadable runner observation may neither kill nor abort on a
// guess, so the drain stays draining and retries.
func TestEventDrainRetriesWhenRunnerBusyEvidenceIsUnusable(t *testing.T) {
	calls := []string{}
	state := &memoryState{instance: eventDrainInstance()}
	control := &fakeDrainControl{calls: &calls, safe: true, busyErr: context.DeadlineExceeded}
	executor := drainExecutor(state, fakeVM{calls: &calls, running: true}, control)

	err := executor.Execute(context.Background(), operations.Operation{Kind: OperationDrain, ResourceID: state.instance.ID})

	if err == nil || err.Error() != "runner lifecycle failed at drain_guard" {
		t.Fatalf("unverifiable runner premise must surface as a guard-stage retry, got %v", err)
	}
	if state.instance.State != operations.StateDraining {
		t.Fatalf("instance must stay draining pending fresh evidence, got %s", state.instance.State)
	}
	if len(calls) != 0 {
		t.Fatalf("no effect may run when the runner premise cannot be verified; calls=%#v", calls)
	}
}

// TestDrainRefusedByBusyRunnerAbortsInsteadOfRetrying pins the second half of
// the repair across every drain phase: GitHub refusing a deregister while fresh
// evidence shows the runner busy is premise-disproved, so the drain aborts. Only
// a refusal with no busy evidence is transient and keeps retrying. This is what
// bounds the 60+ attempt loop the incident produced.
func TestDrainRefusedByBusyRunnerAbortsInsteadOfRetrying(t *testing.T) {
	for _, test := range []struct {
		name      string
		phase     int
		control   fakeDrainControl
		wantState operations.State
		wantErr   string
	}{
		{name: "event drain refused while a brokered job runs", phase: 1,
			control:   fakeDrainControl{safe: true, deregisterErr: errors.New("busy"), busy: true},
			wantState: operations.StateRunning},
		{name: "stalled assignment refused while a brokered job runs", phase: operations.DrainPhaseStalledAssignment,
			control:   fakeDrainControl{jobStarted: false, deregisterErr: errors.New("busy"), busy: true},
			wantState: operations.StateRunning},
		{name: "lingering runner refused while a brokered job runs", phase: operations.DrainPhaseLingeringRunner,
			control:   fakeDrainControl{jobActive: false, deregisterErr: errors.New("busy"), busy: true},
			wantState: operations.StateRunning},
		{name: "transient refusal with an idle runner keeps retrying", phase: 1,
			control:   fakeDrainControl{safe: true, deregisterErr: errors.New("gateway timeout")},
			wantState: operations.StateDraining, wantErr: "runner lifecycle failed at deregister (deregister_failed)"},
		// A recovery phase reaches the deregister without the event drain's
		// runner-scoped pre-check, so it is the case that exercises an unreadable
		// busy observation at refusal time: it must stay a retryable deregister
		// failure rather than being guessed either way.
		{name: "refusal whose busy evidence is unreadable keeps retrying", phase: operations.DrainPhaseStalledAssignment,
			control:   fakeDrainControl{jobStarted: false, deregisterErr: errors.New("gateway timeout"), busyErr: context.DeadlineExceeded},
			wantState: operations.StateDraining, wantErr: "runner lifecycle failed at deregister (deregister_failed)"},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := []string{}
			test.control.calls = &calls
			state := &memoryState{instance: lifecycleInstance(operations.StateDraining)}
			state.instance.DrainPhase = test.phase
			executor := drainExecutor(state, fakeVM{calls: &calls, running: true}, &test.control)

			err := executor.Execute(context.Background(), operations.Operation{Kind: OperationDrain, ResourceID: state.instance.ID})

			if test.wantErr == "" && err != nil {
				t.Fatalf("premise-disproved refusal must acknowledge the operation as a no-op, got %v", err)
			}
			if test.wantErr != "" && (err == nil || err.Error() != test.wantErr) {
				t.Fatalf("transient refusal must keep retrying, got %v", err)
			}
			if state.instance.State != test.wantState {
				t.Fatalf("state = %s, want %s", state.instance.State, test.wantState)
			}
			for _, call := range calls {
				if call == "stop:"+state.instance.ID || call == "delete:"+state.instance.ID {
					t.Fatalf("a refused deregister may never reach teardown; calls=%#v", calls)
				}
			}
		})
	}
}
