package daemon

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/telemetry"
)

// probeRunner is the neutral command runner under the probe: it records the
// argument vector and either answers, fails, or outlives the caller's deadline.
type probeRunner struct {
	args  []string
	err   error
	delay time.Duration
}

func (r *probeRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	r.args = args
	if r.delay > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(r.delay):
		}
	}
	return nil, r.err
}

// The three outcomes, and the argument vector. `exec <instance> true` is the
// same verb on both supported backends, which is why the probe is typed on the
// command runner rather than on a backend.
func TestTheGuestProbeClassifiesRefusalSeparatelyFromSlowness(t *testing.T) {
	for _, test := range []struct {
		name   string
		runner *probeRunner
		want   domain.GuestLiveness
	}{
		{name: "the guest ran the command", runner: &probeRunner{}, want: domain.GuestLivenessAlive},
		{name: "the transport was refused", runner: &probeRunner{err: errors.New("Failed to connect to the VM using its control socket")},
			want: domain.GuestLivenessRefused},
		{name: "the guest was too busy to answer in time",
			runner: &probeRunner{delay: time.Second, err: errors.New("signal: killed")},
			want:   domain.GuestLivenessUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			probe := execGuestProbe{Runner: test.runner, Timeout: 20 * time.Millisecond}
			if got := probe.Probe(context.Background(), "trf-xl-1"); got != test.want {
				t.Fatalf("Probe() = %q, want %q", got, test.want)
			}
			if len(test.runner.args) != 3 || test.runner.args[0] != "exec" ||
				test.runner.args[1] != "trf-xl-1" || test.runner.args[2] != "true" {
				t.Fatalf("the probe must run `exec <instance> true`; got %v", test.runner.args)
			}
		})
	}
}

// Everything the probe cannot establish is unknown, and unknown never counts
// against a guest.
func TestTheGuestProbeEstablishesNothingWithoutItsInputs(t *testing.T) {
	for _, test := range []struct {
		name  string
		probe execGuestProbe
		id    string
	}{
		{name: "no runner", probe: execGuestProbe{Timeout: time.Second}, id: "trf-xl-1"},
		{name: "no deadline", probe: execGuestProbe{Runner: &probeRunner{}}, id: "trf-xl-1"},
		{name: "an instance name the fleet does not mint",
			probe: execGuestProbe{Runner: &probeRunner{}, Timeout: time.Second}, id: "; rm -rf /"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := test.probe.Probe(context.Background(), test.id); got != domain.GuestLivenessUnknown {
				t.Fatalf("Probe() = %q, want unknown", got)
			}
		})
	}
	// A cancelled tick is not a refusal either.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	probe := execGuestProbe{Runner: &probeRunner{err: errors.New("context canceled")}, Timeout: time.Second}
	if got := probe.Probe(ctx, "trf-xl-1"); got != domain.GuestLivenessUnknown {
		t.Fatalf("a cancelled tick must establish nothing; got %q", got)
	}
}

func TestTheTrackerIsWiredOnlyWhereBothHalvesExist(t *testing.T) {
	apple, linux := platformFor("darwin"), platformFor("linux")
	enabled := config.Default()
	if tracker := guestLivenessTracker(apple, enabled, nil); tracker == nil {
		t.Fatal("a Tart node with the shipped bound must probe its guests")
	}
	disabled := config.Default()
	disabled.GuestLiveness = config.GuestLiveness{}
	if tracker := guestLivenessTracker(apple, disabled, nil); tracker != nil {
		t.Fatal("a node whose operator disabled the bound must not probe at all")
	}
	// Node B before it has a container backend: a real daemon measuring a real
	// machine, with no guest to ask.
	if tracker := guestLivenessTracker(linux, enabled, nil); tracker != nil {
		t.Fatal("a node with no execution technology has no guest to probe")
	}
	containers := config.Default()
	containers.Executor = config.Executor{Backend: config.ExecutorPodman, Image: "ghcr.io/example/runner:1"}
	if tracker := guestLivenessTracker(linux, containers, nil); tracker == nil {
		t.Fatal("a container node probes its guests with the same verb")
	}
	if tracker := guestLivenessTracker(platform{}, enabled, nil); tracker != nil {
		t.Fatal("a platform with no probe constructor wires nothing")
	}
}

