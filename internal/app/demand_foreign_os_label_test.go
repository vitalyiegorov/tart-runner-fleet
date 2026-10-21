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

// TestIssue392OSLabelledJobIsForeignToANodeWithoutThatOS replays budgie's
// maestro jobs (`self-hosted, macOS, ARM64, macos-maestro`) on a Linux-only
// canonical node, and the symmetric Linux-labelled job on a macOS-only node.
func TestIssue392OSLabelledJobIsForeignToANodeWithoutThatOS(t *testing.T) {
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	linux := Binding{ScaleSetID: 3, Targets: []string{"budgie/app"},
		ScaleSetLabels: []string{"self-hosted", "linux-portable-4x8", "trf-linux-amd64-4x8"},
		Profile:        domain.Profile{ID: "linux-4x8", Route: "linux-portable-4x8", Platform: domain.PlatformLinux}}
	macos := Binding{ScaleSetID: 12, Targets: []string{"budgie/app"},
		ScaleSetLabels: []string{"self-hosted", "macOS", "ARM64", "macos-maestro"},
		Profile:        domain.Profile{ID: "macos-4x7", Route: "macos-maestro", Platform: domain.PlatformMacOS}}
	job := githubscaleset.WorkflowJob{ID: 9, RunID: 9, RunAttempt: 1,
		Repository: githubscaleset.Repository{Owner: "budgie", Name: "app"},
		Labels:     []string{"self-hosted", "macOS", "ARM64", "macos-maestro"}, Status: "queued", CreatedAt: now}
	cases := []struct {
		name    string
		binding Binding
		labels  [][]string
	}{
		{"macOS job on Linux-only node", linux, [][]string{
			{"self-hosted", "macOS", "ARM64", "macos-maestro"},
			{"self-hosted", "MACOS", "arm64"},
			{"self-hosted", "macos", "macos-maestro", "trf-macos-arm64-4x7"},
		}},
		{"Linux job on macOS-only node", macos, [][]string{
			{"self-hosted", "Linux", "X64", "linux-builder"},
			{"self-hosted", "linux"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, labels := range tc.labels {
				job.Labels = labels
				store := &fakeDemandStore{}
				c := DemandCoordinator{Store: store, StrictJobRouting: true}
				if _, err := c.ReconcileQueuedJobs(context.Background(), []Binding{tc.binding}, fakeQueueSnapshot{at: now, jobs: []githubscaleset.WorkflowJob{job}}); err != nil {
					t.Fatalf("OS-labelled job %v rejected the snapshot: %v", labels, err)
				}
				if len(store.githubJobs[tc.binding.ScaleSetID]) != 0 {
					t.Fatalf("foreign job %v attributed locally: %+v", labels, store.githubJobs)
				}
			}
		})
	}
}

// TestOSLabelProofFailsClosed keeps every unproven OS-labelled shape uncertain.
func TestOSLabelProofFailsClosed(t *testing.T) {
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	linux := Binding{ScaleSetID: 3, Targets: []string{"budgie/app"},
		ScaleSetLabels: []string{"self-hosted", "trf-linux-amd64-4x8"},
		Profile:        domain.Profile{ID: "linux-4x8", Route: "trf-linux-amd64-4x8", Platform: domain.PlatformLinux}}
	macos := Binding{ScaleSetID: 12, Targets: []string{"budgie/app"},
		ScaleSetLabels: []string{"self-hosted", "trf-macos-arm64-4x7"},
		Profile:        domain.Profile{ID: "macos-4x7", Route: "trf-macos-arm64-4x7", Platform: domain.PlatformMacOS}}
	job := githubscaleset.WorkflowJob{ID: 9, RunID: 9, RunAttempt: 1,
		Repository: githubscaleset.Repository{Owner: "budgie", Name: "app"},
		Labels:     []string{"self-hosted", "macOS", "ARM64", "macos-maestro"}, Status: "queued", CreatedAt: now}
	uncertain := func(bindings []Binding, labels []string, why string) {
		t.Helper()
		job.Labels = labels
		if jobRequiresUnservedCapability(bindings, job) {
			t.Fatalf("%s: %v treated as foreign", why, labels)
		}
		c := DemandCoordinator{Store: &fakeDemandStore{}, StrictJobRouting: true}
		_, err := c.ReconcileQueuedJobs(context.Background(), bindings, fakeQueueSnapshot{at: now, jobs: []githubscaleset.WorkflowJob{job}})
		if !errors.Is(err, operations.ErrUncertain) {
			t.Fatalf("%s: %v accepted: %v", why, labels, err)
		}
	}
	uncertain([]Binding{linux, macos}, []string{"self-hosted", "macOS", "ARM64", "macos-maestro"}, "node serves macOS")
	uncertain([]Binding{macos}, []string{"self-hosted", "macOS", "x64"}, "node serves macOS")
	uncertain([]Binding{linux, macos}, []string{"self-hosted", "Linux", "linux-builder"}, "node serves Linux")
	for _, labels := range [][]string{
		{"self-hosted", "macOS", "Linux"},
		{"self-hosted", "Linux", "trf-macos-arm64-4x7"},
		{"self-hosted", "macOS", "trf-linux-amd64-4x8"},
		{"self-hosted", "macOS", "linux-portable-4x8"},
		{"self-hosted", "macOS", "linux-builder"},
	} {
		uncertain([]Binding{linux}, labels, "contradictory platform evidence")
		uncertain([]Binding{macos}, labels, "contradictory platform evidence")
	}
	other := linux
	other.Targets = []string{"other/repo"}
	uncertain([]Binding{other}, []string{"self-hosted", "macOS", "ARM64", "macos-maestro"}, "unaccepted repository")
	uncertain([]Binding{linux}, []string{"self-hosted", "ARM64", "macos-maestro"}, "no standard OS label")
}
