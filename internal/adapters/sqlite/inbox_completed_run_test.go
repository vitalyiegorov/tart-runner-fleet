package sqlite

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

func TestCompletedRunRetiresUncorroboratedAndOrphanStartedDemand(t *testing.T) {
	for _, kind := range []operations.DemandEventKind{operations.DemandJobAvailable, operations.DemandJobAssigned, operations.DemandJobStarted} {
		t.Run(string(kind), func(t *testing.T) {
			store := overdueStore(t)
			event := demandEvent(kind, 1)
			if _, err := store.ApplyDemandBatch(context.Background(), overdueScaleSet, 1, []operations.DemandEvent{event}); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			run := githubscaleset.WorkflowRun{ID: 77, Repository: githubscaleset.Repository{Owner: "owner", Name: "repo"}, Status: "completed"}
			n, err := store.ExpireCompletedRunDemands(context.Background(), overdueScaleSet, now, run)
			if err != nil || n != 1 {
				t.Fatalf("retired=%d err=%v", n, err)
			}
			active, err := store.ActiveDemands(context.Background(), overdueScaleSet)
			if err != nil || len(active) != 0 {
				t.Fatalf("active=%v err=%v", active, err)
			}
		})
	}
}

func TestCompletedRunExpiryGuards(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	run := githubscaleset.WorkflowRun{ID: 77, Repository: githubscaleset.Repository{Owner: "owner", Name: "repo"}, Status: "completed"}
	for _, live := range []bool{false, true} {
		for _, kind := range []operations.DemandEventKind{operations.DemandJobAvailable, operations.DemandJobStarted} {
			t.Run(fmt.Sprintf("live=%v/%s", live, kind), func(t *testing.T) {
				store := clockedStore(t, func() time.Time { return now })
				event := demandEvent(kind, 1)
				event.RunnerName = "runner"
				if _, err := store.ApplyDemandBatch(ctx, overdueScaleSet, 1, []operations.DemandEvent{event}); err != nil {
					t.Fatal(err)
				}
				if live {
					instance := runnerDemandInstance("runner", 1)
					seedInstanceRow(t, store, instance.ID, instance.State, now, instance.Demand, "base@1")
				}
				n, err := store.ExpireCompletedRunDemands(ctx, overdueScaleSet, now, run)
				want := int64(1)
				if live && kind == operations.DemandJobStarted {
					want = 0
				}
				if err != nil || n != want {
					t.Fatalf("retired=%d want=%d err=%v", n, want, err)
				}
				if n == 1 {
					n, err = store.ExpireCompletedRunDemands(ctx, overdueScaleSet, now, run)
					if err != nil || n != 0 {
						t.Fatalf("not idempotent: %d %v", n, err)
					}
				}
			})
		}
	}
	store := clockedStore(t, func() time.Time { return now })
	seedQueuedDemand(t, store, 1, now.Add(-time.Hour))
	for _, other := range []githubscaleset.WorkflowRun{
		{ID: 78, Repository: run.Repository, Status: "completed"},
		{ID: 77, Repository: run.Repository, Attempt: 2, Status: "completed"},
		{ID: 77, Repository: githubscaleset.Repository{Owner: "else", Name: "repo"}, Status: "completed"},
	} {
		if n, err := store.ExpireCompletedRunDemands(ctx, overdueScaleSet, now, other); err != nil || n != 0 {
			t.Fatalf("wrong identity retired: %d %v", n, err)
		}
	}
	for _, bad := range []githubscaleset.WorkflowRun{{}, {ID: 77, Repository: run.Repository, Status: "queued"}} {
		if _, err := store.ExpireCompletedRunDemands(ctx, overdueScaleSet, now, bad); !errors.Is(err, operations.ErrInvalid) {
			t.Fatalf("invalid: %v", err)
		}
	}
	if n, err := store.ExpireCompletedRunDemands(ctx, overdueScaleSet, now.Add(-time.Second), run); err != nil || n != 0 {
		t.Fatalf("newer row retired %d %v", n, err)
	}
	_ = store.Close()
	if _, err := store.ExpireCompletedRunDemands(ctx, overdueScaleSet, now, run); err == nil {
		t.Fatal("closed store")
	}
}
