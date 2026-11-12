package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

type completedDemandStore struct {
	*fakeDemandStore
	fail bool
}

func (s completedDemandStore) ExpireCompletedRunDemands(context.Context, int64, time.Time, operations.RunIdentity) (int64, error) {
	if s.fail {
		return 0, operations.ErrUncertain
	}
	return 1, nil
}
func TestCompletedRunCoordinatorFailsClosed(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	binding := []Binding{{ScaleSetID: 1}}
	observer := func(context.Context, []githubscaleset.WorkflowRun) ([]githubscaleset.WorkflowRun, error) {
		return nil, operations.ErrUncertain
	}
	if n, err := (DemandCoordinator{Store: &fakeDemandStore{}}).RetireCompletedRuns(ctx, binding, nil, observer); n != 0 || err != nil {
		t.Fatal(n, err)
	}
	store := completedDemandStore{fakeDemandStore: &fakeDemandStore{}}
	c := DemandCoordinator{Store: store, Now: func() time.Time { return now }}
	if _, err := c.RetireCompletedRuns(ctx, binding, nil, nil); err != nil {
		t.Fatal(err)
	}
	store.err = operations.ErrUncertain
	if _, err := c.RetireCompletedRuns(ctx, binding, nil, observer); !errors.Is(err, operations.ErrUncertain) {
		t.Fatal(err)
	}
	store.err = nil
	if _, err := c.RetireCompletedRuns(ctx, binding, nil, observer); !errors.Is(err, operations.ErrUncertain) {
		t.Fatal(err)
	}
	record := operations.DemandRecord{Owner: "o", Repository: "r", WorkflowRunID: 1, FirstQueueTime: now.Add(-time.Hour)}
	store.records = []operations.DemandRecord{record, record, {}, {QueueTime: now}}
	observer = func(_ context.Context, runs []githubscaleset.WorkflowRun) ([]githubscaleset.WorkflowRun, error) {
		if len(runs) != 1 {
			t.Fatalf("candidates=%v", runs)
		}
		good := runs[0]
		wrong := good
		wrong.ID = 2
		return []githubscaleset.WorkflowRun{wrong, good}, nil
	}
	if n, err := c.RetireCompletedRuns(ctx, binding, nil, observer); n != 1 || err != nil {
		t.Fatal(n, err)
	}
	store.fail = true
	c.Store = store
	if _, err := c.RetireCompletedRuns(ctx, binding, nil, observer); !errors.Is(err, operations.ErrUncertain) {
		t.Fatal(err)
	}
}

// Active snapshots suppress only the matching repository and recorded attempt.
type completionSnapshot struct{ run githubscaleset.WorkflowRun }

func (s completionSnapshot) Run(id int64) (githubscaleset.WorkflowRun, bool) {
	return s.run, s.run.ID == id
}
func TestCompletedRunCandidatesSkipActiveAttempt(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		attempt int
		owner   string
		want    int
	}{
		{"unknown", 0, "o", 0}, {"same", 2, "o", 0}, {"older", 1, "o", 1}, {"other repo", 2, "other", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := completedDemandStore{fakeDemandStore: &fakeDemandStore{records: []operations.DemandRecord{{Owner: tc.owner, Repository: "r", WorkflowRunID: 1, RunAttempt: tc.attempt, QueueTime: now.Add(-time.Hour)}}}}
			c := DemandCoordinator{Store: store, Now: func() time.Time { return now }}
			snapshot := completionSnapshot{run: githubscaleset.WorkflowRun{ID: 1, Repository: githubscaleset.Repository{Owner: "o", Name: "r"}, Attempt: 2}}
			_, err := c.RetireCompletedRuns(context.Background(), []Binding{{ScaleSetID: 1}}, snapshot, func(_ context.Context, runs []githubscaleset.WorkflowRun) ([]githubscaleset.WorkflowRun, error) {
				if len(runs) != tc.want {
					t.Fatalf("candidates=%v", runs)
				}
				return nil, nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