// deathClock is the wall clock of the host-side reproduction: the guest kernel
// panicked at 17:48:26 and every probe from that instant on was refused.
var deathClock = time.Date(2026, 8, 16, 17, 50, 26, 0, time.UTC)

var lostJob = domain.DemandKey{Repo: "rnw-community/rnw-community", RunID: 31_939_037_119,
	Attempt: 1, JobID: 93_540_000_001}

func guestSilence(refusals int, silent time.Duration, unresponsive bool) scheduler.GuestSilence {
	return scheduler.GuestSilence{Instance: "trf-xl-0aacdbcc6653bd8a", Profile: "xl",
		Repo: lostJob.Repo, Demand: lostJob, Resources: domain.Resources{CPU: 6, MemoryMB: 12_288, Slots: 1},
		Refusals: refusals, Silence: silent, LastAlive: deathClock.Add(-silent - 30*time.Second),
		RequiredRefusals: 5, Window: 90 * time.Second, Unresponsive: unresponsive}
}

func silenceReporter() (*failureReporter, *bytes.Buffer) {
	logged := &bytes.Buffer{}
	return newFailureReporter(logged, func() time.Time { return deathClock }), logged
}

// The gap this closes, stated exactly: across eight production deaths "there is
// no daemon log line for the instance at all". The line has to carry the
// instance, the job binding, and the probe timeline, or the class stays
// self-concealing after the fix.
func TestReportGuestSilenceNamesTheInstanceTheJobAndTheTimeline(t *testing.T) {
	reporter, logged := silenceReporter()

	reporter.reportGuestSilence([]scheduler.GuestSilence{guestSilence(5, 2*time.Minute, true)})

	line := logged.String()
	for _, fragment := range []string{"instance guest unresponsive", "instance=trf-xl-0aacdbcc6653bd8a",
		"profile=xl", "repo=rnw-community/rnw-community", "cpu=6", "memoryMb=12288",
		"refusals=5", "requiredRefusals=5", "silent=2m0s", "window=1m30s",
		"lastAlive=2026-08-16T17:47:56Z", "runId=31939037119", "jobId=93540000001"} {
		if !strings.Contains(line, fragment) {
			t.Fatalf("guest warning missing %q: %q", fragment, line)
		}
	}
}

// A guest that goes quiet is worth one line before it is worth a verdict, and
// the escalation must never be swallowed by the warning that preceded it.
func TestReportGuestSilenceEscalatesButDoesNotRepeat(t *testing.T) {
	reporter, logged := silenceReporter()
	watched := guestSilence(2, 40*time.Second, false)

	reporter.reportGuestSilence([]scheduler.GuestSilence{watched})
	reporter.reportGuestSilence([]scheduler.GuestSilence{watched})
	if got := strings.Count(logged.String(), "instance guest silent"); got != 1 {
		t.Fatalf("in-window silent lines = %d, want the rate limit to hold at 1: %q", got, logged.String())
	}

	dead := guestSilence(5, 2*time.Minute, true)
	reporter.reportGuestSilence([]scheduler.GuestSilence{dead})
	reporter.reportGuestSilence([]scheduler.GuestSilence{dead})
	if got := strings.Count(logged.String(), "instance guest unresponsive"); got != 1 {
		t.Fatalf("in-window unresponsive lines = %d: %q", got, logged.String())
	}
	if got := strings.Count(logged.String(), "instance guest "); got != 2 {
		t.Fatalf("total guest lines = %d, want the warning and the escalation: %q", got, logged.String())
	}
	neighbour := dead
	neighbour.Instance = "trf-xl-9c4d0011"
	reporter.reportGuestSilence([]scheduler.GuestSilence{neighbour})
	if !strings.Contains(logged.String(), "instance=trf-xl-9c4d0011") {
		t.Fatalf("a second instance was suppressed by the first: %q", logged.String())
	}
}

