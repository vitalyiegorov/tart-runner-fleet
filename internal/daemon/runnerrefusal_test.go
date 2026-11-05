package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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

type noJobEvidence struct{}

func (noJobEvidence) InstanceTookJob(context.Context, string) (bool, error) { return false, nil }

// The production read addresses the guest by argument vector, refuses a name the
// grammar does not admit, and feeds the shared gate.
func TestATeardownReadRunsTheFixedCommandAgainstTheGuest(t *testing.T) {
	refusals := &app.RunnerRefusals{}
	runner := &refusalRunner{output: []byte("An error occurred: Runner version v2.336.0 is deprecated and cannot receive messages.\n")}
	node := dependencies{guestExec: func(config.Config) executor.CommandRunner { return runner }}
	reader := node.runnerRefusalReader(config.Default(), noJobEvidence{}, refusals)
	for _, id := range []string{"trf-linux-2x4-abc", "trf-linux-2x4-def"} {
		reader.ReadRunnerRefusal(context.Background(), operations.Instance{ID: id, Platform: domain.PlatformLinux})
	}
	if refusals.Observe(nil)[domain.PlatformLinux].Version != "2.336.0" {
		t.Fatalf("two fresh guests make a verdict: %+v", refusals.Observe(nil))
	}
	if runner.args[0] != "exec" || runner.args[1] != "trf-linux-2x4-def" {
		t.Fatalf("got %v", runner.args[:2])
	}
	runner.args = nil
	reader.ReadRunnerRefusal(context.Background(), operations.Instance{ID: "bad name; rm -rf", Platform: domain.PlatformLinux})
	if runner.args != nil {
		t.Fatalf("executed %v", runner.args)
	}
}

// The identity moves with either thing a rebuild changes.
func TestAnImageGenerationMovesWithTheReferenceAndTheDeclaredVersion(t *testing.T) {
	cfg := config.Default()
	cfg.MacOS.BaseVM, cfg.MacOS.BaseImageRunnerVersion = "macos-base", "2.336.0"
	cfg.Linux.BaseImageRunnerVersion = "2.336.0"
	node := dependencies{linuxImage: func(config.Config) string { return "registry/runner:1" }}
	first := node.imageGenerations(cfg)
	if first[domain.PlatformLinux] != "registry/runner:1@2.336.0" || first[domain.PlatformMacOS] != "macos-base@2.336.0" {
		t.Fatalf("got %+v", first)
	}
	cfg.MacOS.BaseImageRunnerVersion = "2.338.2"
	moved := node.imageGenerations(cfg)
	if moved[domain.PlatformMacOS] == first[domain.PlatformMacOS] || moved[domain.PlatformLinux] != first[domain.PlatformLinux] {
		t.Fatalf("got %+v", moved)
	}
	node.linuxImage = func(config.Config) string { return "registry/runner:2" }
	if node.imageGenerations(cfg)[domain.PlatformLinux] == first[domain.PlatformLinux] {
		t.Fatal("a new reference is a new generation")
	}
}

// A Tart node never reads its guests: each `tart exec` is vsock traffic (#478).
func TestATartNodeHasNoRefusalReader(t *testing.T) {
	cfg := config.Default()
	tartNode := dependencies{guestExec: applePlatform().guestExec, hostTraffic: applePlatform().hostTraffic}
	if reader := tartNode.runnerRefusalReader(cfg, noJobEvidence{}, &app.RunnerRefusals{}); reader != nil {
		t.Fatal("a Tart node must not exec into a guest for this")
	}
	containerNode := dependencies{guestExec: func(config.Config) executor.CommandRunner { return &refusalRunner{} }}
	if reader := containerNode.runnerRefusalReader(cfg, noJobEvidence{}, &app.RunnerRefusals{}); reader == nil {
		t.Fatal("a container node reads its guest")
	}
	if reader := (dependencies{}).runnerRefusalReader(cfg, noJobEvidence{}, &app.RunnerRefusals{}); reader != nil {
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

func TestReleasesAreFetchedAndAFailedAnswerIsAnError(t *testing.T) {
	status, body := http.StatusOK, `[{"tag_name":"v2.338.2","published_at":"2026-10-26T14:45:12Z"}]`
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(status)
		_, _ = writer.Write([]byte(body))
	}))
	defer server.Close()
	releases, err := fetchRunnerReleasesFrom(context.Background(), server.URL)
	if err != nil || len(releases) != 1 || releases[0].Version != "2.338.2" {
		t.Fatalf("got %+v, %v", releases, err)
	}
	status = http.StatusForbidden
	if _, err := fetchRunnerReleasesFrom(context.Background(), server.URL); err == nil {
		t.Fatal("a refused answer must not read as a calendar")
	}
	server.Close()
	if _, err := fetchRunnerReleasesFrom(context.Background(), server.URL); err == nil {
		t.Fatal("an unreachable calendar must be an error")
	}
	if _, err := fetchRunnerReleasesFrom(context.Background(), "http://bad host/"); err == nil {
		t.Fatal("an invalid URL must be an error")
	}
}

