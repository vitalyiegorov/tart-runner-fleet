package simulation_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/sqlite"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

type selectorSnapshot struct {
	at   time.Time
	jobs []githubscaleset.WorkflowJob
}

func (s selectorSnapshot) ObservedAt() time.Time                    { return s.at }
func (s selectorSnapshot) QueuedJobs() []githubscaleset.WorkflowJob { return s.jobs }

// This world drives real reconciliation and SQLite. Earlier inventory worlds
// generated foreign architectures, but omitted same-platform peer selectors.
func TestSiblingSelectorWorldPreservesLocalWorkAcrossEvidenceLoss(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	at := time.Date(2026, 10, 16, 7, 0, 0, 0, time.UTC)
	binding := app.Binding{Scope: "scope", ScaleSetID: 1, Targets: []string{"o/r"}, ScaleSetLabels: []string{"self-hosted", "macos-builder"}, Profile: domain.Profile{ID: "builder", Route: "macos-builder", Platform: domain.PlatformMacOS}}
	local := githubscaleset.WorkflowJob{ID: 101, RunID: 1, RunAttempt: 1, Name: "local", Repository: githubscaleset.Repository{Owner: "o", Name: "r"}, Labels: []string{"self-hosted", "macos-builder"}, Status: "queued", CreatedAt: at}
	peer := local
	peer.ID = 102
	peer.Labels = []string{"self-hosted", "macos-builder", "peer-builder"}
	coordinator := app.DemandCoordinator{Store: store, StrictJobRouting: true}
	for tick, proof := range []bool{false, true, false, true, false} {
		coordinator.ForeignSelectors = nil
		if proof {
			coordinator.ForeignSelectors = []app.ForeignScaleSetSelector{{Scope: "scope", ID: 2, Name: "peer-builder"}}
		}
		_, err = coordinator.ReconcileQueuedJobs(ctx, []app.Binding{binding}, selectorSnapshot{at: at.Add(time.Duration(tick) * time.Minute), jobs: []githubscaleset.WorkflowJob{peer, local}})
		if proof && err != nil {
			t.Fatalf("tick%d fresh proof failed: %v", tick, err)
		}
		if !proof && !errors.Is(err, operations.ErrUncertain) {
			t.Fatalf("tick%d unknown proof became empty: %v", tick, err)
		}
		jobs, err := store.QueuedGitHubJobs(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		if tick == 0 {
			if len(jobs) != 0 {
				t.Fatal("failed first snapshot persisted work")
			}
			continue
		}
		if len(jobs) != 1 || jobs[0].WorkflowJobID != local.ID {
			t.Fatalf("tick%d lost local or acquired foreign work: %+v", tick, jobs)
		}
	}
}
