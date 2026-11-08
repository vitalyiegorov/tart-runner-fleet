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

// macMiniKneeBindings is the Mac mini's knee scope on 2026-11-08: a macOS
// maestro set beside the portable Linux sets, so the macOS platform is served
// and the standard-OS exemption of #392 cannot apply.
func macMiniKneeBindings() []Binding {
	targets := []string{"vitalyiegorov/knee-doctor"}
	return []Binding{
		{Scope: "knee-repo", ScaleSetID: 21, Targets: targets,
			ScaleSetLabels: []string{"self-hosted", "macOS", "ARM64", "macos-maestro"},
			Profile:        domain.Profile{ID: "macos-maestro", Route: "macos-maestro", Platform: domain.PlatformMacOS}},
		{Scope: "knee-repo", ScaleSetID: 22, Targets: targets,
			ScaleSetLabels: []string{"self-hosted", "linux-portable-2x4", "trf-linux-amd64-2x4"},
			Profile:        domain.Profile{ID: "linux-2x4", Route: "linux-portable-2x4", Platform: domain.PlatformLinux}},
		{Scope: "knee-repo", ScaleSetID: 23, Targets: targets,
			ScaleSetLabels: []string{"self-hosted", "linux-portable-4x8", "trf-linux-amd64-4x8"},
			Profile:        domain.Profile{ID: "linux-4x8", Route: "linux-portable-4x8", Platform: domain.PlatformLinux}},
	}
}

// kneeRunners answers the repository runner listing the way GitHub did on
// 2026-11-08: the standalone `macmini-webkit` runner with its labels, and the
// fleet's scale-set runners, which the listing shows with no labels at all.
type kneeRunners struct {
	runners []githubscaleset.Runner
	err     error
	calls   int
}

func (k *kneeRunners) list(_ context.Context, repo githubscaleset.Repository) ([]githubscaleset.Runner, error) {
	k.calls++
	if k.err != nil {
		return nil, k.err
	}
	if repo != (githubscaleset.Repository{Owner: "vitalyiegorov", Name: "knee-doctor"}) {
		return nil, nil
	}
	return k.runners, nil
}

func webkitRunner() githubscaleset.Runner {
	return githubscaleset.Runner{ID: 7, Name: "macmini-webkit", Status: "online", Busy: true,
		Repository: githubscaleset.Repository{Owner: "vitalyiegorov", Name: "knee-doctor"},
		Labels:     []string{"self-hosted", "ARM64", "macOS", "webkit"}}
}

func scaleSetRunner() githubscaleset.Runner {
	return githubscaleset.Runner{ID: 8, Name: "trf-linux-2x4-42934b716d1037bd", Status: "online", Busy: true,
		Repository: githubscaleset.Repository{Owner: "vitalyiegorov", Name: "knee-doctor"}}
}

func kneeJob(id int64, now time.Time, labels ...string) githubscaleset.WorkflowJob {
	return githubscaleset.WorkflowJob{ID: id, RunID: 42503818948, RunAttempt: 1, Status: "queued", CreatedAt: now,
		Repository: githubscaleset.Repository{Owner: "vitalyiegorov", Name: "knee-doctor"}, Labels: labels}
}

// TestIssue498JobHeldByAStandaloneRunnerIsForeign replays the Mac mini's
// knee-doctor reconcile failure: a queued WebKit Memory job on
// `[self-hosted, macOS, webkit]` matched no local set, and because the node
// serves macOS the whole scope reconcile failed closed at the routing stage.
// The repository's own standalone runner advertises every label of that job,
// so it is not this node's work.
func TestIssue498JobHeldByAStandaloneRunnerIsForeign(t *testing.T) {
	now := time.Date(2026, 11, 8, 17, 12, 45, 0, time.UTC)
	bindings := macMiniKneeBindings()
	local := kneeJob(2, now, "self-hosted", "linux-portable-2x4")
	snapshot := fakeQueueSnapshot{at: now, jobs: []githubscaleset.WorkflowJob{
		kneeJob(1, now, "self-hosted", "macOS", "webkit"), local,
		kneeJob(3, now, "self-hosted", "MACOS", "WebKit"),
	}}
	runners := &kneeRunners{runners: []githubscaleset.Runner{scaleSetRunner(), webkitRunner()}}
	store := &fakeDemandStore{}
	c := DemandCoordinator{Store: store, StrictJobRouting: true, Runners: runners.list}
	if _, err := c.ReconcileQueuedJobs(context.Background(), bindings, snapshot); err != nil {
		t.Fatalf("a job the repository's standalone runner serves failed the scope reconcile: %v", err)
	}
	for _, binding := range bindings {
		for _, job := range store.githubJobs[binding.ScaleSetID] {
			if job.WorkflowJobID != local.ID {
				t.Fatalf("standalone runner's job %d attributed to set %d", job.WorkflowJobID, binding.ScaleSetID)
			}
		}
	}
	if got := store.githubJobs[22]; len(got) != 1 || got[0].WorkflowJobID != local.ID {
		t.Fatalf("local job lost while skipping the foreign one: %+v", store.githubJobs)
	}
	if runners.calls != 1 {
		t.Fatalf("runner listing called %d times in one reconcile, want once per repository", runners.calls)
	}
}