// A reclaim is a destructive decision and every one of them is on the record.
func TestReportGuestReclaimNamesTheJobThatDiedWithTheGuest(t *testing.T) {
	reporter, logged := silenceReporter()
	dead := guestSilence(5, 2*time.Minute, true)
	reclaim := scheduler.Operation{Kind: scheduler.OperationDrain, Instance: dead.Instance, Profile: dead.Profile,
		Demand: lostJob, Recovery: true, GuestUnresponsive: true}

	reporter.reportGuestReclaim(reclaim, []scheduler.GuestSilence{dead})

	line := logged.String()
	for _, fragment := range []string{"instance reclaimed because its guest stopped answering",
		"instance=trf-xl-0aacdbcc6653bd8a", "profile=xl", "repo=rnw-community/rnw-community",
		"runId=31939037119", "jobId=93540000001", "attempt=1", "refusals=5", "silent=2m0s",
		"lastAlive=2026-08-16T17:47:56Z", "lost-communication failure on GitHub"} {
		if !strings.Contains(line, fragment) {
			t.Fatalf("reclaim line missing %q: %q", fragment, line)
		}
	}
	reporter.reportGuestReclaim(reclaim, []scheduler.GuestSilence{dead})
	if got := strings.Count(logged.String(), "instance reclaimed because its guest"); got != 2 {
		t.Fatalf("reclaim lines = %d, want every destructive decision on the record", got)
	}
}

// Every other reclaim has a different premise and its own reporting. Announcing
// one as a guest death would attribute a cut job to a probe that never fired.
func TestReportGuestReclaimIgnoresEveryOtherReclaim(t *testing.T) {
	reporter, logged := silenceReporter()
	dead := guestSilence(5, 2*time.Minute, true)
	for _, other := range []scheduler.Operation{
		{Kind: scheduler.OperationDrain, Instance: dead.Instance, Recovery: true, LingeringRunner: true},
		{Kind: scheduler.OperationDrain, Instance: dead.Instance, Recovery: true, OccupancyExceeded: true},
		{Kind: scheduler.OperationSpawn, Demand: lostJob},
	} {
		reporter.reportGuestReclaim(other, []scheduler.GuestSilence{dead})
	}
	if logged.Len() != 0 {
		t.Fatalf("a reclaim with another premise was announced as a guest death: %q", logged.String())
	}
}

// A guest this daemon has never seen answer is a different fact from one that
// answered a minute ago, and neither may be rendered as year one.
func TestAnUnobservedGuestIsNamedRatherThanDated(t *testing.T) {
	reporter, logged := silenceReporter()
	never := guestSilence(5, 2*time.Minute, true)
	never.LastAlive = time.Time{}

	reporter.reportGuestSilence([]scheduler.GuestSilence{never})

	if !strings.Contains(logged.String(), `lastAlive="never observed"`) {
		t.Fatalf("an unobserved guest must be named, not dated: %q", logged.String())
	}
}

// silenceTicker builds the engine ticker over the incident's profile and bound,
// so the metric, the warning, and the reclaim all read the same projection.
func silenceTicker(t *testing.T, reporter *failureReporter) (engineTicker, *telemetry.Health) {
	t.Helper()
	health, err := telemetry.NewHealth(wallClock{}, telemetry.HealthConfig{Profiles: []string{"xl"},
		CriticalObservations: []string{"scheduler"}})
	if err != nil {
		t.Fatal(err)
	}
	profile := domain.Profile{ID: "xl", Platform: domain.PlatformLinux, Route: "tiered",
		Resources: domain.Resources{CPU: 6, MemoryMB: 12_288, Slots: 1}}
	engine := app.Engine{Config: scheduler.Config{Profiles: map[domain.ProfileID]domain.Profile{"xl": profile},
		GuestLiveness: domain.GuestLivenessPolicy{ConsecutiveRefusals: 5, Window: 90 * time.Second}}}
	return engineTicker{engine: engine, health: health, reporter: reporter}, health
}

