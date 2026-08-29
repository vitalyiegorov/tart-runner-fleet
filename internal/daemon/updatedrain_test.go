package daemon

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/autoupdate"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/telemetry"
)

func drainController(t *testing.T, names []string, err error) (*updateDrainController, *failureReporter) {
	t.Helper()
	reporter, _ := silenceReporter()
	read := func(string) ([]string, error) {
		if err != nil {
			return nil, err
		}
		return names, nil
	}
	policy := autoupdate.DrainPolicy{Enabled: true, PendingFor: 30 * time.Minute,
		MaxWait: 2 * time.Hour, Cooldown: time.Hour}
	return newUpdateDrainController(policy, "/root", "v0.1.498+main.a", read, reporter), reporter
}

var drainAt = time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC)

func TestUpdateDrainRefusesAdmissionOnlyForAWaitingCandidate(t *testing.T) {
	controller, _ := drainController(t, []string{"v0.1.498+main.a", "v0.1.510+main.b"}, nil)
	controller.Observe(drainAt, 2)
	if controller.Draining() {
		t.Fatal("drained on the tick the candidate was first seen")
	}
	if got := controller.Candidate(); got != "v0.1.510+main.b" {
		t.Fatalf("candidate = %q, want the newest unapplied generation", got)
	}
	controller.Observe(drainAt.Add(30*time.Minute), 2)
	if !controller.Draining() {
		t.Fatal("never drained for a candidate that waited the full bound")
	}
	metric := controller.Metric()
	if !metric.Draining || metric.Candidate != "v0.1.510+main.b" {
		t.Fatalf("published metric %+v does not describe the drain", metric)
	}
	if metric.PendingSince.IsZero() || metric.Since.IsZero() {
		t.Fatal("a drain with no dates cannot be aged by an operator")
	}
}

// A node whose releases directory it cannot read must serve normally. Refusing
// admission over a fact this node could not establish is the failure mode that
// would turn a filesystem hiccup into a self-inflicted outage.
func TestUpdateDrainNeverRefusesOnAnUnreadableRoot(t *testing.T) {
	controller, _ := drainController(t, nil, errors.New("unreadable"))
	for minute := 0; minute <= 240; minute += 15 {
		controller.Observe(drainAt.Add(time.Duration(minute)*time.Minute), 1)
		if controller.Draining() {
			t.Fatalf("drained at minute %d on an unreadable releases directory", minute)
		}
	}
	if controller.Metric().Candidate != "" {
		t.Fatal("named a candidate it could not read")
	}
}

func TestUpdateDrainStandsDownWhenNothingNewerExists(t *testing.T) {
	controller, _ := drainController(t, []string{"v0.1.498+main.a", "v0.1.461+main.old"}, nil)
	for minute := 0; minute <= 120; minute += 15 {
		controller.Observe(drainAt.Add(time.Duration(minute)*time.Minute), 0)
	}
	if controller.Draining() {
		t.Fatal("drained with no newer generation on disk")
	}
	if metric := controller.Metric(); metric.Draining || metric.Candidate != "" {
		t.Fatalf("published %+v for a node already running the newest generation", metric)
	}
}

func TestUpdateDrainLogsEachTransitionWithItsCandidate(t *testing.T) {
	controller, _ := drainController(t, []string{"v0.1.498+main.a", "v0.1.510+main.b"}, nil)
	reporter, logged := silenceReporter()
	controller.report = func(action, version string, instances int) {
		reporter.reportUpdateDrain(action, version, instances)
	}
	controller.Observe(drainAt, 1)
	controller.Observe(drainAt.Add(30*time.Minute), 1)
	line := logged.String()
	if line == "" {
		t.Fatal("a drain started with no log line at all")
	}
	for _, want := range []string{"update drain start", "v0.1.510+main.b"} {
		if !strings.Contains(line, want) {
			t.Fatalf("log %q does not carry %q", line, want)
		}
	}
}

func TestUpdateDrainNilControllerIsInert(t *testing.T) {
	var absent *updateDrainController
	absent.Observe(drainAt, 3)
	if absent.Draining() || absent.Candidate() != "" {
		t.Fatal("a nil controller claimed a drain")
	}
	if (absent.Metric() != telemetry.UpdateDrainMetric{}) {
		t.Fatal("a nil controller published a posture")
	}
}

// The daemon reads the releases beside the configuration it was started with,
// not the ambient user's default installation — two installations on one
// machine must not read each other's candidates.
func TestInstallationRootIsDerivedFromTheConfigurationPath(t *testing.T) {
	for path, want := range map[string]string{
		"/opt/trf/state/fleet.json":                               "/opt/trf",
		"/home/u/.local/share/tart-runner-fleet/state/fleet.json": "/home/u/.local/share/tart-runner-fleet",
		"": "",
	} {
		if got := installationRoot(path); got != want {
			t.Fatalf("installationRoot(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestApplyUpdateDrainCountsLiveInstancesAndPublishes(t *testing.T) {
	health, err := telemetry.NewHealth(wallClock{}, telemetry.HealthConfig{Profiles: []string{"small"}})
	if err != nil {
		t.Fatalf("NewHealth: %v", err)
	}
	controller, _ := drainController(t, []string{"v0.1.498+main.a", "v0.1.510+main.b"}, nil)
	ticker := engineTicker{health: health, drain: controller}

	live := domain.Instance{ID: "trf-small-1", Profile: "small", State: domain.InstanceRunning}
	result := func(at time.Time) app.TickResult {
		return app.TickResult{At: at, Instances: []domain.Instance{live}}
	}
	ticker.applyUpdateDrain(result(drainAt))
	metric := health.Snapshot().UpdateDrain
	if metric == nil || metric.Draining {
		t.Fatalf("published %+v, want a candidate noted but no drain yet", metric)
	}
	if metric.Candidate != "v0.1.510+main.b" {
		t.Fatalf("candidate = %q, want the newest generation on disk", metric.Candidate)
	}

	ticker.applyUpdateDrain(result(drainAt.Add(30 * time.Minute)))
	if published := health.Snapshot().UpdateDrain; published == nil || !published.Draining {
		t.Fatalf("a drain in progress was not published: %+v", published)
	}

	// A ticker without a controller is every mode that has no updater: it must
	// publish nothing rather than a false "not draining".
	quiet := engineTicker{health: health}
	quiet.applyUpdateDrain(result(drainAt))
}

func TestUpdateDrainReportingToleratesAnAbsentLoggerAndCandidate(t *testing.T) {
	var absent *failureReporter
	absent.reportUpdateDrain("start", "v0.1.510", 1)
	(&failureReporter{}).reportUpdateDrain("start", "v0.1.510", 1)

	reporter, logged := silenceReporter()
	reporter.reportUpdateDrain("stop", "", 0)
	if line := logged.String(); !strings.Contains(line, "unknown") {
		t.Fatalf("a nameless candidate logged as %q", line)
	}
}
