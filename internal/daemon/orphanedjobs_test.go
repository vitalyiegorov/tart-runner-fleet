package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/sqlite"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/reconcile"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scalesetaudit"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/telemetry"
)

func TestIssue520RESTIngestPublishesDoctorFindingWithoutDemand(t *testing.T) {
	for _, scenario := range []string{"old broker fresh audit", "sibling backlog", "diagnostic error", "wording", "recover observe", "recover act", "recover rate limit", "recover stale audit", "inactive unset", "inactive off", "inactive observe", "inactive act"} {
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
						created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
						if scenario == "recover stale audit" {
							created = now.Add(-20 * time.Minute)
						}
						return runtimeResponse(fmt.Sprintf(`{"jobs":[{"id":42,"name":"Plan","status":"queued","labels":["self-hosted","macos-builder"],"created_at":%q}]}`, created.Format(time.RFC3339))), nil
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
			auditor := parkedScaleSetAuditor{config: auditConfig(), key: githubscaleset.NewPrivateKeySecret("pem"), open: func(githubscaleset.GitHubAppAdminConfig) (scalesetaudit.Client, error) { return client, nil }, interval: 15 * time.Minute, health: health, now: func() time.Time {
				if scenario == "recover stale audit" {
					return now.Add(-30 * time.Minute)
				}
				return now
			}}
			if err := auditor.Ingest(ctx); err != nil {
				t.Fatal(err)
			}
			binding := app.Binding{Scope: "suuudokuuu", ScaleSetID: 1, SharedLabels: true, Profile: domain.Profile{ID: "builder", Route: "macos-builder", Platform: domain.PlatformMacOS}}
			r := restQueueIngester{coordinator: app.DemandCoordinator{Store: store, Now: func() time.Time { return now }}, bindings: []app.Binding{binding}, observer: observer, interval: time.Second, auditInterval: 15 * time.Minute, health: health, observation: "github-rest-scope", now: func() time.Time { return now }}
			if strings.HasPrefix(scenario, "recover ") {
				mode := strings.TrimPrefix(scenario, "recover ")
				api := deniedOrphanRecovery{}
				if scenario == "recover rate limit" {
					mode = "act"
					api.err = &githubscaleset.APIError{Kind: githubscaleset.Authorization, Status: 403, RetryAfter: 90 * time.Second}
				}
				if scenario == "recover stale audit" {
					mode = "act"
				}
				r.recovery = &app.OrphanRecovery{Config: config.OrphanRequeue{Mode: mode}, Store: store, API: api}
			}
			if strings.HasPrefix(scenario, "inactive ") {
				r.orphanMode = strings.TrimPrefix(scenario, "inactive ")
				if r.orphanMode == "unset" {
					r.orphanMode = ""
				}
				queued = false
			}
			var reported bytes.Buffer
			r.reporter = newFailureReporter(&reported, func() time.Time { return now })
			if scenario == "diagnostic error" {
				health.SetUndeliveredJobs(r.observation, []string{"old candidate"})
				r.coordinator.Store = &diagnosticReadErrorStore{Store: store}
			}
			if _, err = r.IngestChanged(ctx); err != nil {
				t.Fatal(err)
			}
			want := scenario == "old broker fresh audit" || scenario == "wording" || strings.HasPrefix(scenario, "recover ")
			if scenario == "inactive observe" || scenario == "inactive act" {
				want = true
			}
			findings := health.Snapshot().UndeliveredJobs
			if strings.HasPrefix(scenario, "inactive ") {
				if (len(findings) > 0) != want {
					t.Fatalf("inactive mode %q findings=%v want=%v", r.orphanMode, findings, want)
				}
				return
			}
			if (len(findings) > 0) != want {
				t.Fatalf("candidate=%v want=%v", findings, want)
			}
			if !health.Ingest().OK {
				t.Fatal("candidate must leave ingest passing")
			}
			if scenario == "recover rate limit" && r.next.Sub(now) != 90*time.Second {
				t.Fatalf("recovery ignored Retry-After: %s", r.next.Sub(now))
			}
			if scenario == "recover stale audit" && strings.Contains(strings.Join(findings, " "), "grant Actions: write") {
				t.Fatal("old pre-job audit authorized recovery")
			}
			if scenario == "recover act" && !strings.Contains(strings.Join(findings, " "), "grant Actions: write") {
				t.Fatal(findings)
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
			if got := health.Ingest(); !got.OK || (!strings.HasPrefix(scenario, "recover ") && len(got.Reasons) != 0) {
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

type deniedOrphanRecovery struct{ err error }

func (a deniedOrphanRecovery) RequeueOrphan(context.Context, githubscaleset.WorkflowJob, func(string, []string) bool, bool) (string, error) {
	if a.err != nil {
		return "", a.err
	}
	return "permission_denied", nil
}

func TestOrphanRecoveryWiringRequiresDurableActionAdapter(t *testing.T) {
	for _, supported := range []bool{false, true} {
		t.Run(fmt.Sprint(supported), func(t *testing.T) {
			path := writeMultiScopeConfig(t)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			raw = bytes.Replace(raw, []byte(`"canonicalJobInventory": true`), []byte(`"canonicalJobInventory": true, "orphanRequeue":{"mode":"observe"}`), 1)
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			d := testDependencies(t)
			d.newScaleSet = func(context.Context, githubscaleset.GitHubAppScaleSetConfig) (scaleSetSource, error) {
				return &fakeSource{}, nil
			}
			d.newRESTObserver = func(githubscaleset.ObserverConfig) (queueObserver, error) {
				if !supported {
					return &scriptedQueueObserver{}, nil
				}
				return githubscaleset.NewObserver(githubscaleset.ObserverConfig{BaseURL: "https://api.test", Tokens: githubscaleset.TokenSourceFunc(func(context.Context) (string, error) { return "test", nil }), HTTP: runtimeDoerFunc(func(*http.Request) (*http.Response, error) { return runtimeResponse(`{"workflow_runs":[]}`), nil })})
			}
			ready := make(chan struct{})
			var once sync.Once
			d.inventory = func(runtimeStore, config.Config, app.RecoveryObserver, *updateDrainController) app.Inventory {
				return notifyingInventory{ready: ready, once: &once}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- runWithDependencies(ctx, options{ConfigPath: path, DatabasePath: filepath.Join(t.TempDir(), "fleet.db"), HealthAddress: "127.0.0.1:0", Mode: reconcile.Authority}, d)
			}()
			if !supported {
				select {
				case err := <-done:
					if err == nil || !strings.Contains(err.Error(), "orphan recovery requires") {
						t.Fatalf("err=%v", err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("unsupported action adapter was accepted")
				}
				return
			}
			select {
			case <-ready:
				cancel()
			case err := <-done:
				t.Fatalf("runtime stopped early: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("runtime never became ready")
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
