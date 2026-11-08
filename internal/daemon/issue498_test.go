package daemon

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/sqlite"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/telemetry"
)

// TestIssue498StandaloneRunnerKeepsTheKneeInventoryFresh replays the Mac mini's
// knee-repo REST observation through the real observer, coordinator and store.
// A queued WebKit Memory job waits for the standalone `macmini-webkit` runner;
// the node serves macOS through its maestro set, so before #498 the snapshot
// failed at the routing stage and the node reported a critical observation
// unavailable. A runner listing GitHub refuses keeps that fail-closed answer.
func TestIssue498StandaloneRunnerKeepsTheKneeInventoryFresh(t *testing.T) {
	repo := githubscaleset.Repository{Owner: "vitalyiegorov", Name: "knee-doctor"}
	jobs := `[{"id":101,"name":"WebKit Memory","status":"queued","created_at":"2026-11-08T17:12:45Z","labels":["self-hosted","macOS","webkit"]},` +
		`{"id":102,"name":"lint","status":"queued","created_at":"2026-11-08T17:12:45Z","labels":["self-hosted","linux-portable-2x4"]}]`
	listing := `{"runners":[` +
		`{"id":1,"name":"macmini-webkit","status":"online","busy":true,"labels":[{"name":"self-hosted"},{"name":"ARM64"},{"name":"macOS"},{"name":"webkit"}]},` +
		`{"id":2,"name":"trf-linux-2x4-42934b716d1037bd","status":"online","busy":true,"labels":[]}]}`
	cases := []struct {
		name    string
		status  int
		runners string
		fresh   bool
	}{
		{"standalone runner listed", http.StatusOK, listing, true},
		{"runner listing forbidden", http.StatusForbidden, `{"message":"Resource not accessible by integration"}`, false},
		{"no runner advertises the job", http.StatusOK, `{"runners":[{"id":2,"name":"trf-linux-2x4-42934b716d1037bd","status":"online","labels":[]}]}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Date(2026, 11, 8, 17, 13, 0, 0, time.UTC)
			store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "fleet.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			health, err := telemetry.NewHealth(wallClock{}, telemetry.HealthConfig{Profiles: []string{"macos-maestro", "linux-2x4"}, CriticalObservations: []string{"rest"}})
			if err != nil {
				t.Fatal(err)
			}
			targets := []string{"vitalyiegorov/knee-doctor"}
			bindings := []app.Binding{
				{Scope: "knee-repo", ScaleSetID: 21, Targets: targets, ScaleSetLabels: []string{"self-hosted", "macOS", "ARM64", "macos-maestro"},
					Profile: domain.Profile{ID: "macos-maestro", Route: "macos-maestro", Platform: domain.PlatformMacOS}},
				{Scope: "knee-repo", ScaleSetID: 22, Targets: targets, ScaleSetLabels: []string{"self-hosted", "linux-portable-2x4", "trf-linux-amd64-2x4"},
					Profile: domain.Profile{ID: "linux-2x4", Route: "linux-portable-2x4", Platform: domain.PlatformLinux}},
			}
			observer := oneRunObserverWithRunners(t, now, repo, "2026-11-08T17:12:45Z", jobs, tc.status, tc.runners)
			ingester := restQueueIngester{coordinator: app.DemandCoordinator{Store: store, StrictJobRouting: true}, bindings: bindings,
				observer: observer, health: health, observation: "rest", interval: time.Minute, now: func() time.Time { return now }}
			_, err = ingester.IngestChanged(ctx)
			observation := health.Snapshot().Observations["rest"]
			if !tc.fresh {
				if !errors.Is(err, operations.ErrUncertain) || app.ReconcileStage(err) != app.ReconcileStageRouting {
					t.Fatalf("unproven job accepted: %v", err)
				}
				if observation.Freshness != telemetry.ObservationUnavailable || observation.Detail != githubscaleset.ReasonQueueReconcileFailed {
					t.Fatalf("observation = %+v, want unavailable/%s", observation, githubscaleset.ReasonQueueReconcileFailed)
				}
				return
			}
			if err != nil || observation.Freshness != telemetry.ObservationFresh {
				t.Fatalf("standalone runner's job failed the knee reconcile: err=%v observation=%+v", err, observation)
			}
			for _, binding := range bindings {
				queued, err := store.QueuedGitHubJobs(ctx, binding.ScaleSetID)
				if err != nil {
					t.Fatal(err)
				}
				for _, job := range queued {
					if job.WorkflowJobID != 102 || binding.ScaleSetID != 22 {
						t.Fatalf("job %d attributed to set %d", job.WorkflowJobID, binding.ScaleSetID)
					}
				}
			}
			if queued, _ := store.QueuedGitHubJobs(ctx, 22); len(queued) != 1 {
				t.Fatalf("local Linux job lost: %+v", queued)
			}
		})
	}
}
