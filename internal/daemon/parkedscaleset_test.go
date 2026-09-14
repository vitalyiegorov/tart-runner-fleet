package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scalesetaudit"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/telemetry"
)

type fakeAuditClient struct {
	listed  []githubscaleset.ScaleSetSummary
	listErr error
	lists   int
	// spend advances the test clock while GitHub is answering, so the start of an
	// audit and its completion are distinguishable instants.
	spend func()
}

func (f *fakeAuditClient) List(context.Context, string) ([]githubscaleset.ScaleSetSummary, error) {
	f.lists++
	if f.spend != nil {
		f.spend()
	}
	return f.listed, f.listErr
}

func (f *fakeAuditClient) Statistics(context.Context, int) (githubscaleset.ScaleSetStatistics, error) {
	return githubscaleset.ScaleSetStatistics{}, nil
}

func auditConfig() config.Config {
	cfg := config.Default()
	cfg.GitHub.SessionOwner = "studio"
	cfg.GitHub.App = config.GitHubApp{ClientID: "client", KeychainService: "service", KeychainAccount: "account"}
	cfg.GitHub.Installations = []config.GitHubInstallation{{Name: "personal", InstallationID: 7}}
	scope := config.GitHubScope{Name: "suuudokuuu", Kind: config.ScopeRepository, Installation: "personal"}
	scope.ConfigURL = "https://github.com/vitalyiegorov/suuudokuuu"
	scope.Targets = []string{"vitalyiegorov/suuudokuuu"}
	scope.ScaleSets = []config.ScaleSet{{Profile: "builder", Name: "trf-sudoku-builder", ID: 1, MaxCapacity: 2}}
	cfg.GitHub.Scopes = []config.GitHubScope{scope}
	return cfg
}

// TestTheAuthorityPublishesOneAuditPerCadence is the daemon half of issue #164:
// the node itself notices scale set 7 without anyone running a command, and does
// so on a slow cadence rather than on every tick.
func TestTheAuthorityPublishesOneAuditPerCadence(t *testing.T) {
	ctx := context.Background()
	client := &fakeAuditClient{listed: []githubscaleset.ScaleSetSummary{
		{ID: 1, Name: "trf-sudoku-builder", Statistics: &githubscaleset.ScaleSetStatistics{}},
		{ID: 7, Name: "trf-sudoku-builder-studio", Statistics: &githubscaleset.ScaleSetStatistics{
			AssignedJobs: 2, BusyRunners: 2}},
	}}
	health, err := telemetry.NewHealth(wallClock{}, telemetry.HealthConfig{})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 8, 4, 18, 30, 0, 0, time.UTC)
	now := started
	client.spend = func() { now = now.Add(2 * time.Second) }
	auditor := &parkedScaleSetAuditor{config: auditConfig(), key: githubscaleset.NewPrivateKeySecret("pem"),
		open:     func(githubscaleset.GitHubAppAdminConfig) (scalesetaudit.Client, error) { return client, nil },
		interval: 15 * time.Minute, health: health, now: func() time.Time { return now }}

	if err := auditor.Ingest(ctx); err != nil {
		t.Fatal(err)
	}
	snapshot := health.Snapshot()
	if len(snapshot.ParkedScaleSets) != 1 || snapshot.ParkedScaleSets[0].ScaleSetID != 7 {
		t.Fatalf("only the parked set is published: %#v", snapshot.ParkedScaleSets)
	}
	if snapshot.ParkedScaleSets[0].Assigned != 2 || snapshot.ParkedScaleSets[0].Busy != 2 ||
		!snapshot.ParkedScaleSets[0].ObservedAt.Equal(started) {
		t.Fatalf("the row carries GitHub's counts and when they were read: %#v", snapshot.ParkedScaleSets[0])
	}
	// The published timestamp is when the audit COMPLETED, which is what the
	// status document promises and what an operator reads the reading's age
	// against; the start instant would understate that age by the time GitHub
	// spent answering.
	if !snapshot.ParkedScaleSetsAuditedAt.Equal(started.Add(2 * time.Second)) {
		t.Fatalf("audited at %s, want the completion instant %s",
			snapshot.ParkedScaleSetsAuditedAt, started.Add(2*time.Second))
	}

	// A second call inside the cadence waits rather than spending another
	// listing, and a cancelled wait returns rather than auditing anyway.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := auditor.Ingest(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled wait = %v", err)
	}
	if client.lists != 1 {
		t.Fatalf("one listing per cadence: %d", client.lists)
	}
	// The cadence runs from the start of the last audit, so the time GitHub spent
	// answering does not push the next one out by its own duration.
	now = started.Add(15 * time.Minute)
	if err := auditor.Ingest(ctx); err != nil || client.lists != 2 {
		t.Fatalf("the next cadence audits again: lists=%d err=%v", client.lists, err)
	}
}

// An audit that failed publishes nothing. The last completed audit keeps
// standing and an unaudited node keeps reading "not audited": an empty
// publication would say "no set is parked" on the evidence of a failed request.
func TestAFailedAuditPublishesNothing(t *testing.T) {
	health, err := telemetry.NewHealth(wallClock{}, telemetry.HealthConfig{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 4, 18, 30, 0, 0, time.UTC)
	auditor := &parkedScaleSetAuditor{config: auditConfig(), key: githubscaleset.NewPrivateKeySecret("pem"),
		open: func(githubscaleset.GitHubAppAdminConfig) (scalesetaudit.Client, error) {
			return &fakeAuditClient{listErr: errors.New("GitHub is unreachable")}, nil
		},
		interval: 15 * time.Minute, health: health, now: func() time.Time { return now }}

	if err := auditor.Ingest(context.Background()); err == nil {
		t.Fatal("an unanswered audit must surface as a failure")
	}
	if !health.Snapshot().ParkedScaleSetsAuditedAt.IsZero() {
		t.Fatal("a failed audit must not mark the node as having audited")
	}

	if err := (*parkedScaleSetAuditor)(nil).Ingest(context.Background()); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("an unwired auditor is invalid: %v", err)
	}
}
