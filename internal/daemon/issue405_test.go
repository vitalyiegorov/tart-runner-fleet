package daemon

import (
	"context"
	"errors"
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
)

func TestIssue405RESTIngesterRetiresCompletedRun(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(map[bool]string{false: "completed", true: "unavailable"}[unavailable], func(t *testing.T) {
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
			observer, err := githubscaleset.NewObserver(githubscaleset.ObserverConfig{BaseURL: "https://api.test", Repositories: []githubscaleset.Repository{{Owner: "o", Name: "r"}}, HTTP: runtimeDoerFunc(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/77") {
					if unavailable {
						return nil, errors.New("unavailable")
					}
					return runtimeResponse(`{"id":77,"run_attempt":1,"status":"completed"}`), nil
				}
				return runtimeResponse(`{"workflow_runs":[]}`), nil
			}), Tokens: githubscaleset.TokenSourceFunc(func(context.Context) (string, error) { return "test", nil })})
			if err != nil {
				t.Fatal(err)
			}
			ingester := restQueueIngester{observer: observer, interval: time.Second, now: func() time.Time { return now }, coordinator: app.DemandCoordinator{Store: store, Now: func() time.Time { return now }}, bindings: []app.Binding{{ScaleSetID: 1, Profile: domain.Profile{ID: "linux", Route: "linux", Platform: domain.PlatformLinux}}}}
			_, err = ingester.IngestChanged(ctx)
			if (err != nil) != unavailable {
				t.Fatalf("err=%v", err)
			}
			active, err := store.ActiveDemands(ctx, 1)
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if unavailable {
				want = 1
			}
			if len(active) != want {
				t.Fatalf("active=%d want=%d", len(active), want)
			}
		})
	}
}
