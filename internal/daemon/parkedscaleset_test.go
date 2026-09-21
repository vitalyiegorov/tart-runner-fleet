package daemon

import (
	"context"
	"errors"
	"strings"
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

// TestABoundSetGitHubStoppedDeliveringForIsPublishedAfterTheBootTimeout is
// issue #336 in the daemon: node-b's own set read assigned=3 busy=3
// registered=0 for hours with an empty queue and no instance, every check
// passed, and no job was ever delivered.
//
// One reading is not the finding — a booting runner reads the same way — so the
// first audit only starts the clock, and the audit after the boot timeout
// publishes the verdict.
func TestABoundSetGitHubStoppedDeliveringForIsPublishedAfterTheBootTimeout(t *testing.T) {
	ctx := context.Background()
	client := &fakeAuditClient{listed: []githubscaleset.ScaleSetSummary{
		{ID: 1, Name: "trf-sudoku-builder", Statistics: &githubscaleset.ScaleSetStatistics{
			AssignedJobs: 3, BusyRunners: 3}},
	}}
	health, err := telemetry.NewHealth(wallClock{}, telemetry.HealthConfig{Profiles: []string{"builder"}})
	if err != nil {
		t.Fatal(err)
	}
	// The node is running and holds no instance for the profile: that is an
	// observation, published every tick, and not an absence.
	if err := health.SetInstances("builder", 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	cfg := auditConfig()
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	auditor := &parkedScaleSetAuditor{config: cfg, key: githubscaleset.NewPrivateKeySecret("pem"),
		open:     func(githubscaleset.GitHubAppAdminConfig) (scalesetaudit.Client, error) { return client, nil },
		interval: 15 * time.Minute, health: health, now: func() time.Time { return now }}

	if err := auditor.Ingest(ctx); err != nil {
		t.Fatal(err)
	}
	if rows := health.Snapshot().StrandedScaleSets; len(rows) != 0 {
		t.Fatalf("the first reading only starts the clock: %#v", rows)
	}
	if !health.Ingest().OK {
		t.Fatal("a single reading must not fail the node: a booting runner reads the same way")
	}

	now = now.Add(cfg.Timeouts.Boot + 15*time.Minute)
	if err := auditor.Ingest(ctx); err != nil {
		t.Fatal(err)
	}
	rows := health.Snapshot().StrandedScaleSets
	if len(rows) != 1 || rows[0].ScaleSetID != 1 || rows[0].Assigned != 3 || rows[0].Busy != 3 {
		t.Fatalf("the set this node serves and GitHub stopped delivering for must be published: %#v", rows)
	}
	if rows[0].Profile != "builder" || !rows[0].HoldingSince.Equal(time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("the row carries the profile and when the reading began: %#v", rows[0])
	}
	ingest := health.Ingest()
	if ingest.OK {
		t.Fatal("a set nothing is being delivered for must fail the ingest-delivery check")
	}
	if !strings.Contains(strings.Join(ingest.Reasons, " "), "recreate the set") {
		t.Fatalf("the failure must name the remedy: %v", ingest.Reasons)
	}

	// GitHub resumed delivering: the finding is a live reading, not a latch.
	client.listed[0].Statistics.RegisteredRunners = 2
	now = now.Add(15 * time.Minute)
	if err := auditor.Ingest(ctx); err != nil {
		t.Fatal(err)
	}
	if rows := health.Snapshot().StrandedScaleSets; len(rows) != 0 {
		t.Fatalf("a delivering set must clear the finding: %#v", rows)
	}
	if !health.Ingest().OK {
		t.Fatal("a cleared finding must stop failing the node")
	}
}

// A node that has not published an instance count for the profile cannot make
// the finding: an unobserved instance count is not an absent instance.
func TestAnUnobservedProfileMakesNoStrandedFinding(t *testing.T) {
	client := &fakeAuditClient{listed: []githubscaleset.ScaleSetSummary{
		{ID: 1, Name: "trf-sudoku-builder", Statistics: &githubscaleset.ScaleSetStatistics{
			AssignedJobs: 3, BusyRunners: 3}},
	}}
	health, err := telemetry.NewHealth(wallClock{}, telemetry.HealthConfig{})
	if err != nil {
		t.Fatal(err)
	}
	cfg := auditConfig()
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	auditor := &parkedScaleSetAuditor{config: cfg, key: githubscaleset.NewPrivateKeySecret("pem"),
		open:     func(githubscaleset.GitHubAppAdminConfig) (scalesetaudit.Client, error) { return client, nil },
		interval: 15 * time.Minute, health: health, now: func() time.Time { return now }}

	for _, at := range []time.Time{now, now.Add(cfg.Timeouts.Boot + 15*time.Minute)} {
		now = at
		if err := auditor.Ingest(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if rows := health.Snapshot().StrandedScaleSets; len(rows) != 0 {
		t.Fatalf("an unobserved profile makes no finding: %#v", rows)
	}
}

// TestASharedProfileLeavesItsBoundSetsUnjudged is CodeRabbit's finding on #341:
// instance counts are published per PROFILE, so when two scale sets share one
// profile an instance booted for either reads as an instance for both. That
// answer cannot support a per-set verdict, and reporting it would silently hide
// a stranding behind its sibling's healthy traffic.
//
// The node says "not observed" instead, which no surface reads as health.
func TestASharedProfileLeavesItsBoundSetsUnjudged(t *testing.T) {
	client := &fakeAuditClient{listed: []githubscaleset.ScaleSetSummary{
		{ID: 1, Name: "trf-sudoku-builder", Statistics: &githubscaleset.ScaleSetStatistics{
			AssignedJobs: 3, BusyRunners: 3}},
		{ID: 2, Name: "trf-sudoku-builder-two", Statistics: &githubscaleset.ScaleSetStatistics{
			AssignedJobs: 3, BusyRunners: 3}},
	}}
	health, err := telemetry.NewHealth(wallClock{}, telemetry.HealthConfig{Profiles: []string{"builder"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := health.SetInstances("builder", 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	cfg := auditConfig()
	cfg.GitHub.Scopes[0].ScaleSets = append(cfg.GitHub.Scopes[0].ScaleSets,
		config.ScaleSet{Profile: "builder", Name: "trf-sudoku-builder-two", ID: 2, MaxCapacity: 2})
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	auditor := &parkedScaleSetAuditor{config: cfg, key: githubscaleset.NewPrivateKeySecret("pem"),
		open:     func(githubscaleset.GitHubAppAdminConfig) (scalesetaudit.Client, error) { return client, nil },
		interval: 15 * time.Minute, health: health, now: func() time.Time { return now }}

	for _, at := range []time.Time{now, now.Add(cfg.Timeouts.Boot + 15*time.Minute)} {
		now = at
		if err := auditor.Ingest(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if rows := health.Snapshot().StrandedScaleSets; len(rows) != 0 {
		t.Fatalf("an instance count two sets share cannot judge either: %#v", rows)
	}
	if !health.Ingest().OK {
		t.Fatal("an unjudged set must not fail the node on an observation it does not have")
	}
}
