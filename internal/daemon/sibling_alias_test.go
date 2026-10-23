package daemon

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/sqlite"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scalesetaudit"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/telemetry"
)

// TestIssue410AuditedSiblingLabelsProveAnAliasForeign carries a parked
// sibling's advertised labels from the audit listing through the published
// health snapshot into the REST coordinator's foreign selectors, so a Linux
// node whose repository queues the Mac-only `linux-go-4x8` alias keeps its
// inventory instead of failing the whole snapshot.
func TestIssue410AuditedSiblingLabelsProveAnAliasForeign(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	advertised := []string{"self-hosted", "linux-go-4x8", "trf-mini-go"}
	client := &fakeAuditClient{listed: []githubscaleset.ScaleSetSummary{
		{ID: 1, Name: "trf-sudoku-builder", Labels: []string{"self-hosted", "linux-portable-4x8", "trf-sudoku-builder"}, Statistics: &githubscaleset.ScaleSetStatistics{}},
		{ID: 20, Name: "trf-mini-go", Labels: advertised, Statistics: &githubscaleset.ScaleSetStatistics{}},
	}}
	health, err := telemetry.NewHealth(wallClock{}, telemetry.HealthConfig{Profiles: []string{"builder"}, CriticalObservations: []string{"rest"}})
	if err != nil {
		t.Fatal(err)
	}
	auditor := &parkedScaleSetAuditor{config: auditConfig(), key: githubscaleset.NewPrivateKeySecret("pem"),
		open:     func(githubscaleset.GitHubAppAdminConfig) (scalesetaudit.Client, error) { return client, nil },
		interval: 15 * time.Minute, health: health, now: func() time.Time { return now.Add(-time.Second) }}
	if err := auditor.Ingest(ctx); err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	observer := oneRunObserver(t, now, githubscaleset.Repository{Owner: "vitalyiegorov", Name: "suuudokuuu"}, "2026-10-22T08:00:00Z",
		`[{"id":101,"name":"local","status":"queued","created_at":"2026-10-22T08:00:00Z","labels":["self-hosted","linux-portable-4x8"]},{"id":102,"name":"sibling","status":"queued","created_at":"2026-10-22T08:00:00Z","labels":["self-hosted","linux-go-4x8"]}]`)
	binding := app.Binding{Scope: "suuudokuuu", ScaleSetID: 1, Targets: []string{"vitalyiegorov/suuudokuuu"},
		ScaleSetLabels: []string{"self-hosted", "linux-portable-4x8", "trf-sudoku-builder"},
		Profile:        domain.Profile{ID: "builder", Route: "linux-portable-4x8", Platform: domain.PlatformLinux}}
	ingester := restQueueIngester{coordinator: app.DemandCoordinator{Store: store, StrictJobRouting: true}, bindings: []app.Binding{binding},
		observer: observer, health: health, observation: "rest", interval: time.Minute, auditInterval: 15 * time.Minute, now: func() time.Time { return now }}

	selectors := ingester.foreignSelectors(now)
	if len(selectors) != 1 || selectors[0].ID != 20 || !slices.Equal(selectors[0].Labels, advertised) {
		t.Fatalf("selector lost the sibling's advertised labels: %+v", selectors)
	}
	if changed, err := ingester.IngestChanged(ctx); err != nil || !changed {
		t.Fatalf("sibling-advertised alias rejected the snapshot: changed=%v err=%v", changed, err)
	}
	jobs, err := store.QueuedGitHubJobs(ctx, 1)
	if err != nil || len(jobs) != 1 || jobs[0].WorkflowJobID != 101 {
		t.Fatalf("local snapshot=%+v err=%v", jobs, err)
	}
}
