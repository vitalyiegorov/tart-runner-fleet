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

// TestIssue410SiblingAdvertisedAliasIsForeignOnASamePlatformNode replays the
// Omarchy/ubuntu-vitalii wedge: fleet-repo queues `self-hosted, linux-go-4x8`,
// an alias only the Mac ARM64 sets advertise, on an AMD64 node that serves
// Linux. The platform rule cannot prove it foreign (the node serves Linux), so
// the audited sibling's advertised labels must.
func TestIssue410SiblingAdvertisedAliasIsForeignOnASamePlatformNode(t *testing.T) {
	now := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	linux := Binding{Scope: "fleet", ScaleSetID: 3, Targets: []string{"vitalyiegorov/fleet-repo"},
		ScaleSetLabels: []string{"self-hosted", "linux-portable-4x8", "trf-linux-amd64-4x8"},
		Profile:        domain.Profile{ID: "linux-4x8", Route: "linux-portable-4x8", Platform: domain.PlatformLinux}}
	sibling := ForeignScaleSetSelector{Scope: "fleet", ID: 20, Name: "trf-mini-go",
		Labels: []string{"self-hosted", "Linux-Go-4x8", "trf-mini-go"}}
	job := githubscaleset.WorkflowJob{ID: 9, RunID: 9, RunAttempt: 1,
		Repository: githubscaleset.Repository{Owner: "vitalyiegorov", Name: "fleet-repo"},
		Labels:     []string{"self-hosted", "linux-go-4x8"}, Status: "queued", CreatedAt: now}
	reconcile := func(bindings []Binding, selectors []ForeignScaleSetSelector, labels []string) (*fakeDemandStore, error) {
		t.Helper()
		job.Labels = labels
		store := &fakeDemandStore{}
		c := DemandCoordinator{Store: store, StrictJobRouting: true, ForeignSelectors: selectors}
		_, err := c.ReconcileQueuedJobs(context.Background(), bindings, fakeQueueSnapshot{at: now, jobs: []githubscaleset.WorkflowJob{job}})
		return store, err
	}
	store, err := reconcile([]Binding{linux}, []ForeignScaleSetSelector{sibling}, []string{"self-hosted", "linux-go-4x8"})
	if err != nil {
		t.Fatalf("sibling-advertised alias rejected the snapshot: %v", err)
	}
	if len(store.githubJobs[linux.ScaleSetID]) != 0 {
		t.Fatalf("foreign job attributed locally: %+v", store.githubJobs)
	}

	uncertain := func(why string, bindings []Binding, selectors []ForeignScaleSetSelector, labels []string) {
		t.Helper()
		if _, err := reconcile(bindings, selectors, labels); !errors.Is(err, operations.ErrUncertain) {
			t.Fatalf("%s: %v accepted: %v", why, labels, err)
		}
	}
	alias := []string{"self-hosted", "linux-go-4x8"}
	unadvertised := sibling
	unadvertised.Labels = []string{"self-hosted", "macos-go-4x8", "trf-mini-go"}
	uncertain("no selector advertises the alias", []Binding{linux}, []ForeignScaleSetSelector{unadvertised}, alias)
	uncertain("no audit", []Binding{linux}, nil, alias)
	elsewhere := sibling
	elsewhere.Scope = "other"
	uncertain("sibling in another scope", []Binding{linux}, []ForeignScaleSetSelector{elsewhere}, alias)
	localID := sibling
	localID.ID = linux.ScaleSetID
	uncertain("sibling is this node's set by id", []Binding{linux}, []ForeignScaleSetSelector{localID}, alias)
	localName := sibling
	localName.Name = "trf-linux-amd64-4x8"
	uncertain("sibling is this node's set by name", []Binding{linux}, []ForeignScaleSetSelector{localName}, alias)
	uncertain("job label the sibling does not advertise", []Binding{linux}, []ForeignScaleSetSelector{sibling},
		[]string{"self-hosted", "linux-go-4x8", "gpu"})
	split := ForeignScaleSetSelector{Scope: "fleet", ID: 21, Name: "trf-gpu", Labels: []string{"self-hosted", "gpu", "trf-gpu"}}
	uncertain("labels spread across two siblings", []Binding{linux}, []ForeignScaleSetSelector{sibling, split},
		[]string{"self-hosted", "linux-go-4x8", "gpu"})
	// A bare `self-hosted` job matches the local binding before this proof is
	// consulted, so the vacuous subset is pinned on the predicate itself.
	if advertisesEveryRoutingLabel(sibling.Labels, []string{"self-hosted"}) {
		t.Fatal("a job with no routing label selected a sibling")
	}
	nameless := sibling
	nameless.ID = 0
	uncertain("selector without identity", []Binding{linux}, []ForeignScaleSetSelector{nameless}, alias)
	other := linux
	other.Targets = []string{"other/repo"}
	uncertain("repository not accepted", []Binding{other}, []ForeignScaleSetSelector{sibling}, alias)
}
