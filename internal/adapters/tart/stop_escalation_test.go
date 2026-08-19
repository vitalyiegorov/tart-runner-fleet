package tart

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// findCommand returns the last command whose first argument is verb.
func findCommand(t *testing.T, runner *fakeRunner, verb string) []string {
	t.Helper()
	for i := len(runner.commands) - 1; i >= 0; i-- {
		if len(runner.commands[i]) > 0 && runner.commands[i][0] == verb {
			return runner.commands[i]
		}
	}
	t.Fatalf("no %q command in %v", verb, runner.commands)
	return nil
}

// TestGracefulStopNamesAWindowInsideItsOwnDeadline is the mechanism the
// 2026-08-10 incident turned on.
//
// `tart stop` waits `--timeout` seconds (default 30) for a guest-initiated
// shutdown and then forcefully terminates the VM itself. A bare `tart stop`
// under a context deadline therefore races tart's own escalation, and when the
// context wins, exec.CommandContext SIGKILLs the process that was about to
// escalate. The fleet's stop could never be more forceful than "ask nicely until
// the deadline"; an operator's unbounded shell `tart stop` escalated and
// returned exit 0 in about thirty seconds. Naming the window explicitly, and
// naming it well inside the deadline, is what stops the daemon killing its own
// escalation.
func TestGracefulStopNamesAWindowInsideItsOwnDeadline(t *testing.T) {
	now := time.Unix(500, 0).UTC()
	adapter, runner, registry, ownership := testAdapter(now)
	adapter.CommandTimeout = 45 * time.Second
	registry.data["vm"] = ownership
	runner.vms["vm"] = vm{Name: "vm", Running: true}
	if err := adapter.Stop(context.Background(), "vm", ownership); err != nil {
		t.Fatal(err)
	}
	command := findCommand(t, runner, "stop")
	if !reflect.DeepEqual(command, []string{"stop", "vm", "--timeout", "15"}) {
		t.Fatalf("stop command = %v, want an explicit graceful window", command)
	}
	// The window must leave tart room to force the guest off and finish inside
	// the deadline. Equal would be the incident: tart escalates exactly as the
	// daemon kills it.
	if window := time.Duration(adapter.gracefulStopSeconds()) * time.Second; window*2 > adapter.timeout() {
		t.Fatalf("graceful window %s leaves no room inside the %s deadline", window, adapter.timeout())
	}
}

func TestGracefulStopWindowNeverReachesZero(t *testing.T) {
	adapter := &Adapter{CommandTimeout: time.Second}
	if got := adapter.gracefulStopSeconds(); got != 1 {
		t.Fatalf("gracefulStopSeconds() = %d, want a window that still asks the guest", got)
	}
	if got := (&Adapter{}).gracefulStopSeconds(); got != 10 {
		t.Fatalf("default gracefulStopSeconds() = %d", got)
	}
}

// TestTerminateForcesTheGuestOffWithoutWaiting is rung two. `--timeout 0` tells
// tart to forcefully terminate the VM immediately rather than wait for a guest
// that has stopped answering.
func TestTerminateForcesTheGuestOffWithoutWaiting(t *testing.T) {
	now := time.Unix(500, 0).UTC()
	adapter, runner, registry, ownership := testAdapter(now)
	registry.data["vm"] = ownership
	runner.vms["vm"] = vm{Name: "vm", Running: true}
	if err := adapter.Terminate(context.Background(), "vm", ownership); err != nil {
		t.Fatal(err)
	}
	command := findCommand(t, runner, "stop")
	if !reflect.DeepEqual(command, []string{"stop", "vm", "--timeout", "0"}) {
		t.Fatalf("terminate command = %v", command)
	}
	if runner.vms["vm"].Running {
		t.Fatal("guest still running after Terminate")
	}
}

// TestDestroyForcesThenRemovesUnderTheSameEvidenceDeleteRequires pins that the
// most forceful rung is not a relaxed one. It differs from Delete only in
// refusing to wait for a shutdown the guest has already proved it will not
// perform; the ownership and deletion-confirmation guards are identical.
func TestDestroyForcesThenRemovesUnderTheSameEvidenceDeleteRequires(t *testing.T) {
	now := time.Unix(500, 0).UTC()
	adapter, runner, registry, ownership := testAdapter(now)
	registry.data["vm"] = ownership
	runner.vms["vm"] = vm{Name: "vm", Running: true}
	if err := adapter.Destroy(context.Background(), "vm", ownership); err != nil {
		t.Fatal(err)
	}
	if _, present := runner.vms["vm"]; present {
		t.Fatal("guest survived Destroy")
	}
	var verbs []string
	for _, command := range runner.commands {
		if len(command) > 0 && (command[0] == "stop" || command[0] == "delete") {
			verbs = append(verbs, command[0])
		}
	}
	if !reflect.DeepEqual(verbs, []string{"stop", "delete"}) {
		t.Fatalf("destroy ran %v, want a forced stop then a delete", verbs)
	}
	if got := findCommand(t, runner, "stop"); !reflect.DeepEqual(got, []string{"stop", "vm", "--timeout", "0"}) {
		t.Fatalf("destroy stop command = %v", got)
	}
}

func TestDestroyRefusesWhatDeleteRefuses(t *testing.T) {
	now := time.Unix(500, 0).UTC()
	adapter, runner, registry, ownership := testAdapter(now)
	registry.data["vm"] = ownership
	runner.vms["vm"] = vm{Name: "vm", Running: true}
	adapter.Confirmation = fakeConfirmation{confirmation: operations.DeletionConfirmation{}}
	if err := adapter.Destroy(context.Background(), "vm", ownership); !errors.Is(err, operations.ErrUncertain) {
		t.Fatalf("Destroy without fresh deletion confirmation = %v", err)
	}
	if err := adapter.Destroy(context.Background(), "../escape", ownership); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("Destroy(bad name) = %v", err)
	}
	if err := adapter.Destroy(context.Background(), "absent", ownership); err != nil {
		t.Fatalf("Destroy(absent) = %v, want success so a partial teardown can be retried", err)
	}
	other := operations.Ownership{ControllerID: "other", ResourceID: "r", OperationID: "o"}
	if err := adapter.Destroy(context.Background(), "vm", other); !errors.Is(err, operations.ErrConflict) {
		t.Fatalf("Destroy(foreign ownership) = %v", err)
	}
}
