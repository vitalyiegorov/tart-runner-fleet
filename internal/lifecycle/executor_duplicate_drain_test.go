package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// demandRows is a durable demand table in miniature: it answers DemandRecord by
// request ID and RunnerActiveJob exactly as the SQLite store does — any row in
// the scale set whose status is JobStarted and whose runner is the one asked
// about. It exists so the replay below exercises the real ControlRouter rather
// than a fake that already knows the answer.
type demandRows struct {
	scaleSet int64
	rows     map[int64]operations.DemandRecord
	err      error
}

func (d demandRows) DemandRecord(_ context.Context, scaleSet, request int64) (operations.DemandRecord, error) {
	if d.err != nil {
		return operations.DemandRecord{}, d.err
	}
	record, ok := d.rows[request]
	if scaleSet != d.scaleSet || !ok {
		return operations.DemandRecord{}, operations.ErrNotFound
	}
	return record, nil
}

func (d demandRows) RunnerActiveJob(_ context.Context, scaleSet int64, runner string) (bool, error) {
	if d.err != nil {
		return false, d.err
	}
	if scaleSet != d.scaleSet {
		return false, nil
	}
	for _, record := range d.rows {
		if record.RunnerName == runner && record.Status == operations.DemandJobStarted {
			return true, nil
		}
	}
	return false, nil
}

const (
	duplicateLoser   = "trf-small-1" // lifecycleInstance's ID: the VM being drained
	duplicateSibling = "trf-small-2" // bound to the same demand; GitHub chose it
)

// duplicateDrain wires one recovery drain of the idle duplicate through the real
// ControlRouter, so the guard reads the same evidence production reads.
func duplicateDrain(phase int, demand DemandReader) (*memoryState, *fakeScaleControl, DrainExecutor) {
	now := time.Unix(1000, 0).UTC()
	state := &memoryState{instance: recoveryInstance(phase)}
	source := &fakeScaleControl{registered: true}
	router := ControlRouter{State: state, Demand: demand, Now: func() time.Time { return now },
		Sources: map[SourceKey]SourceBinding{{Repo: state.instance.Repo, Profile: state.instance.Profile}: {StoreKey: 9, Source: source}}}
	calls := []string{}
	executor := DrainExecutor{State: state, VM: fakeVM{calls: &calls, running: true}, Control: router,
		Now: func() time.Time { return now }, ConfirmationMaxAge: time.Minute,
		ConfirmationPollInterval: time.Millisecond, ConfirmationTimeout: time.Second}
	return state, source, executor
}

// TestRecoveryDrainReclaimsTheIdleDuplicateWhoseDemandStartedOnItsSibling is the
// issue #398 replay (Mac Studio, 2026-10-21).
//
// A re-run booted two VMs bound to ONE demand. GitHub brokered the job to the
// sibling, so the demand row records JobStarted (and later JobCompleted) with
// the SIBLING's runner name. The loser never got a job, stayed Assigned, and the
// scheduler planned a stalled-assignment recovery for it. The drain's guard then
// re-read the spawning demand, saw "started", and aborted to Running — so the
// idle duplicate was never reclaimed and held 4 CPU / 7 GiB until the occupancy
// budget. The lingering-runner guard has the identical shape.
//
// The guard must ask what THIS runner is doing, not what the spawning demand is
// doing. This runner is executing nothing, so the drain must deregister it.
func TestRecoveryDrainReclaimsTheIdleDuplicateWhoseDemandStartedOnItsSibling(t *testing.T) {
	for _, test := range []struct {
		name   string
		phase  int
		status operations.DemandEventKind
	}{
		{name: "stalled assignment, sibling executing", phase: operations.DrainPhaseStalledAssignment, status: operations.DemandJobStarted},
		{name: "stalled assignment, sibling finished", phase: operations.DrainPhaseStalledAssignment, status: operations.DemandJobCompleted},
		{name: "lingering runner, sibling executing", phase: operations.DrainPhaseLingeringRunner, status: operations.DemandJobStarted},
	} {
		t.Run(test.name, func(t *testing.T) {
			demand := demandRows{scaleSet: 9, rows: map[int64]operations.DemandRecord{
				41: {ScaleSetID: 9, RunnerRequestID: 41, Status: test.status, RunnerName: duplicateSibling},
			}}
			state, source, executor := duplicateDrain(test.phase, demand)
			executor.OnAbort = func(abort DrainAbort) { t.Fatalf("the idle duplicate's drain aborted: %#v", abort) }

			err := executor.Execute(context.Background(), operations.Operation{Kind: OperationDrain, ResourceID: state.instance.ID})

			if err != nil {
				t.Fatalf("drain of the idle duplicate failed: %v", err)
			}
			if source.deregister != 1 || state.instance.State != operations.StateDeleted {
				t.Fatalf("idle duplicate not reclaimed: deregistrations=%d state=%s (aborted on the sibling's job)",
					source.deregister, state.instance.State)
			}
		})
	}
}

