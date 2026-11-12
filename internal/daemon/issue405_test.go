package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/sqlite"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/telemetry"
)

func TestIssue405RESTIngesterRetiresCompletedRun(t *testing.T) {
	for _, mode := range []string{"completed", "unavailable", "mixed", "active", "limited403", "limited429"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
			store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "fleet.db"), sqlite.WithClock(func() time.Time { return now }))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = store.Close() }()
			event := operations.DemandEvent{Kind: operations.DemandJobAvailable, RunnerRequestID: 1, Owner: "o", Repository: "r", WorkflowRunID: 77, JobID: "uuid", QueueTime: now.Add(-26 * time.Hour), Labels: []string{"linux"}}
			if _, err = store.ApplyDemandBatch(ctx, 1, 1, []operations.DemandEvent{event}); err != nil {
				t.Fatal(err)
			}
			if mode == "mixed" {
				event.RunnerRequestID = 2
				event.WorkflowRunID = 76
				if _, err = store.ApplyDemandBatch(ctx, 1, 2, []operations.DemandEvent{event}); err != nil {
					t.Fatal(err)
				}
			}
			completionGETs := 0
			observer, err := githubscaleset.NewObserver(githubscaleset.ObserverConfig{BaseURL: "https://api.test", Repositories: []githubscaleset.Repository{{Owner: "o", Name: "r"}}, HTTP: runtimeDoerFunc(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/76") {
					resp := runtimeResponse(`{}`)
					resp.StatusCode = 404
					return resp, nil
				}
				if strings.HasSuffix(r.URL.Path, "/77") {
					completionGETs++
					if strings.HasPrefix(mode, "limited") {
						resp := runtimeResponse(`{}`)
						resp.StatusCode = 429
						if mode == "limited403" {
							resp.StatusCode = 403
						}
						resp.Header.Set("Retry-After", "90")
						return resp, nil
					}
					if mode == "unavailable" {
						return nil, errors.New("unavailable")
					}
					return runtimeResponse(`{"id":77,"run_attempt":1,"status":"completed"}`), nil
				}
				if mode == "active" && strings.HasSuffix(r.URL.Path, "/runs") && r.URL.Query().Get("status") == "queued" {
					return runtimeResponse(fmt.Sprintf(`{"workflow_runs":[{"id":77,"run_attempt":1,"status":"queued","created_at":%q}]}`, now.Add(-26*time.Hour).Format(time.RFC3339))), nil
				}
				if strings.HasSuffix(r.URL.Path, "/jobs") {
					return runtimeResponse(`{"jobs":[]}`), nil
				}
				return runtimeResponse(`{"workflow_runs":[]}`), nil
			}), Tokens: githubscaleset.TokenSourceFunc(func(context.Context) (string, error) { return "test", nil })})
			if err != nil {
				t.Fatal(err)
			}
			health, _ := telemetry.NewHealth(wallClock{}, telemetry.HealthConfig{CriticalObservations: []string{"rest"}})
			ingester := restQueueIngester{health: health, observation: "rest", observer: observer, interval: time.Second, now: func() time.Time { return now }, coordinator: app.DemandCoordinator{Store: store, Now: func() time.Time { return now }}, bindings: []app.Binding{{ScaleSetID: 1, Profile: domain.Profile{ID: "linux", Route: "linux", Platform: domain.PlatformLinux}}}}
			_, err = ingester.IngestChanged(ctx)
			if err != nil {
				t.Fatalf("err=%v", err)
			}
			if got := health.Snapshot().Observations["rest"].Freshness; got != telemetry.ObservationFresh {
				t.Fatalf("freshness=%v", got)
			}
			if mode == "active" && completionGETs != 0 {
				t.Fatalf("active completion GETs=%d", completionGETs)
			}
			if strings.HasPrefix(mode, "limited") && ingester.next.Sub(now) != 90*time.Second {
				t.Fatalf("backoff=%v", ingester.next.Sub(now))
			}
			active, err := store.ActiveDemands(ctx, 1)
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if mode != "completed" {
				want = 1
			}
			if len(active) != want {
				t.Fatalf("active=%d want=%d", len(active), want)
			}
		})
	}
}
