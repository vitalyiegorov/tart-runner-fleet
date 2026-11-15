package simulation_test

import (
	"context"
	"errors"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// Generate contradictory reservations and deliver a real broker event through
// the coordinator and SQLite. This is evidence of a fleet contradiction, not
// an oracle/world-model refusal: ingestion must keep the conflict loud (#165).
func TestIssue502DemandConflictRetainsDiagnosticAndNack(t *testing.T) {
	w := newWorld(t, containerNodeWorld(), simTrace{})
	defer w.close()
	ctx := context.Background()
	binding := w.cfg.Bindings[0]
	for _, id := range []string{"trf-conflict-one", "trf-conflict-two"} {
		instance := operations.Instance{ID: id, Repo: "a/repo", Platform: binding.Profile.Platform,
			Profile: binding.Profile.ID, Route: binding.Profile.Route, Resources: binding.Profile.Resources,
			Demand: domain.DemandKey{Repo: "a/repo", RunID: 71, Attempt: 1, JobID: 71}, State: operations.StateRegistering,
			Ownership: operations.Ownership{ControllerID: simOwner, ResourceID: id, OperationID: "spawn-" + id}, CreatedAt: w.now}
		if err := w.store.CreateInstance(ctx, instance); err != nil {
			t.Fatal(err)
		}
	}
	source := &projectionReplay{message: githubscaleset.Demand{MessageID: 1, Events: []githubscaleset.JobEvent{
		{Kind: githubscaleset.JobStarted, RunnerRequestID: 71, JobID: "job-71", Owner: "a", Repository: "repo", WorkflowRunID: 71, QueueTime: w.now},
	}}}
	for attempt := range 2 {
		changed, err := w.demand.IngestOnceResult(ctx, binding, source)
		failure := githubscaleset.DescribeIngestFailure(err)
		var detail *operations.DemandConflict
		if !errors.Is(err, operations.ErrConflict) || !errors.As(err, &detail) || detail.Check != "reservation_duplicate" || detail.RequestID != 71 || detail.JobID != "job-71" || detail.MessageType != operations.DemandJobStarted || detail.Instance != "trf-conflict-one" || detail.Other != "trf-conflict-two" {
			t.Fatalf("missing contradiction diagnostic: %v", err)
		}
		if failure.Reason != githubscaleset.ReasonDemandCommitConflict || failure.Phase != githubscaleset.PhaseCommit || source.acks != 0 || changed != (attempt == 0) {
			t.Fatalf("changed=%v failure=%+v acks=%d", changed, failure, source.acks)
		}
		if cursor, err := w.store.DemandCursor(ctx, binding.StoreKey); err != nil || cursor != 1 {
			t.Fatalf("cursor=%d error=%v", cursor, err)
		}
	}
}