// TestStandaloneRunnerProofFailsClosed keeps every unproven shape uncertain:
// no listing, a listing GitHub refused, a label no runner advertises, an
// offline runner, and a scale-set runner, which the listing shows unlabelled.
func TestStandaloneRunnerProofFailsClosed(t *testing.T) {
	now := time.Date(2026, 11, 8, 17, 12, 45, 0, time.UTC)
	webkit := kneeJob(1, now, "self-hosted", "macOS", "webkit")
	offline := webkitRunner()
	offline.Status = "offline"
	bare := webkitRunner()
	bare.Labels = []string{"macOS", "webkit"}
	forbidden := &githubscaleset.APIError{Kind: githubscaleset.Authorization, Status: 403}
	cases := []struct {
		name    string
		job     githubscaleset.WorkflowJob
		runners *kneeRunners
	}{
		{"no runner listing wired", webkit, nil},
		{"runner listing forbidden", webkit, &kneeRunners{err: forbidden}},
		{"runner listing unavailable", webkit, &kneeRunners{err: errors.New("timeout")}},
		{"label no runner advertises", kneeJob(1, now, "self-hosted", "macOS", "webkit", "gpu"), &kneeRunners{runners: []githubscaleset.Runner{webkitRunner()}}},
		{"offline standalone runner", webkit, &kneeRunners{runners: []githubscaleset.Runner{offline}}},
		{"runner not self-hosted", webkit, &kneeRunners{runners: []githubscaleset.Runner{bare}}},
		{"scale-set runners only", webkit, &kneeRunners{runners: []githubscaleset.Runner{scaleSetRunner()}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := DemandCoordinator{Store: &fakeDemandStore{}, StrictJobRouting: true}
			if tc.runners != nil {
				c.Runners = tc.runners.list
			}
			_, err := c.ReconcileQueuedJobs(context.Background(), macMiniKneeBindings(),
				fakeQueueSnapshot{at: now, jobs: []githubscaleset.WorkflowJob{tc.job}})
			if !errors.Is(err, operations.ErrUncertain) || ReconcileStage(err) != ReconcileStageRouting {
				t.Fatalf("unproven job %v accepted: %v", tc.job.Labels, err)
			}
		})
	}
}

// TestStandaloneRunnerListingIsNotConsultedForRoutedJobs keeps the listing off
// the steady path: a job a local set matches, or one an existing exemption
// already proves foreign, never spends a GitHub request.
func TestStandaloneRunnerListingIsNotConsultedForRoutedJobs(t *testing.T) {
	now := time.Date(2026, 11, 8, 17, 12, 45, 0, time.UTC)
	runners := &kneeRunners{runners: []githubscaleset.Runner{webkitRunner()}}
	c := DemandCoordinator{Store: &fakeDemandStore{}, StrictJobRouting: true, Runners: runners.list}
	snapshot := fakeQueueSnapshot{at: now, jobs: []githubscaleset.WorkflowJob{
		kneeJob(1, now, "self-hosted", "linux-portable-2x4"),
		kneeJob(2, now, "self-hosted", "trf-linux-amd64-8x16"),
		kneeJob(3, now, "ubuntu-latest"),
	}}
	if _, err := c.ReconcileQueuedJobs(context.Background(), macMiniKneeBindings(), snapshot); err != nil {
		t.Fatal(err)
	}
	if runners.calls != 0 {
		t.Fatalf("runner listing consulted %d times for jobs that were already routed", runners.calls)
	}
}
