package daemon

import (
	"context"
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

func TestIssue520RESTIngestPublishesDoctorFindingWithoutDemand(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	if _, err = store.PutDemandStatistics(ctx, 1, operations.DemandStatistics{MessageID: 1, ObservedAt: now}); err != nil {
		t.Fatal(err)
	}
	queued := true
	observer, err := githubscaleset.NewObserver(githubscaleset.ObserverConfig{BaseURL: "https://api.test", Repositories: []githubscaleset.Repository{{Owner: "owner", Name: "repo"}}, Tokens: githubscaleset.TokenSourceFunc(func(context.Context) (string, error) { return "test", nil }), HTTP: runtimeDoerFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/repos/owner/repo/actions/runs/420/jobs" {
			if queued {
				return runtimeResponse(`{"jobs":[{"id":42,"name":"Plan","status":"queued","labels":["self-hosted","macos-builder"],"created_at":"2026-01-01T00:00:00Z"}]}`), nil
			}
			return runtimeResponse(`{"jobs":[]}`), nil
		}
		return runtimeResponse(`{"workflow_runs":[{"id":420,"run_attempt":1,"status":"queued"}]}`), nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	health, err := telemetry.NewHealth(wallClock{}, telemetry.HealthConfig{Profiles: []string{"builder"}, CriticalObservations: []string{"github-rest-scope"}})
	if err != nil {
		t.Fatal(err)
	}
	binding := app.Binding{ScaleSetID: 1, SharedLabels: true, Profile: domain.Profile{ID: "builder", Route: "macos-builder", Platform: domain.PlatformMacOS}}
	r := restQueueIngester{coordinator: app.DemandCoordinator{Store: store, Now: func() time.Time { return now }}, bindings: []app.Binding{binding}, observer: observer, interval: time.Second, health: health, observation: "github-rest-scope", now: func() time.Time { return now }}
	if _, err = r.IngestChanged(ctx); err != nil {
		t.Fatal(err)
	}
	if health.Ingest().OK {
		t.Fatal("REST job lost before doctor")
	}
	demands, err := store.ActiveDemands(ctx, 1)
	if err != nil || len(demands) != 0 {
		t.Fatalf("diagnosis created demand: %v %v", demands, err)
	}
	queued = false
	now = now.Add(2 * time.Second)
	if _, err = r.IngestChanged(ctx); err != nil {
		t.Fatal(err)
	}
	if !health.Ingest().OK {
		t.Fatalf("resolved finding persisted: %v", health.Ingest())
	}
}
