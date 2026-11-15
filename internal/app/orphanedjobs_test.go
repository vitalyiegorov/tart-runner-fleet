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

// Replay #520: the broker never delivered a demand; its counters are old
// after rejoin but the audit is fresh. REST still sees the old job. Attribution correctly
// excludes it from our shared queue; doctor must not lose that evidence too.
func TestIssue520UnassignedJobSurvivesRejoinAsADoctorFinding(t *testing.T) {
	store := newShardedDemandStore()
	store.statistics[1] = freshStatistics(0, 0, 0)
	c := DemandCoordinator{Store: store, AuditedAssignments: []ScaleSetAssignment{{Scope: "sudoku-repo", ID: 1, Fresh: true}}, Now: func() time.Time { return restShareEpoch }}
	snapshot := fakeQueueSnapshot{at: restShareEpoch, jobs: []githubscaleset.WorkflowJob{restJob(42, 78*time.Minute)}}
	statistics := store.statistics[1]
	statistics.ObservedAt = restShareEpoch.Add(-30 * time.Minute)
	store.statistics[1] = statistics
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
	for _, scenario := range []string{"young", "inexact", "future", "unmatched", "assigned", "stale", "missing", "invalid audit", "demand", "numeric demand", "demand error", "disabled", "nil snapshot"} {
		t.Run(scenario, func(t *testing.T) {
			store := newShardedDemandStore()
			store.statistics[1] = freshStatistics(0, 0, 0)
			c := DemandCoordinator{Store: store, AuditedAssignments: []ScaleSetAssignment{{Scope: "sudoku-repo", ID: 1, Fresh: true}}, Now: func() time.Time { return restShareEpoch }}
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
				c.AuditedAssignments[0].Assigned = 1
			case "stale":
				c.AuditedAssignments[0].Fresh = false
			case "missing":
				c.AuditedAssignments = nil
			case "invalid audit":
				c.AuditedAssignments[0].Assigned = -1
			case "demand":
				store.records[1] = []operations.DemandRecord{{Owner: "owner", Repository: "repo", WorkflowRunID: 420, DisplayName: "Maestro"}}
			case "numeric demand":
				store.records[1] = []operations.DemandRecord{{WorkflowJobID: 42}}
			case "demand error":
				store.recordErr = errors.New("read")
				wantErr = true
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

func TestUndeliveredJobsAuditDependsOnEachJobsLabels(t *testing.T) {
	store := newShardedDemandStore()
	binding := sharedLabelBinding(1)
	binding.ScaleSetLabels = []string{"self-hosted", "macos-maestro", "special"}
	first := restJob(42, 78*time.Minute)
	second := restJob(43, 78*time.Minute)
	first.Labels = []string{"self-hosted", "macos-maestro", "special"}
	c := DemandCoordinator{Store: store, Now: func() time.Time { return restShareEpoch }, AuditedAssignments: []ScaleSetAssignment{
		{Scope: "sudoku-repo", ID: 1, Fresh: true}, {Scope: "sudoku-repo", ID: 2, Name: "sibling", Labels: second.Labels, Assigned: 1, Fresh: true},
	}}
	findings, err := c.UndeliveredJobs(context.Background(), []Binding{binding}, fakeQueueSnapshot{at: restShareEpoch, jobs: []githubscaleset.WorkflowJob{first, second}}, time.Minute)
	if err != nil || len(findings) != 1 || !strings.Contains(findings[0], "job 42") {
		t.Fatalf("findings=%v err=%v", findings, err)
	}
}
