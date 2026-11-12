package app

import (
	"context"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// CompletedRunStore retracts scheduling demand only; instance lifecycle stays guarded.
type CompletedRunStore interface {
	ExpireCompletedRunDemands(context.Context, int64, time.Time, operations.RunIdentity) (int64, error)
}

// RetireCompletedRuns asks for positive evidence only for old durable demand.
// The observer bounds and rotates the batch; unknown evidence changes nothing.
func (c DemandCoordinator) RetireCompletedRuns(ctx context.Context, bindings []Binding,
	snapshot interface {
		Run(int64) (githubscaleset.WorkflowRun, bool)
	},
	observe func(context.Context, []githubscaleset.WorkflowRun) ([]githubscaleset.WorkflowRun, error),
) (int64, error) {
	store, ok := c.Store.(CompletedRunStore)
	if !ok || observe == nil {
		return 0, nil
	}
	now := c.now()
	seen := map[githubscaleset.WorkflowRun]bool{}
	var candidates []githubscaleset.WorkflowRun
	for _, b := range bindings {
		rows, err := c.Store.ActiveDemands(ctx, b.durableKey())
		if err != nil {
			return 0, err
		}
		for _, row := range rows {
			queued := row.FirstQueueTime
			if queued.IsZero() {
				queued = row.QueueTime
			}
			if queued.IsZero() || queued.After(now.Add(-defaultGhostAbsence)) {
				continue
			}
			if snapshot != nil {
				active, found := snapshot.Run(row.WorkflowRunID)
				if found && active.Repository == (githubscaleset.Repository{Owner: row.Owner, Name: row.Repository}) && (row.RunAttempt == 0 || row.RunAttempt == active.Attempt) {
					continue
				}
			}
			run := githubscaleset.WorkflowRun{ID: row.WorkflowRunID, Repository: githubscaleset.Repository{Owner: row.Owner, Name: row.Repository}, Attempt: row.RunAttempt}
			if !seen[run] {
				seen[run] = true
				candidates = append(candidates, run)
			}
		}
	}
	completed, observationErr := observe(ctx, candidates)
	var total int64
	for _, run := range completed {
		if !seen[run] {
			continue
		}
		for _, b := range bindings {
			n, err := store.ExpireCompletedRunDemands(ctx, b.durableKey(), now, operations.RunIdentity{Owner: run.Repository.Owner, Repository: run.Repository.Name, RunID: run.ID, Attempt: run.Attempt})
			if err != nil {
				return total, err
			}
			total += n
		}
	}
	return total, observationErr
}