func TestANilReleaseStoreIsAnUnreadCalendar(t *testing.T) {
	var releases *runnerReleases
	if releases.load() != nil {
		t.Fatal("no store has no calendar")
	}
}

func TestAContainerNodeWhoseConfigRunsNoContainersHasNoReader(t *testing.T) {
	node := dependencies{guestExec: func(config.Config) executor.CommandRunner { return nil }}
	if node.runnerRefusalReader(config.Default(), noJobEvidence{}, &app.RunnerRefusals{}) != nil {
		t.Fatal("no runner, no reader")
	}
}

// The verdicts are said once per read and once per attempt, and a tick that cannot
// publish says so rather than leaving the last answer standing silently.
func TestTheTickSaysEachVerdictOnce(t *testing.T) {
	now := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	var logged strings.Builder
	reporter := newFailureReporter(&logged, func() time.Time { return now })
	health, _ := telemetry.NewHealth(wallClock{}, telemetry.HealthConfig{})
	cfg := config.Default()
	cfg.Linux.BaseVM = "base"
	cfg.Linux.BaseImageRunnerVersion = "2.336.0"
	ticker := engineTicker{health: health, runnerImages: cfg, reporter: reporter}
	key := domain.DemandKey{Repo: "o/r", RunID: 1, Attempt: 1, JobID: 7}
	refusal := domain.RunnerRefusal{Version: "2.336.0", Instance: "trf-linux-2x4-abc", At: now}
	result := app.TickResult{At: now, RunnerRefusals: map[domain.Platform]domain.RunnerRefusal{domain.PlatformLinux: refusal},
		EmptyExitBackoffs: []app.EmptyExitBackoff{{Demand: key, Exits: 3, LastAt: now, RetryAt: now.Add(5 * time.Minute)}}}
	for range 2 {
		ticker.recordRunnerImages(result)
		ticker.recordEmptyExitBackoffs(result)
	}
	output := logged.String()
	if strings.Count(output, "runner image refused by GitHub") != 1 || strings.Count(output, "demand_empty_exit_backoff") != 1 {
		t.Fatalf("got %q", output)
	}

	ticker.runnerImages.Linux.BaseVM = "not a valid name!"
	ticker.recordRunnerImages(app.TickResult{At: now})
	if !strings.Contains(logged.String(), "runner image verdicts were refused") {
		t.Fatalf("got %q", logged.String())
	}
}

func TestAReporterWithNoLoggerStaysSilent(t *testing.T) {
	(&failureReporter{}).reportRunnerRefusal("linux", "base", domain.RunnerRefusal{})
	(&failureReporter{}).reportEmptyExitBackoffs([]app.EmptyExitBackoff{{}})
}

// A hold lasts up to half an hour and the reporter's rate limit one minute, so "once"
// has to be stated by the thing that changed, not by a window. A new reading or a new
// exit is the only news.
func TestAStandingVerdictAndHoldAreSaidOnceHoweverLongTheyLast(t *testing.T) {
	now := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	clock := now
	var logged strings.Builder
	reporter := newFailureReporter(&logged, func() time.Time { return clock })
	health, _ := telemetry.NewHealth(wallClock{}, telemetry.HealthConfig{})
	cfg := config.Default()
	cfg.Linux.BaseVM = "base"
	cfg.Linux.BaseImageRunnerVersion = "2.336.0"
	ticker := engineTicker{health: health, runnerImages: cfg, reporter: reporter}
	key := domain.DemandKey{Repo: "o/r", RunID: 1, Attempt: 1, JobID: 7}
	tick := func(refusedAt, exitAt time.Time) {
		t.Helper()
		result := app.TickResult{At: clock,
			RunnerRefusals:    map[domain.Platform]domain.RunnerRefusal{domain.PlatformLinux: {Version: "2.336.0", Instance: "trf-a", At: refusedAt}},
			EmptyExitBackoffs: []app.EmptyExitBackoff{{Demand: key, Exits: 3, LastAt: exitAt, RetryAt: exitAt.Add(30 * time.Minute)}}}
		ticker.recordRunnerImages(result)
		ticker.recordEmptyExitBackoffs(result)
	}
	count := func(want string) int { return strings.Count(logged.String(), want) }
	for range 10 {
		tick(now, now)
		clock = clock.Add(3 * time.Minute)
	}
	if refusals, holds := count("runner image refused by GitHub"), count("demand_empty_exit_backoff"); refusals != 1 || holds != 1 {
		t.Fatalf("said %d and %d times across one half-hour hold:\n%s", refusals, holds, logged.String())
	}
	tick(clock, clock)
	if refusals, holds := count("runner image refused by GitHub"), count("demand_empty_exit_backoff"); refusals != 2 || holds != 2 {
		t.Fatalf("a new reading and a new exit are news: %d and %d", refusals, holds)
	}
}
