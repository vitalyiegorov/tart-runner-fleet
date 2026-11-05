package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/executor"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/telemetry"
)

type refusalRunner struct {
	output []byte
	err    error
	args   []string
}

func (r *refusalRunner) Run(_ context.Context, args ...string) ([]byte, error) {
	r.args = args
	return r.output, r.err
}

func TestATeardownReadRecordsWhatTheGuestLogSays(t *testing.T) {
	refusals := &app.RunnerRefusals{}
	instance := operations.Instance{ID: "trf-linux-2x4-abc", Platform: domain.PlatformLinux}

	runner := &refusalRunner{output: []byte("An error occurred: Runner version v2.336.0 is deprecated and cannot receive messages.\n")}
	guestRunnerRefusalReader{Runner: runner, Refusals: refusals}.ReadRunnerRefusal(context.Background(), instance)
	refusal, refused := refusals.Observe(nil)[domain.PlatformLinux]
	if !refused || refusal.Version != "2.336.0" || refusal.Instance != instance.ID {
		t.Fatalf("got %+v", refusal)
	}
	if runner.args[0] != "exec" || runner.args[1] != instance.ID {
		t.Fatalf("the read must address the guest by argument vector: %v", runner.args[:2])
	}
}

func TestAGuestThatDoesNotSaySoIsNoEvidence(t *testing.T) {
	for name, runner := range map[string]*refusalRunner{
		"no such line": {err: errors.New("exit status 1")},
		"other text":   {output: []byte("An error occurred: something else\n")},
	} {
		refusals := &app.RunnerRefusals{}
		guestRunnerRefusalReader{Runner: runner, Refusals: refusals}.ReadRunnerRefusal(context.Background(),
			operations.Instance{ID: "trf-linux-2x4-abc", Platform: domain.PlatformLinux})
		if len(refusals.Observe(nil)) != 0 {
			t.Fatalf("%s produced a verdict", name)
		}
	}
}

// A Tart node never reads its guests: each `tart exec` is vsock traffic (#478).
func TestATartNodeHasNoRefusalReader(t *testing.T) {
	cfg := config.Default()
	tartNode := dependencies{guestExec: applePlatform().guestExec, hostTraffic: applePlatform().hostTraffic}
	if reader := tartNode.runnerRefusalReader(cfg, &app.RunnerRefusals{}); reader != nil {
		t.Fatal("a Tart node must not exec into a guest for this")
	}
	containerNode := dependencies{guestExec: func(config.Config) executor.CommandRunner { return &refusalRunner{} }}
	if reader := containerNode.runnerRefusalReader(cfg, &app.RunnerRefusals{}); reader == nil {
		t.Fatal("a container node reads its guest")
	}
	if reader := (dependencies{}).runnerRefusalReader(cfg, &app.RunnerRefusals{}); reader != nil {
		t.Fatal("a node with no way to exec has no reader")
	}
}

func TestReleaseParsingKeepsStableReleasesOnly(t *testing.T) {
	releases, err := parseRunnerReleases(strings.NewReader(`[
		{"tag_name":"v2.338.2","published_at":"2026-10-26T14:45:12Z"},
		{"tag_name":"v2.338.1","published_at":"2026-10-22T14:14:46Z","prerelease":true},
		{"tag_name":"v2.339.0","published_at":"2026-11-01T00:00:00Z","draft":true},
		{"tag_name":"nightly","published_at":"2026-11-01T00:00:00Z"}]`))
	if err != nil || len(releases) != 1 || releases[0].Version != "2.338.2" {
		t.Fatalf("got %+v, %v", releases, err)
	}
	if _, err := parseRunnerReleases(strings.NewReader(`[]`)); err == nil {
		t.Fatal("an empty answer must not read as no newer release existing")
	}
	if _, err := parseRunnerReleases(strings.NewReader(`{`)); err == nil {
		t.Fatal("malformed JSON must be refused")
	}
}

func TestAFailedCalendarReadKeepsTheLastOneAndDoesNotFailTheLoop(t *testing.T) {
	now := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	calendar := []config.RunnerRelease{{Version: "2.338.2", PublishedAt: now}}
	var fetchErr error
	warned := 0
	store := &runnerReleases{}
	observer := &runnerReleaseObserver{releases: store, now: func() time.Time { return now },
		fetch: func(context.Context) ([]config.RunnerRelease, error) { return calendar, fetchErr },
		warn:  func(string, ...any) { warned++ }}
	if err := observer.Ingest(context.Background()); err != nil || len(store.load()) != 1 {
		t.Fatalf("first read: %v %v", err, store.load())
	}
	fetchErr = errors.New("rate limited")
	now = now.Add(runnerReleaseInterval)
	if err := observer.Ingest(context.Background()); err != nil {
		t.Fatalf("a failed calendar must not fail the loop: %v", err)
	}
	if warned != 1 || len(store.load()) != 1 {
		t.Fatalf("warned=%d calendar=%v", warned, store.load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := observer.Ingest(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("the wait must honour cancellation: %v", err)
	}
}

func TestTheTickRejudgesEachImage(t *testing.T) {
	cfg := config.Default()
	cfg.RunnerVersionFloor = "2.336.0"
	cfg.Linux.BaseImageRunnerVersion = "2.336.0"
	cfg.MacOS.BaseImageRunnerVersion = "2.336.0"
	cfg.MacOS.Enabled = true
	now := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	health, _ := telemetry.NewHealth(wallClock{}, telemetry.HealthConfig{})
	releases := &runnerReleases{}
	ticker := engineTicker{health: health, runnerImages: cfg, releases: releases}

	ticker.recordRunnerImages(app.TickResult{At: now})
	if result := health.RunnerVersions(); !result.OK {
		t.Fatalf("an unread calendar and no refusal must pass: %v", result.Reasons)
	}

	calendar := []config.RunnerRelease{{Version: "2.337.0", PublishedAt: now.AddDate(0, -2, 0)}}
	releases.current.Store(&calendar)
	refusal := domain.RunnerRefusal{Version: "2.336.0", Instance: "trf-linux-2x4-abc", At: now}
	ticker.recordRunnerImages(app.TickResult{At: now, RunnerRefusals: map[domain.Platform]domain.RunnerRefusal{domain.PlatformLinux: refusal}})
	reasons := strings.Join(health.RunnerVersions().Reasons, "\n")
	if !strings.Contains(reasons, "runner_version_refused") || !strings.Contains(reasons, "macOS base image") {
		t.Fatalf("the Linux image is refused and the macOS one past its window: %s", reasons)
	}
}

func TestTheTickPublishesEmptyExitBackoffs(t *testing.T) {
	now := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	health, _ := telemetry.NewHealth(wallClock{}, telemetry.HealthConfig{})
	ticker := engineTicker{health: health}
	key := domain.DemandKey{Repo: "o/r", RunID: 1, Attempt: 1, JobID: 7}
	ticker.recordEmptyExitBackoffs(app.TickResult{EmptyExitBackoffs: []app.EmptyExitBackoff{
		{Demand: key, Scope: "s", Profile: "maestro", Exits: 3, LastAt: now, RetryAt: now.Add(5 * time.Minute)}}})
	if rows := health.Snapshot().EmptyExitBackoffs; len(rows) != 1 || rows[0].Demand != key.String() {
		t.Fatalf("got %+v", rows)
	}
	ticker.recordEmptyExitBackoffs(app.TickResult{})
	if rows := health.Snapshot().EmptyExitBackoffs; len(rows) != 0 {
		t.Fatalf("a demand that left the queue stops being reported: %+v", rows)
	}
}
