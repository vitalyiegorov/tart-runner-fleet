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

// TestSiblingPinnedJobPreservesLocalInventory replays the MacBook Budgie failure
// through the real REST observer, coordinator and durable snapshot store.
func TestSiblingPinnedJobPreservesLocalInventory(t *testing.T) {
	for _, name := range []string{"fresh", "missing", "stale", "future", "wrong-scope", "local-id", "unknown-selector", "stale-row"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "fleet.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			now := time.Date(2026, 10, 16, 7, 0, 0, 0, time.UTC)
			at := now.Add(-time.Second)
			row := telemetry.ParkedScaleSetMetric{Scope: "budgie-org", ScaleSetID: 2, Name: "trf-budgie-builder-2", ObservedAt: at}
			switch name {
			case "stale":
				at = now.Add(-3 * time.Minute)
			case "future":
				at = now.Add(time.Minute)
			case "wrong-scope":
				row.Scope = "other-org"
			case "local-id":
				row.ScaleSetID = 1
			case "unknown-selector":
				row.Name = "another-set"
			case "stale-row":
				row.ObservedAt = now.Add(-3 * time.Minute)
			}
			health, _ := telemetry.NewHealth(wallClock{}, telemetry.HealthConfig{Profiles: []string{"builder"}, CriticalObservations: []string{"rest"}})
			if name != "missing" {
				if err := health.SetParkedScaleSets([]telemetry.ParkedScaleSetMetric{row}, at); err != nil {
					t.Fatal(err)
				}
			}
			observer := oneRunObserver(t, now, githubscaleset.Repository{Owner: "budgie-at", Name: "budgie"}, "2026-10-16T04:00:00Z",
				`[{"id":101,"name":"local","status":"queued","created_at":"2026-10-16T04:00:00Z","labels":["self-hosted","macos-builder"]},{"id":102,"name":"peer","status":"queued","created_at":"2026-10-16T04:00:00Z","labels":["self-hosted","macos-builder","trf-budgie-builder-2"]}]`)
			binding := app.Binding{Scope: "budgie-org", ScaleSetID: 1, Targets: []string{"budgie-at/budgie"}, ScaleSetLabels: []string{"self-hosted", "macos-builder", "trf-budgie-builder-2-macbook"}, Profile: domain.Profile{ID: "builder", Route: "macos-builder", Platform: domain.PlatformMacOS}}
			ingester := restQueueIngester{coordinator: app.DemandCoordinator{Store: store, StrictJobRouting: true}, bindings: []app.Binding{binding}, observer: observer, health: health, observation: "rest", interval: time.Minute, now: func() time.Time { return now }}
			changed, err := ingester.IngestChanged(ctx)
			if name != "fresh" {
				if !errors.Is(err, operations.ErrUncertain) {
					t.Fatalf("unproved peer route must remain uncertain: %v", err)
				}
				if health.Snapshot().Observations["rest"].Freshness != telemetry.ObservationUnavailable {
					t.Fatal("failed routing became healthy")
				}
				return
			}
			if err != nil || !changed {
				t.Fatalf("fresh sibling-pinned snapshot rejected: changed=%v err=%v", changed, err)
			}
			jobs, err := store.QueuedGitHubJobs(ctx, 1)
			if err != nil || len(jobs) != 1 || jobs[0].WorkflowJobID != 101 {
				t.Fatalf("local snapshot=%+v err=%v", jobs, err)
			}
			if health.Snapshot().Observations["rest"].Freshness != telemetry.ObservationFresh {
				t.Fatal("local observation did not recover")
			}
		})
	}
}

type selectorClock struct{ at time.Time }

// oneRunObserver is a REST observer over one repository holding one queued run
// whose jobs are the given JSON array.
func oneRunObserver(t *testing.T, now time.Time, repo githubscaleset.Repository, createdAt, jobs string) *githubscaleset.Observer {
	t.Helper()
	base := "/repos/" + repo.Owner + "/" + repo.Name + "/actions/runs"
	observer, err := githubscaleset.NewObserver(githubscaleset.ObserverConfig{BaseURL: "https://api.test", Clock: selectorClock{now}, Repositories: []githubscaleset.Repository{repo},
		Tokens: githubscaleset.TokenSourceFunc(func(context.Context) (string, error) { return "test", nil }), HTTP: runtimeDoerFunc(func(r *http.Request) (*http.Response, error) {
			switch r.URL.Path {
			case base:
				return runtimeResponse(`{"workflow_runs":[{"id":10,"run_attempt":1,"status":"queued","created_at":"` + createdAt + `"}]}`), nil
			case base + "/10/jobs":
				return runtimeResponse(`{"jobs":` + jobs + `}`), nil
			default:
				t.Fatalf("unexpected request %s", r.URL.Path)
				return nil, nil
			}
		})})
	if err != nil {
		t.Fatal(err)
	}
	return observer
}

func (c selectorClock) Now() time.Time { return c.at }
