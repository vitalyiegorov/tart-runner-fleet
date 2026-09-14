package autoupdate

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// withdrawnStatus is what the mac studio published for a day on 2026-09-14: it
// is under its disk reserve, has released every scale-set session, admits
// nothing, and runs nothing. Readiness is false and health is true, which is
// exactly the state ADR 0052 named.
const withdrawnStatus = `{"data":{"controllerVersion":"v1","controllerMode":"authority",` +
	`"ready":{"ok":false,"reasons":["critical_observation_stale"]},"healthy":{"ok":true},` +
	`"queues":[{"jobs":4}],"instances":[],"operations":{"retrying":0,"dead":0},` +
	`"sessionYield":{"yielded":true,"reason":"disk reserve"}}}`

// TestAWithdrawnNodeWithNoInstancesTakesARelease is issue #320 as a test. The
// node is maximally quiescent — nothing is running and nothing can be admitted —
// and before ADR 0052 that was the one state from which no release could ever be
// installed, because the gate demanded a readiness a withdrawn node can never
// report. Both transactions share the helper, so both are pinned here.
func TestAWithdrawnNodeWithNoInstancesTakesARelease(t *testing.T) {
	localHost, localCommand, localCurrent, _, _ := hostFixture(t)
	_, systemdCommand, systemdCurrent, _, _ := systemdFixture(t)
	localCommand.current = withdrawnStatus
	systemdCommand.current = withdrawnStatus
	if err := localHost.ensureQuiescent(context.Background(), localCurrent); err != nil {
		t.Fatalf("launchd: a withdrawn idle node was refused its release: %v", err)
	}
	if err := ensureQuiescent(context.Background(), systemdCommand, systemdCurrent); err != nil {
		t.Fatalf("systemd: a withdrawn idle node was refused its release: %v", err)
	}
	// The gate asks for health, never for readiness: asking for readiness is the
	// defect, not the wording of it.
	for _, command := range []*fakeCommand{localCommand, systemdCommand} {
		var asked bool
		for _, call := range command.calls {
			if strings.Contains(call, "status --require-ready") {
				t.Fatalf("the gate still demanded readiness: %s", call)
			}
			asked = asked || strings.Contains(call, "status --require-healthy")
		}
		if !asked {
			t.Fatalf("the gate asked nothing about health: %v", command.calls)
		}
	}
}

// The guarantee ADR 0011 exists for is untouched by the split: a withdrawn node
// that is still running an instance is still busy. Withdrawal is not drain — a
// node holds its sessions while an instance lives — so this state is real, and
// it is the one a release must never interrupt.
func TestAWithdrawnNodeRunningAnInstanceIsStillRefused(t *testing.T) {
	for name, status := range map[string]string{
		"one live instance": strings.Replace(withdrawnStatus, `"instances":[]`, `"instances":[{"profile":"maestro","count":1}]`, 1),
		"a retrying operation": strings.Replace(withdrawnStatus, `"operations":{"retrying":0,"dead":0}`,
			`"operations":{"retrying":1,"dead":0}`, 1),
	} {
		host, command, current, _, _ := hostFixture(t)
		command.current = status
		if err := host.ensureQuiescent(context.Background(), current); !errors.Is(err, ErrBusy) {
			t.Fatalf("%s: quiescence error=%v, want ErrBusy", name, err)
		}
	}
}

// Health is a gate, not a formality. A daemon that cannot report itself healthy
// fails the CLI's own `--require-healthy` with exit 5, and the transaction must
// carry that refusal out rather than swap a generation under a daemon that has
// stopped ticking or cannot write its store.
func TestAnUnhealthyNodeIsStillRefused(t *testing.T) {
	host, command, current, _, _ := hostFixture(t)
	command.current = withdrawnStatus
	command.currentErr = errors.New("exit status 5")
	if err := host.ensureQuiescent(context.Background(), current); err == nil {
		t.Fatal("an unhealthy daemon passed the quiescence gate")
	}
}

// The candidate proof is the other half of the same change. A generation that
// boots healthy as itself is proven, even though the node it booted on is
// withdrawn and therefore not ready — otherwise the transaction would roll back
// every release on exactly the node that had nothing running to protect.
func TestTheCandidateIsProvenHealthyNotReady(t *testing.T) {
	host, command, _, candidate, _ := hostFixture(t)
	command.ready = `{"data":{"controllerVersion":"v2","controllerMode":"authority",` +
		`"ready":{"ok":false,"reasons":["critical_observation_stale"]},"healthy":{"ok":true}}}`
	if err := host.Ready(context.Background(), candidate); err != nil {
		t.Fatalf("a healthy withdrawn candidate was rejected: %v", err)
	}
	// A candidate that is neither ready nor healthy is rejected, and so is one
	// whose identity does not match.
	command.ready = `{"data":{"controllerVersion":"v2","controllerMode":"authority",` +
		`"ready":{"ok":true},"healthy":{"ok":false,"reasons":["successful_tick_expired"]}}}`
	if err := host.Ready(context.Background(), candidate); err == nil {
		t.Fatal("an unhealthy candidate was accepted because it called itself ready")
	}
	// A candidate older than ADR 0052 publishes no health field at all. Readiness
	// is the only word it has for the same question and a strictly stronger one,
	// so a rollback onto such a generation still proves something.
	command.ready = `{"data":{"controllerVersion":"v2","controllerMode":"authority","ready":{"ok":true}}}`
	if err := host.Ready(context.Background(), candidate); err != nil {
		t.Fatalf("a pre-0052 candidate could not be proven at all: %v", err)
	}
}