// The other side of the same invariant: whatever demand the job came from, a
// runner that is executing one is never deregistered by these phases. The second
// case is a job GitHub brokered to this runner from a DIFFERENT demand while the
// spawning demand still reads assigned — the spawning-demand guard would have
// proceeded and only GitHub's refusal would have stopped the kill.
func TestRecoveryDrainAbortsWhenThisRunnerIsExecutingAJob(t *testing.T) {
	for _, phase := range []int{operations.DrainPhaseStalledAssignment, operations.DrainPhaseLingeringRunner} {
		for _, test := range []struct {
			name string
			rows map[int64]operations.DemandRecord
		}{
			{name: "spawning demand started on this runner", rows: map[int64]operations.DemandRecord{
				41: {ScaleSetID: 9, RunnerRequestID: 41, Status: operations.DemandJobStarted, RunnerName: duplicateLoser}}},
			{name: "brokered sibling demand started on this runner", rows: map[int64]operations.DemandRecord{
				41: {ScaleSetID: 9, RunnerRequestID: 41, Status: operations.DemandJobAssigned},
				42: {ScaleSetID: 9, RunnerRequestID: 42, Status: operations.DemandJobStarted, RunnerName: duplicateLoser}}},
		} {
			t.Run(fmt.Sprintf("phase %d, %s", phase, test.name), func(t *testing.T) {
				state, source, executor := duplicateDrain(phase, demandRows{scaleSet: 9, rows: test.rows})
				var reported []DrainAbort
				executor.OnAbort = func(abort DrainAbort) { reported = append(reported, abort) }

				err := executor.Execute(context.Background(), operations.Operation{Kind: OperationDrain, ResourceID: state.instance.ID})

				if err != nil {
					t.Fatalf("abort must acknowledge the operation as a no-op, got %v", err)
				}
				if source.deregister != 0 || state.instance.State != operations.StateRunning {
					t.Fatalf("busy runner: deregistrations=%d state=%s, want 0 and running", source.deregister, state.instance.State)
				}
				if len(reported) != 1 || reported[0].Instance.ID != duplicateLoser ||
					reported[0].Phase != DrainPhaseName(phase) || reported[0].Evidence != AbortEvidenceRunnerBusy {
					t.Fatalf("abort report = %#v, want one runner_busy report for %s", reported, duplicateLoser)
				}
			})
		}
	}
}

// Unreadable runner evidence is not permission to deregister and not evidence to
// abort on: the guard fails closed and the drain retries.
func TestRecoveryDrainFailsClosedWhenRunnerEvidenceIsUnreadable(t *testing.T) {
	for _, phase := range []int{operations.DrainPhaseStalledAssignment, operations.DrainPhaseLingeringRunner} {
		state, source, executor := duplicateDrain(phase, demandRows{scaleSet: 9, err: context.DeadlineExceeded})
		executor.OnAbort = func(abort DrainAbort) { t.Fatalf("unreadable evidence was reported as an abort: %#v", abort) }

		err := executor.Execute(context.Background(), operations.Operation{Kind: OperationDrain, ResourceID: state.instance.ID})

		var staged stageError
		if !errors.As(err, &staged) || staged.stage != StageGuard {
			t.Fatalf("phase %d: err = %v, want a guard-stage retry", phase, err)
		}
		if source.deregister != 0 || state.instance.State != operations.StateDraining {
			t.Fatalf("phase %d: deregistrations=%d state=%s, want 0 and still draining", phase, source.deregister, state.instance.State)
		}
	}
}

// Every durable phase renders to its own name, and a phase this build does not
// know says so rather than borrowing a neighbour's.
func TestDrainPhaseNameIsAClosedVocabulary(t *testing.T) {
	want := map[int]string{
		0:                                      "unknown",
		operations.DrainPhaseEvent:             "event",
		operations.DrainPhaseStoppedRecovery:   "stopped_recovery",
		operations.DrainPhaseInactiveRecovery:  "inactive_recovery",
		operations.DrainPhaseStalledAssignment: "stalled_assignment",
		operations.DrainPhaseLingeringRunner:   "lingering_runner",
		operations.DrainPhaseOccupancyBudget:   "occupancy_budget",
		operations.DrainPhaseGuestUnresponsive: "guest_unresponsive",
		operations.DrainPhaseProvisionStalled:  "provision_stalled",
		99:                                     "unknown",
	}
	for phase, name := range want {
		if got := DrainPhaseName(phase); got != name {
			t.Fatalf("DrainPhaseName(%d) = %q, want %q", phase, got, name)
		}
	}
}
