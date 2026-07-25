package tart

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// reapFixture registers one owned, stopped VM — the shape a phantom instance
// leaves behind once its drain dead-letters.
func reapFixture(t *testing.T) (*Adapter, *fakeRunner, operations.Ownership) {
	t.Helper()
	now := time.Unix(100, 0).UTC()
	adapter, runner, registry, ownership := testAdapter(now)
	runner.vms["trf-maestro-096ffcb3a52d8624"] = VM{Name: "trf-maestro-096ffcb3a52d8624", Running: false}
	registry.data["trf-maestro-096ffcb3a52d8624"] = ownership
	return adapter, runner, ownership
}

// Reap must succeed exactly where Delete cannot: the runner confirmation a normal
// drain requires can never be Safe for a leaked GitHub registration, because
// RunnerInactive never becomes true.
func TestReapRemovesAStoppedOwnedVMWithoutRunnerConfirmation(t *testing.T) {
	adapter, runner, ownership := reapFixture(t)
	adapter.Confirmation = fakeConfirmation{err: errors.New("GitHub says the runner is still running a job")}
	if err := adapter.Reap(context.Background(), "trf-maestro-096ffcb3a52d8624", ownership); err != nil {
		t.Fatal(err)
	}
	if _, present := runner.vms["trf-maestro-096ffcb3a52d8624"]; present {
		t.Fatal("reap left the VM in place")
	}
	// Reap never stops a VM: it either finds one stopped or refuses.
	for _, command := range runner.commands {
		if command[0] == "stop" {
			t.Fatalf("reap stopped a VM: %v", runner.commands)
		}
	}
}

// An already-absent VM is success. The durable row is retired before the VM is
// removed, so a retry after a partial remedy must converge rather than fail.
func TestReapTreatsAnAbsentVMAsDone(t *testing.T) {
	adapter, runner, ownership := reapFixture(t)
	delete(runner.vms, "trf-maestro-096ffcb3a52d8624")
	if err := adapter.Reap(context.Background(), "trf-maestro-096ffcb3a52d8624", ownership); err != nil {
		t.Fatalf("absent VM reap=%v", err)
	}
	if err := adapter.Reap(context.Background(), "trf-maestro-096ffcb3a52d8624", ownership); err != nil {
		t.Fatalf("repeat reap=%v", err)
	}
}

// A lost delete response that actually took effect is success; one that did not is
// the command's own failure; and an unreadable re-observation stays uncertain.
func TestReapReconcilesAndFailsClosedOnDeleteFaults(t *testing.T) {
	t.Run("lost response", func(t *testing.T) {
		adapter, runner, ownership := reapFixture(t)
		runner.lostResponse["delete"] = true
		if err := adapter.Reap(context.Background(), "trf-maestro-096ffcb3a52d8624", ownership); err != nil {
			t.Fatalf("lost delete response was not reconciled: %v", err)
		}
	})
	t.Run("delete failed and VM survives", func(t *testing.T) {
		adapter, runner, ownership := reapFixture(t)
		runner.commandError["delete"] = errors.New("busy")
		if err := adapter.Reap(context.Background(), "trf-maestro-096ffcb3a52d8624", ownership); err == nil {
			t.Fatal("failed delete reported success")
		}
	})
	t.Run("re-observation unavailable", func(t *testing.T) {
		adapter, runner, ownership := reapFixture(t)
		runner.commandError["delete"] = errors.New("busy")
		runner.observeError = errors.New("tart unavailable")
		err := adapter.Reap(context.Background(), "trf-maestro-096ffcb3a52d8624", ownership)
		if !errors.Is(err, operations.ErrUncertain) && err == nil {
			t.Fatalf("unreadable observation error=%v", err)
		}
		var typed *Error
		if !errors.As(err, &typed) || typed.Kind != ErrorUncertain {
			t.Fatalf("unreadable observation must stay uncertain: %v", err)
		}
	})
}

// The three refusals that keep an operator override from becoming a blunt
// instrument: a running guest, a VM this controller does not own, and a name that
// is not a controller VM name at all.
func TestReapRefusesRunningUnownedAndInvalidVMs(t *testing.T) {
	t.Run("running", func(t *testing.T) {
		adapter, runner, ownership := reapFixture(t)
		vm := runner.vms["trf-maestro-096ffcb3a52d8624"]
		vm.Running = true
		runner.vms["trf-maestro-096ffcb3a52d8624"] = vm
		err := adapter.Reap(context.Background(), "trf-maestro-096ffcb3a52d8624", ownership)
		if !errors.Is(err, operations.ErrConflict) {
			t.Fatalf("running VM reap error=%v want ErrConflict", err)
		}
		if _, present := runner.vms["trf-maestro-096ffcb3a52d8624"]; !present {
			t.Fatal("refused reap still deleted the VM")
		}
	})
	t.Run("not owned", func(t *testing.T) {
		adapter, runner, _ := reapFixture(t)
		err := adapter.Reap(context.Background(), "trf-maestro-096ffcb3a52d8624",
			operations.Ownership{ControllerID: "other", ResourceID: "other", OperationID: "other"})
		if !errors.Is(err, operations.ErrConflict) {
			t.Fatalf("unowned VM reap error=%v want ErrConflict", err)
		}
		if _, present := runner.vms["trf-maestro-096ffcb3a52d8624"]; !present {
			t.Fatal("refused reap still deleted the VM")
		}
	})
	t.Run("ownership unreadable", func(t *testing.T) {
		now := time.Unix(100, 0).UTC()
		adapter, _, registry, ownership := testAdapter(now)
		registry.getErr = errors.New("ownership unavailable")
		if err := adapter.Reap(context.Background(), "trf-maestro-096ffcb3a52d8624", ownership); err == nil {
			t.Fatal("unreadable ownership reap succeeded")
		}
	})
	t.Run("invalid name", func(t *testing.T) {
		adapter, _, ownership := reapFixture(t)
		if err := adapter.Reap(context.Background(), strings.Repeat("x", 300), ownership); err == nil {
			t.Fatal("invalid VM name reap succeeded")
		}
	})
}
