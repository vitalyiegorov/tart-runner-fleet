package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// TestIssue458ReconcileFailureNamesItsStage makes every reconcile failure path
// carry the closed stage that failed, so an intermittent reconcile fault on a
// node is attributable without rendering the concrete cause. Wrapping must keep
// the sentinel errors callers already match.
func TestIssue458ReconcileFailureNamesItsStage(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 1, 17, 16, 0, 0, time.UTC)
	maestro := soleBinding(9)
	ghostBinding := Binding{StoreKey: 201, ScaleSetID: 1, Targets: []string{"owner/repo"},
		Profile: domain.Profile{ID: "large", Route: "linux-large", Platform: domain.PlatformLinux}}
	unmatched := githubscaleset.WorkflowJob{ID: 7, RunID: 7, RunAttempt: 1,
		Repository: githubscaleset.Repository{Owner: "owner", Name: "repo"},
		Labels:     []string{"self-hosted", "trf-unknown-route"}, Status: "queued", CreatedAt: now}
	twice := restJob(8, time.Minute)
	overlap := soleBinding(10)
	overlap.Profile.ID = "maestro-other"
	sharded := newShardedDemandStore()
	sharded.statsErr = errors.New("statistics unreadable")
	cases := []struct {
		name     string
		c        DemandCoordinator
		bindings []Binding
		snapshot GitHubQueueSnapshot
		stage    string
		is       error
	}{
		{"an unusable snapshot is input", DemandCoordinator{Store: &fakeDemandStore{}}, []Binding{maestro}, nil,
			ReconcileStageInput, operations.ErrInvalid},
		{"an invalid binding is input", DemandCoordinator{Store: &fakeDemandStore{}}, []Binding{{}},
			fakeQueueSnapshot{at: now}, ReconcileStageInput, operations.ErrInvalid},
		{"an unrouted self-hosted job is routing", DemandCoordinator{Store: &fakeDemandStore{}, StrictJobRouting: true},
			[]Binding{maestro}, fakeQueueSnapshot{at: now, jobs: []githubscaleset.WorkflowJob{unmatched}},
			ReconcileStageRouting, operations.ErrUncertain},
		{"a job two undeclared sets claim is routing", DemandCoordinator{Store: &fakeDemandStore{}},
			[]Binding{maestro, overlap}, fakeQueueSnapshot{at: now, jobs: []githubscaleset.WorkflowJob{twice}},
			ReconcileStageRouting, operations.ErrConflict},
		{"an unreadable share bound is share", DemandCoordinator{Store: sharded, Now: func() time.Time { return restShareEpoch }},
			[]Binding{sharedLabelBinding(1)}, fakeQueueSnapshot{at: restShareEpoch, jobs: []githubscaleset.WorkflowJob{restJob(801, time.Minute)}},
			ReconcileStageShare, nil},
		{"a refused snapshot write is commit", DemandCoordinator{Store: &fakeDemandStore{err: errors.New("database is locked")}},
			[]Binding{maestro}, fakeQueueSnapshot{at: now}, ReconcileStageCommit, nil},
		{"a failed ghost expiry is expiry", DemandCoordinator{Store: &ghostDemandStore{fakeDemandStore: &fakeDemandStore{}, expireErr: errors.New("expire failed")}},
			[]Binding{ghostBinding}, fakeQueueSnapshot{at: now}, ReconcileStageExpiry, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.c.ReconcileQueuedJobs(ctx, tc.bindings, tc.snapshot)
			if err == nil {
				t.Fatal("reconcile must fail")
			}
			if got := ReconcileStage(err); got != tc.stage || !ValidReconcileStage(got) {
				t.Fatalf("stage = %q (err=%v), want %q", got, err, tc.stage)
			}
			if tc.is != nil && !errors.Is(err, tc.is) {
				t.Fatalf("err = %v lost %v", err, tc.is)
			}
		})
	}
	if ReconcileStage(errors.New("plain")) != "" || ReconcileStage(nil) != "" || ValidReconcileStage("database is locked") {
		t.Fatal("an unstaged error must name no stage, and only the closed vocabulary is valid")
	}
	if reconcileStage(ReconcileStageCommit, nil) != nil {
		t.Fatal("success must stay nil")
	}
}
