package daemon

import (
	"bytes"
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
	for _, mode := range []string{"completed", "unavailable", "mixed", "active", "limited403", "limited429", "slow429", "scarce", "slowok"} {
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
			elapsed := time.Duration(0)
			observer, err := githubscaleset.NewObserver(githubscaleset.ObserverConfig{BaseURL: "https://api.test", Repositories: []githubscaleset.Repository{{Owner: "o", Name: "r"}}, HTTP: runtimeDoerFunc(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/76") {
					resp := runtimeResponse(`{}`)
					resp.StatusCode = 404
					return resp, nil
				}
				if strings.HasSuffix(r.URL.Path, "/77") {
					completionGETs++
					if mode == "slow429" {
						// The completion requests themselves took a minute.
						elapsed = time.Minute
						resp := runtimeResponse(`{}`)
						resp.StatusCode = 429
						resp.Header.Set("Retry-After", "90")
						return resp, nil
					}
					if mode == "slowok" {
						// A slow, healthy batch: nothing asked for more than the interval.
						elapsed = time.Minute
						return runtimeResponse(`{"id":77,"run_attempt":1,"status":"completed"}`), nil
					}
					if mode == "scarce" {
						resp := runtimeResponse(`{"id":77,"run_attempt":1,"status":"completed"}`)
						resp.Header.Set("X-RateLimit-Limit", "5000")
						resp.Header.Set("X-RateLimit-Remaining", "100")
						resp.Header.Set("X-RateLimit-Reset", fmt.Sprint(now.Add(20*time.Minute).Unix()))
						return resp, nil
					}
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
			var reported bytes.Buffer
			ingester := restQueueIngester{reporter: newFailureReporter(&reported, func() time.Time { return now }), health: health, observation: "rest", observer: observer, interval: time.Second, now: func() time.Time { return now.Add(elapsed) }, coordinator: app.DemandCoordinator{Store: store, Now: func() time.Time { return now }}, bindings: []app.Binding{{ScaleSetID: 1, Profile: domain.Profile{ID: "linux", Route: "linux", Platform: domain.PlatformLinux}}}}
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
			if mode == "slow429" && ingester.next.Sub(now) != time.Minute+90*time.Second {
				t.Fatalf("backoff must count from the failure, got %v", ingester.next.Sub(now))
			}
			if mode == "slowok" && ingester.next.Sub(now) != time.Second {
				t.Fatalf("a healthy poll must keep the refresh's own pacing, got %v", ingester.next.Sub(now))
			}
			if mode == "scarce" && ingester.next.Sub(now) != 20*time.Minute {
				t.Fatalf("completion requests that left the budget scarce must defer the next poll, got %v", ingester.next.Sub(now))
			}
			if (mode == "unavailable" || strings.HasPrefix(mode, "limited") || mode == "slow429") && reported.Len() == 0 {
				t.Fatal("completion failure not reported")
			}
			active, err := store.ActiveDemands(ctx, 1)
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if mode != "completed" && mode != "scarce" && mode != "slowok" {
				want = 1
			}
			if len(active) != want {
				t.Fatalf("active=%d want=%d", len(active), want)
			}
		})
	}
}