func silenceTickResult(reclaimed bool) app.TickResult {
	dead := domain.Instance{ID: "trf-xl-0aacdbcc6653bd8a", Repo: lostJob.Repo, Demand: lostJob,
		Platform: domain.PlatformLinux, Profile: "xl", Route: "tiered",
		Resources: domain.Resources{CPU: 6, MemoryMB: 12_288, Slots: 1},
		State:     domain.InstanceRunning, Power: domain.InstancePowerRunning,
		RunningSince: deathClock.Add(-20 * time.Minute), OccupiedSince: deathClock.Add(-20 * time.Minute),
		Guest: domain.GuestLivenessState{Refusals: 5, RefusedSince: deathClock.Add(-2 * time.Minute),
			LastAlive: deathClock.Add(-150 * time.Second), LastProbe: deathClock}}
	result := app.TickResult{At: deathClock, Instances: []domain.Instance{dead}}
	if reclaimed {
		result.Plan = scheduler.Plan{Operations: []scheduler.Operation{{Kind: scheduler.OperationDrain,
			Instance: dead.ID, Profile: "xl", Demand: lostJob, Recovery: true, GuestUnresponsive: true}}}
	}
	return result
}

// The tick fans one projection out to both surfaces: the metric an alert scrapes
// and the lines an operator reads. They come from the same scheduler.GuestSilences
// call as the reclaim itself, so they cannot disagree about a silence.
func TestRecordGuestLivenessPublishesAndSpeaks(t *testing.T) {
	reporter, logged := silenceReporter()
	ticker, health := silenceTicker(t, reporter)

	ticker.recordGuestLiveness(silenceTickResult(true))

	published := health.Snapshot().GuestSilences
	if len(published) != 1 {
		t.Fatalf("published silences = %#v, want the dead guest", published)
	}
	want := telemetry.GuestSilenceMetric{Instance: "trf-xl-0aacdbcc6653bd8a", Profile: "xl", Repo: lostJob.Repo,
		CPU: 6, MemoryMB: 12_288, Refusals: 5, Silence: 2 * time.Minute, RequiredRefusals: 5,
		Window: 90 * time.Second, Unresponsive: true, RunID: lostJob.RunID, JobID: lostJob.JobID}
	if published[0] != want {
		t.Fatalf("published metric = %#v, want %#v", published[0], want)
	}
	if health.GuestLiveness().OK {
		t.Fatalf("the dead guest did not reach the health verdict: %#v", health.GuestLiveness())
	}
	for _, fragment := range []string{"instance guest unresponsive",
		"instance reclaimed because its guest stopped answering"} {
		if !strings.Contains(logged.String(), fragment) {
			t.Fatalf("tick log missing %q: %q", fragment, logged.String())
		}
	}
}

// The warning is optional and the metric is mandatory: an observe-mode harness
// has no reporter and must still measure every silence rather than becoming a
// fleet that sees nothing.
func TestRecordGuestLivenessWithoutAReporterStillPublishes(t *testing.T) {
	ticker, health := silenceTicker(t, nil)

	ticker.recordGuestLiveness(silenceTickResult(true))

	if published := health.Snapshot().GuestSilences; len(published) != 1 || !published[0].Unresponsive {
		t.Fatalf("a reporterless tick published %#v", published)
	}
}

// The quiet path stays quiet: no rows, no warning, and a health verdict that is
// OK because every guest answered rather than because nothing was measured.
func TestRecordGuestLivenessOfAFleetWhoseGuestsAnswer(t *testing.T) {
	reporter, logged := silenceReporter()
	ticker, health := silenceTicker(t, reporter)

	ticker.recordGuestLiveness(app.TickResult{At: deathClock})

	if published := health.Snapshot().GuestSilences; len(published) != 0 {
		t.Fatalf("a fleet whose guests answer published %#v", published)
	}
	if !health.GuestLiveness().OK || logged.Len() != 0 {
		t.Fatalf("a quiet fleet was reported: %#v %q", health.GuestLiveness(), logged.String())
	}
}
