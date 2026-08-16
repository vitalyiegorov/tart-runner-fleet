package daemon

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
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
	if tracker := guestLivenessTracker(apple, enabled); tracker == nil {
		t.Fatal("a Tart node with the shipped bound must probe its guests")
	}
	disabled := config.Default()
	disabled.GuestLiveness = config.GuestLiveness{}
	if tracker := guestLivenessTracker(apple, disabled); tracker != nil {
		t.Fatal("a node whose operator disabled the bound must not probe at all")
	}
	// Node B before it has a container backend: a real daemon measuring a real
	// machine, with no guest to ask.
	if tracker := guestLivenessTracker(linux, enabled); tracker != nil {
		t.Fatal("a node with no execution technology has no guest to probe")
	}
	containers := config.Default()
	containers.Executor = config.Executor{Backend: config.ExecutorPodman, Image: "ghcr.io/example/runner:1"}
	if tracker := guestLivenessTracker(linux, containers); tracker == nil {
		t.Fatal("a container node probes its guests with the same verb")
	}
	if tracker := guestLivenessTracker(platform{}, enabled); tracker != nil {
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
