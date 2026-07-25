package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// RunnerActiveJob answers "is THIS runner executing a job", keyed by the runner
// name GitHub recorded on the brokered job's own demand row. The 2026-07-25
// incident turned on exactly this distinction: builder
// trf-builder-35917ac43a789b33 was spawned for request 1 (an iOS App Store
// submission, later JobCompleted) but GitHub brokered request 2 (the Android
// job) to it, and only request 2's row carries the JobStarted status that proves
// the runner is busy.
func TestRunnerActiveJobKeysBusyEvidenceOnTheRunnerName(t *testing.T) {
	const scaleSet = int64(7155)
	const runner = "trf-builder-35917ac43a789b33"
	ctx := context.Background()
	store := testStore(t)
	events := []operations.DemandEvent{
		{Kind: operations.DemandJobCompleted, RunnerRequestID: 1, Owner: "o", Repository: "suuudokuuu", WorkflowRunID: 5,
			JobID: "ios", DisplayName: "Build & Submit iOS to App Store", RunnerName: runner},
		{Kind: operations.DemandJobStarted, RunnerRequestID: 2, Owner: "o", Repository: "suuudokuuu", WorkflowRunID: 5,
			JobID: "android", DisplayName: "Build & Submit Android to Google Play", RunnerName: runner},
		{Kind: operations.DemandJobAssigned, RunnerRequestID: 3, Owner: "o", Repository: "suuudokuuu", WorkflowRunID: 5,
			JobID: "assigned", DisplayName: "Assigned elsewhere", RunnerName: "trf-builder-other"},
	}
	if _, err := store.ApplyDemandBatch(ctx, scaleSet, 1, events); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name     string
		scaleSet int64
		runner   string
		want     bool
		wantErr  error
	}{
		{name: "runner busy with a brokered job the fleet did not spawn it for", scaleSet: scaleSet, runner: runner, want: true},
		{name: "runner whose only job is assigned but never started", scaleSet: scaleSet, runner: "trf-builder-other"},
		{name: "runner with no demand rows at all", scaleSet: scaleSet, runner: "trf-builder-unknown"},
		{name: "another scale set never leaks busy evidence", scaleSet: scaleSet + 1, runner: runner},
		{name: "missing scale set is invalid", runner: runner, wantErr: operations.ErrInvalid},
		{name: "missing runner name is invalid", scaleSet: scaleSet, wantErr: operations.ErrInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			busy, err := store.RunnerActiveJob(ctx, test.scaleSet, test.runner)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("err = %v, want %v", err, test.wantErr)
			}
			if busy != test.want {
				t.Fatalf("busy = %v, want %v", busy, test.want)
			}
		})
	}
}

// An unreadable runner observation must surface as an error, never as a
// confident "idle" that would release a destructive drain.
func TestRunnerActiveJobFailsClosedWhenUnreadable(t *testing.T) {
	store := testStore(t)
	store.injectFault = func(point string) error {
		if point == "inbox.runner.active" {
			return errors.New("query unavailable")
		}
		return nil
	}
	busy, err := store.RunnerActiveJob(context.Background(), 1, "trf-small-1")
	if err == nil || busy {
		t.Fatalf("unreadable runner evidence must error, got %v, %v", busy, err)
	}
}
