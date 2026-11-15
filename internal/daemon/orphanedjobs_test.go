package daemon

import (
	"bytes"
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
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scalesetaudit"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/telemetry"
)

func TestIssue520RESTIngestPublishesDoctorFindingWithoutDemand(t *testing.T) {
	for _, scenario := range []string{"old broker fresh audit", "sibling backlog", "diagnostic error", "wording"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "fleet.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			now := time.Now().UTC()
			if _, err = store.PutDemandStatistics(ctx, 1, operations.DemandStatistics{MessageID: 1, ObservedAt: now.Add(-30 * time.Minute)}); err != nil {
				t.Fatal(err)
			}
			if scenario != "old broker fresh audit" {
				_, err = store.PutDemandStatistics(ctx, 1, operations.DemandStatistics{MessageID: 2, ObservedAt: now})
				if err != nil {
					t.Fatal(err)
				}
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
			client := &fakeAuditClient{listed: []githubscaleset.ScaleSetSummary{
				{ID: 1, Name: "local", Labels: []string{"self-hosted", "macos-builder"}, Statistics: &githubscaleset.ScaleSetStatistics{}},
				{ID: 20, Name: "sibling", Labels: []string{"self-hosted", "macos-builder"}, Statistics: &githubscaleset.ScaleSetStatistics{}},
			}}
			if scenario == "sibling backlog" {
				client.listed[1].Statistics.AssignedJobs = 1
			}
			auditor := parkedScaleSetAuditor{config: auditConfig(), key: githubscaleset.NewPrivateKeySecret("pem"), open: func(githubscaleset.GitHubAppAdminConfig) (scalesetaudit.Client, error) { return client, nil }, interval: 15 * time.Minute, health: health, now: func() time.Time { return now }}
			if err := auditor.Ingest(ctx); err != nil {
				t.Fatal(err)
			}
			binding := app.Binding{Scope: "suuudokuuu", ScaleSetID: 1, SharedLabels: true, Profile: domain.Profile{ID: "builder", Route: "macos-builder", Platform: domain.PlatformMacOS}}
			r := restQueueIngester{coordinator: app.DemandCoordinator{Store: store, Now: func() time.Time { return now }}, bindings: []app.Binding{binding}, observer: observer, interval: time.Second, auditInterval: 15 * time.Minute, health: health, observation: "github-rest-scope", now: func() time.Time { return now }}
			var reported bytes.Buffer
			r.reporter = newFailureReporter(&reported, func() time.Time { return now })
			if scenario == "diagnostic error" {
				health.SetUndeliveredJobs(r.observation, []string{"old candidate"})
				r.coordinator.Store = &diagnosticReadErrorStore{Store: store}
			}
			if _, err = r.IngestChanged(ctx); err != nil {
				t.Fatal(err)
			}
			want := scenario == "old broker fresh audit" || scenario == "wording"
			findings := health.Snapshot().UndeliveredJobs
			if (len(findings) > 0) != want {
				t.Fatalf("candidate=%v want=%v", findings, want)
			}
			if !health.Ingest().OK {
				t.Fatal("candidate must leave ingest passing")
			}
			if scenario == "wording" {
				for _, text := range []string{"candidate", "concurrency group", "sibling backlog", "only after confirming the run is not held by a concurrency group"} {
					if !strings.Contains(findings[0], text) {
						t.Fatalf("missing %q: %v", text, findings)
					}
				}
				if strings.Contains(findings[0], "(Plan)") {
					t.Fatal("unbounded job name rendered")
				}
			}
			if scenario == "diagnostic error" {
				if !strings.Contains(reported.String(), "unassigned job diagnosis unavailable") {
					t.Fatalf("missing separate diagnosis report: %s", reported.String())
				}
				if len(findings) != 0 {
					t.Fatal(findings)
				}
				return
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
			if got := health.Ingest(); !got.OK || len(got.Reasons) != 0 {
				t.Fatalf("resolved finding persisted: %v", got)
			}
		})
	}
}

type diagnosticReadErrorStore struct {
	*sqlite.Store
	committed bool
}

func (s *diagnosticReadErrorStore) ReconcileGitHubJobSnapshot(ctx context.Context, at time.Time, jobs map[int64][]operations.GitHubJobObservation) (bool, error) {
	changed, err := s.Store.ReconcileGitHubJobSnapshot(ctx, at, jobs)
	s.committed = err == nil
	return changed, err
}
func (s *diagnosticReadErrorStore) ActiveDemands(ctx context.Context, id int64) ([]operations.DemandRecord, error) {
	if s.committed {
		return nil, errors.New("diagnosis unavailable")
	}
	return s.Store.ActiveDemands(ctx, id)
}
