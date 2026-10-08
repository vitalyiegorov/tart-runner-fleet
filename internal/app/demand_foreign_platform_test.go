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

// Replay the 2026-10-08 MacBook canary: a complete repository queue includes
// Linux AMD64 work owned by a sibling node beside this node's macOS work.
func TestIssue356ForeignPlatformQueueDoesNotPoisonLocalInventory(t *testing.T) {
	for _, local := range []domain.Platform{domain.PlatformMacOS, domain.PlatformLinux} {
		t.Run(string(local), func(t *testing.T) {
			foreign := domain.PlatformLinux
			if local == domain.PlatformLinux {
				foreign = domain.PlatformMacOS
			}
			now := time.Date(2026, 10, 8, 12, 14, 0, 0, time.UTC)
			binding := Binding{ScaleSetID: 21, Scope: "shared", Targets: []string{"owner/repo"},
				ScaleSetLabels: []string{"self-hosted", "local"},
				Profile:        domain.Profile{ID: "local", Route: "local", Platform: local}}
			localJob := githubscaleset.WorkflowJob{ID: 1, RunID: 1, RunAttempt: 1,
				Repository: githubscaleset.Repository{Owner: "owner", Name: "repo"},
				Labels:     []string{"self-hosted", "local"}, Status: "queued", CreatedAt: now}
			remoteJob := localJob
			remoteJob.ID = 2
			remoteJob.Labels = []string{"self-hosted", "trf-" + string(foreign) + "-arm64-4x8"}
			store := &fakeDemandStore{}
			coordinator := DemandCoordinator{Store: store, StrictJobRouting: true}
			changed, err := coordinator.ReconcileQueuedJobs(context.Background(), []Binding{binding}, fakeQueueSnapshot{at: now, jobs: []githubscaleset.WorkflowJob{remoteJob, localJob}})
			if err != nil || !changed {
				t.Fatalf("mixed-platform observation rejected: changed=%v err=%v", changed, err)
			}
			if got := store.githubJobs[21]; len(got) != 1 || got[0].WorkflowJobID != 1 {
				t.Fatalf("foreign work claimed or local work lost: %+v", got)
			}
		})
	}
}

func TestForeignPlatformClassificationKeepsUnknownRoutingClosed(t *testing.T) {
	binding := Binding{ScaleSetID: 1, ScaleSetLabels: []string{"self-hosted", "local"}, Profile: domain.Profile{ID: "local", Route: "local", Platform: domain.PlatformMacOS}}
	cases := [][]string{
		{"self-hosted", "linux-unknown"},
		{"self-hosted", "trf-linux-amd64-0x8"},
		{"self-hosted", "trf-linux-amd64-4x0"},
		{"self-hosted", "trf-linux-mystery-4x8"},
		{"self-hosted", "trf-windows-amd64-4x8"},
		{"self-hosted", "trf-macos-arm64-4x8"},
		{"self-hosted", "trf-linux-amd64-4x8", "macOS"},
		{"self-hosted", "trf-linux-amd64-4x8", "trf-macos-arm64-4x7"},
		{"self-hosted", "trf-linux-amd64-4x8", "trf-linux-amd64-2x4"},
	}
	for _, labels := range cases {
		job := githubscaleset.WorkflowJob{ID: 1, RunID: 1, RunAttempt: 1, Labels: labels, Status: "queued"}
		c := DemandCoordinator{Store: &fakeDemandStore{}, StrictJobRouting: true}
		_, err := c.ReconcileQueuedJobs(context.Background(), []Binding{binding}, fakeQueueSnapshot{at: time.Now().UTC(), jobs: []githubscaleset.WorkflowJob{job}})
		if !errors.Is(err, operations.ErrUncertain) {
			t.Fatalf("unknown/contradictory route %v did not remain unavailable: %v", labels, err)
		}
	}
	// A configured platform with an unknown shape is still a routing error.
	bindings := []Binding{binding, {ScaleSetID: 2, Profile: domain.Profile{ID: "linux", Route: "linux", Platform: domain.PlatformLinux}}}
	job := githubscaleset.WorkflowJob{ID: 1, Labels: []string{"self-hosted", "trf-linux-amd64-4x8"}}
	_, err := (DemandCoordinator{Store: &fakeDemandStore{}, StrictJobRouting: true}).ReconcileQueuedJobs(context.Background(), bindings, fakeQueueSnapshot{at: time.Now().UTC(), jobs: []githubscaleset.WorkflowJob{job}})
	if !errors.Is(err, operations.ErrUncertain) {
		t.Fatalf("local Linux routing error hidden: %v", err)
	}
}
