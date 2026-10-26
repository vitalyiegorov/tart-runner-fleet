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

// TestIssue434SizeUnservedRouteIsForeign replays Omarchy's sale-repo scope: the
// repository's only local set advertises `trf-linux-amd64-2x4` while the
// repository queues `self-hosted, trf-linux-amd64-4x8`. Routing labels match
// exactly, so no local set can receive that job.
func TestIssue434SizeUnservedRouteIsForeign(t *testing.T) {
	now := time.Date(2026, 10, 26, 17, 27, 0, 0, time.UTC)
	binding := Binding{ScaleSetID: 5, Targets: []string{"owner/sale"},
		ScaleSetLabels: []string{"self-hosted", "trf-linux-amd64-2x4"},
		Profile:        domain.Profile{ID: "linux-2x4", Route: "linux-2x4", Platform: domain.PlatformLinux}}
	local := githubscaleset.WorkflowJob{ID: 1, RunID: 1, RunAttempt: 1,
		Repository: githubscaleset.Repository{Owner: "owner", Name: "sale"},
		Labels:     []string{"self-hosted", "trf-linux-amd64-2x4"}, Status: "queued", CreatedAt: now}
	build := local
	build.ID, build.Labels = 2, []string{"self-hosted", "trf-linux-amd64-4x8"}
	store := &fakeDemandStore{}
	c := DemandCoordinator{Store: store, StrictJobRouting: true}
	if _, err := c.ReconcileQueuedJobs(context.Background(), []Binding{binding}, fakeQueueSnapshot{at: now, jobs: []githubscaleset.WorkflowJob{build, local}}); err != nil {
		t.Fatalf("size-unserved route rejected the snapshot: %v", err)
	}
	if got := store.githubJobs[5]; len(got) != 1 || got[0].WorkflowJobID != 1 {
		t.Fatalf("local attribution changed: %+v", got)
	}
	for _, labels := range [][]string{{"self-hosted", "TRF-LINUX-AMD64-4x8"}, {"self-hosted", "linux", "trf-linux-amd64-8x16"}} {
		build.Labels = labels
		if !jobRequiresUnservedCapability([]Binding{binding}, build) {
			t.Fatalf("route %v advertised by no local set not proven foreign", labels)
		}
	}
}

// TestSizeUnservedProofFailsClosed keeps an advertised route and every unproven
// shape uncertain.
func TestSizeUnservedProofFailsClosed(t *testing.T) {
	small := Binding{ScaleSetID: 5, Targets: []string{"owner/repo"}, ScaleSetLabels: []string{"self-hosted", "trf-linux-amd64-2x4"},
		Profile: domain.Profile{ID: "linux-2x4", Route: "linux-2x4", Platform: domain.PlatformLinux}}
	large := Binding{ScaleSetID: 6, Targets: []string{"owner/repo"}, ScaleSetLabels: []string{"self-hosted", "trf-linux-amd64-4x8", "gpu"},
		Profile: domain.Profile{ID: "linux-4x8", Route: "linux-4x8", Platform: domain.PlatformLinux}}
	job := githubscaleset.WorkflowJob{ID: 3, RunID: 3, RunAttempt: 1, Repository: githubscaleset.Repository{Owner: "owner", Name: "repo"},
		Labels: []string{"self-hosted", "trf-linux-amd64-4x8"}, Status: "queued", CreatedAt: time.Date(2026, 10, 26, 18, 0, 0, 0, time.UTC)}
	if jobRequiresUnservedCapability([]Binding{small, large}, job) {
		t.Fatal("route a local set advertises treated as foreign")
	}
	for _, labels := range [][]string{
		{"self-hosted", "trf-linux-amd64-4x8", "gpu"},
		{"self-hosted", "trf-linux-amd64-4x8", "trf-linux-amd64-8x16"},
		{"self-hosted", "trf-linux-amd64-4x"},
		{"self-hosted", "linux-big"},
	} {
		job.Labels = labels
		if jobRequiresUnservedCapability([]Binding{small}, job) {
			t.Fatalf("unproven route %v treated as foreign", labels)
		}
		c := DemandCoordinator{Store: &fakeDemandStore{}, StrictJobRouting: true}
		_, err := c.ReconcileQueuedJobs(context.Background(), []Binding{small}, fakeQueueSnapshot{at: job.CreatedAt, jobs: []githubscaleset.WorkflowJob{job}})
		if !errors.Is(err, operations.ErrUncertain) {
			t.Fatalf("unproven route %v accepted: %v", labels, err)
		}
	}
	job.Labels = []string{"self-hosted", "trf-linux-amd64-4x8"}
	small.Targets = []string{"other/repo"}
	if jobRequiresUnservedCapability([]Binding{small}, job) {
		t.Fatal("unaccepted repository treated as foreign")
	}
}
