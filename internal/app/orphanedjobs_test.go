package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// Replay #520: the broker never delivered a demand, and its fresh counters
// remain zero after rejoin. REST still sees the old job. Attribution correctly
// excludes it from our shared queue; doctor must not lose that evidence too.
func TestIssue520UnassignedJobSurvivesRejoinAsADoctorFinding(t *testing.T) {
	store := newShardedDemandStore()
	store.statistics[1] = freshStatistics(0, 0, 0)
	c := DemandCoordinator{Store: store, Now: func() time.Time { return restShareEpoch }}
	snapshot := fakeQueueSnapshot{at: restShareEpoch, jobs: []githubscaleset.WorkflowJob{restJob(42, 78*time.Minute)}}
	bindings := []Binding{sharedLabelBinding(1)}
	if _, err := c.ReconcileQueuedJobs(context.Background(), bindings, snapshot); err != nil {
		t.Fatal(err)
	}
	if len(store.attributed(1)) != 0 {
		t.Fatal("unassigned job must not grant local demand")
	}
	findings, err := c.UndeliveredJobs(context.Background(), bindings, snapshot, 10*time.Minute)
	if err != nil || len(findings) != 1 {
		t.Fatalf("findings=%v err=%v", findings, err)
	}
	for _, want := range []string{"owner/repo", "run 420", "job 42", "78m", "assigned=0", "gh run cancel 420", "gh run rerun 420 --failed"} {
		if !strings.Contains(findings[0], want) {
			t.Fatalf("missing %q: %s", want, findings[0])
		}
	}
}

func TestUndeliveredJobsRequireFreshZeroCountersAndNoDemand(t *testing.T) {
	for _, scenario := range []string{"young", "inexact", "future", "unmatched", "assigned", "available", "stale", "missing", "future statistics", "invalid statistics", "demand", "numeric demand", "statistics error", "demand error", "no statistics store", "disabled", "nil snapshot"} {
		t.Run(scenario, func(t *testing.T) {
			store := newShardedDemandStore()
			store.statistics[1] = freshStatistics(0, 0, 0)
			c := DemandCoordinator{Store: store, Now: func() time.Time { return restShareEpoch }}
			job := restJob(42, 78*time.Minute)
			binding := sharedLabelBinding(1)
			slo := 10 * time.Minute
			var wantErr bool
			switch scenario {
			case "young":
				job.CreatedAt = restShareEpoch
			case "inexact":
				job.QueueTimeExact = false
			case "future":
				job.CreatedAt = restShareEpoch.Add(time.Hour)
			case "unmatched":
				job.Labels = []string{"unmatched"}
			case "assigned":
				store.statistics[1] = freshStatistics(0, 1, 0)
			case "available":
				store.statistics[1] = freshStatistics(1, 0, 0)
			case "stale":
				s := store.statistics[1]
				s.ObservedAt = restShareEpoch.Add(-time.Hour)
				store.statistics[1] = s
			case "missing":
				delete(store.statistics, 1)
			case "future statistics":
				s := store.statistics[1]
				s.ObservedAt = restShareEpoch.Add(time.Hour)
				store.statistics[1] = s
			case "invalid statistics":
				s := store.statistics[1]
				s.Assigned = -1
				store.statistics[1] = s
			case "demand":
				store.records[1] = []operations.DemandRecord{{Owner: "owner", Repository: "repo", WorkflowRunID: 420, DisplayName: "Maestro"}}
			case "numeric demand":
				store.records[1] = []operations.DemandRecord{{WorkflowJobID: 42}}
			case "statistics error":
				store.statsErr = errors.New("read")
				wantErr = true
			case "demand error":
				store.recordErr = errors.New("read")
				wantErr = true
			case "no statistics store":
				c.Store = &statisticsFreeStore{inner: store}
			case "disabled":
				slo = 0
			}
			var snapshot GitHubQueueSnapshot = fakeQueueSnapshot{at: restShareEpoch, jobs: []githubscaleset.WorkflowJob{job}}
			if scenario == "nil snapshot" {
				snapshot = nil
			}
			findings, err := c.UndeliveredJobs(context.Background(), []Binding{binding}, snapshot, slo)
			if (err != nil) != wantErr || len(findings) != 0 {
				t.Fatalf("findings=%v err=%v", findings, err)
			}
		})
	}
}
