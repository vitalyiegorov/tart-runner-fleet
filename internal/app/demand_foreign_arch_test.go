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

// TestIssue358ForeignArchitectureInventory preserves local Linux work beside sibling architecture jobs.
func TestIssue358ForeignArchitectureInventory(t *testing.T) {
	for _, arch := range []string{"arm64", "amd64"} {
		t.Run(arch, func(t *testing.T) {
			foreign := "amd64"
			if arch == foreign {
				foreign = "arm64"
			}
			binding := Binding{ScaleSetID: 1, Targets: []string{"owner/repo"},
				ScaleSetLabels: []string{"self-hosted", "linux-medium", "trf-linux-" + arch + "-2x4"},
				Profile:        domain.Profile{ID: "linux", Route: "linux-medium", Platform: domain.PlatformLinux}}
			now := time.Date(2026, 10, 8, 14, 0, 0, 0, time.UTC)
			job := githubscaleset.WorkflowJob{ID: 1, RunID: 1, RunAttempt: 1,
				Repository: githubscaleset.Repository{Owner: "owner", Name: "repo"},
				Labels:     []string{"self-hosted", "linux-medium"}, Status: "queued", CreatedAt: now}
			remote := job
			remote.ID = 2
			remote.Labels = []string{"self-hosted", "linux", "trf-linux-" + foreign + "-4x8"}
			store := &fakeDemandStore{}
			c := DemandCoordinator{Store: store, StrictJobRouting: true}
			changed, err := c.ReconcileQueuedJobs(context.Background(), []Binding{binding}, fakeQueueSnapshot{at: now, jobs: []githubscaleset.WorkflowJob{remote, job}})
			if err != nil || !changed {
				t.Fatalf("mixed architecture snapshot rejected: changed=%v err=%v", changed, err)
			}
			if got := store.githubJobs[1]; len(got) != 1 || got[0].WorkflowJobID != 1 {
				t.Fatalf("local attribution changed: %+v", got)
			}
			for _, labels := range [][]string{
				{"self-hosted", "trf-linux-" + arch + "-4x8"},
				{"self-hosted", "trf-linux-" + foreign + "-4x8", "linux-medium"},
				{"self-hosted", "trf-linux-" + foreign + "-4x8", arch},
			} {
				remote.Labels = labels
				_, err = c.ReconcileQueuedJobs(context.Background(), []Binding{binding}, fakeQueueSnapshot{at: now, jobs: []githubscaleset.WorkflowJob{remote}})
				if !errors.Is(err, operations.ErrUncertain) {
					t.Fatalf("unproven route %v accepted: %v", labels, err)
				}
			}
		})
	}
}

// TestForeignArchitectureProofRequiresEveryLocalBinding preserves uncertainty when local capability is unknown.
func TestForeignArchitectureProofRequiresEveryLocalBinding(t *testing.T) {
	job := githubscaleset.WorkflowJob{Repository: githubscaleset.Repository{Owner: "owner", Name: "repo"},
		Labels: []string{"self-hosted", "trf-linux-amd64-4x8"}}
	binding := Binding{Targets: []string{"owner/repo"}, Profile: domain.Profile{Platform: domain.PlatformLinux}}
	for _, labels := range [][]string{nil, {"linux-medium"}, {"trf-macos-arm64-4x7"},
		{"trf-linux-arm64-2x4", "trf-linux-amd64-2x4"}} {
		binding.ScaleSetLabels = labels
		if jobRequiresUnservedCapability([]Binding{binding}, job) {
			t.Fatalf("unproven local capability %v treated as foreign", labels)
		}
	}
	binding.ScaleSetLabels = []string{"TRF-LINUX-ARM64-2x4"}
	unknown := binding
	unknown.ScaleSetLabels = nil
	if jobRequiresUnservedCapability([]Binding{binding, unknown}, job) {
		t.Fatal("known sibling binding hid unknown local architecture")
	}
	binding.Targets = []string{"other/repo"}
	if jobRequiresUnservedCapability([]Binding{binding}, job) {
		t.Fatal("unaccepted repository treated as foreign")
	}
}
