package simulation_test

import (
	"context"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/discharge"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// This world executes the real recovery service. The former lifecycle mirror
// only modelled successful teardown and never generated a parked stop whose
// external effect later landed: a world-model blind spot, not a bad invariant.
type parkedCleanupWorld struct {
	now             time.Time
	power           domain.InstancePower
	confirmation    operations.DeletionConfirmation
	queued, deleted bool
	// resumedStopped is the power the resumption was told about: a running VM
	// must not restart through the stopped-recovery guard.
	resumedStopped bool
}

func (w *parkedCleanupWorld) Power(context.Context, string) (domain.InstancePower, error) {
	return w.power, nil
}
func (w *parkedCleanupWorld) ConfirmDeletion(context.Context, string) (operations.DeletionConfirmation, error) {
	return w.confirmation, nil
}
func (w *parkedCleanupWorld) DischargeDeadLetter(context.Context, operations.Discharge) (operations.DischargeOutcome, error) {
	panic("automatic recovery must not retire the row")
}
func (w *parkedCleanupWorld) ResumeCleanup(_ context.Context, _, _ string, stopped bool, _ time.Time) error {
	w.queued, w.resumedStopped = true, stopped
	return nil
}
func (w *parkedCleanupWorld) Reap(context.Context, string, operations.Ownership) error {
	w.deleted = true
	return nil
}

func TestParkedStopEventuallyCleansItselfWithoutTouchingLiveOrUnknownWork(t *testing.T) {
	w := &parkedCleanupWorld{now: time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)}
	service := discharge.Service{Store: w, VM: w, Authority: true, Now: func() time.Time { return w.now }}
	instance := domain.Instance{ID: "trf-maestro-28d766f77455c303", State: domain.InstanceDraining}
	letter := operations.DeadLetter{OperationID: "op-7af038cf09ead38cbad123d9", ResourceID: instance.ID, Kind: "deregister", Code: "stop", Attempts: 200}
	for tick := 0; tick < 4; tick++ {
		w.now = w.now.Add(time.Minute)
		switch tick {
		case 0:
			instance.Power = domain.InstancePowerRunning
		case 1:
			instance.Power = domain.InstancePowerUnknown
		case 2:
			instance.Power = domain.InstancePowerStopped
			w.power = domain.InstancePowerStopped
		case 3:
			w.confirmation = operations.DeletionConfirmation{Fresh: true, RunnerInactive: true, JobsInactive: true, ObservedAt: w.now}
		}
		if err := service.RecoverCleanup(context.Background(), letter, instance, false, w); err != nil {
			t.Fatal(err)
		}
		if tick < 3 && (w.queued || w.deleted) {
			t.Fatalf("unsafe cleanup at tick%d", tick)
		}
	}
	if !w.queued || w.deleted {
		t.Fatal("cleanup was not requeued, or the observer deleted the VM directly")
	}
}

// The #424 trigger state the generator lacked: a drain that died at stop while
// a host-wide fault (inode exhaustion, #423) refused every stop, leaving its VM
// running. The world walks the fault and its recovery; the real service must
// wait out the fault and stale GitHub evidence, then resume exactly once, and
// leave the stop itself to the worker.
func TestDeadStopWithRunningGuestResumesOnlyAfterTheHostFaultClears(t *testing.T) {
	w := &parkedCleanupWorld{now: time.Date(2026, 10, 25, 15, 35, 0, 0, time.UTC), power: domain.InstancePowerRunning}
	service := discharge.Service{Store: w, VM: w, Authority: true, Now: func() time.Time { return w.now }}
	instance := domain.Instance{ID: "trf-linux-4x8-83404cc4efa01437", State: domain.InstanceDeregistering, Power: domain.InstancePowerRunning}
	letter := operations.DeadLetter{OperationID: "event-drain-trf-linux-4x8-83404cc4efa01437", ResourceID: instance.ID,
		Kind: "deregister", Code: "stop", Attempts: 10, DeadAt: w.now}
	for tick := 0; tick < 6; tick++ {
		w.now = w.now.Add(5 * time.Minute)
		healthy := tick >= 3 // inodes exhausted until 15:50
		switch tick {
		case 4:
			w.confirmation = operations.DeletionConfirmation{Fresh: true, RunnerInactive: true, JobsInactive: true, ObservedAt: w.now}
		case 5:
			w.confirmation.ObservedAt = w.now
		}
		if err := service.RecoverCleanup(context.Background(), letter, instance, healthy, w); err != nil {
			t.Fatal(err)
		}
		if tick < 4 && w.queued {
			t.Fatalf("resumed at tick %d without a healthy host and a confirmed-idle runner", tick)
		}
		if w.queued {
			break
		}
	}
	if !w.queued || w.resumedStopped || w.deleted {
		t.Fatalf("queued=%v stopped=%v deleted=%v", w.queued, w.resumedStopped, w.deleted)
	}
}
