package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
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
