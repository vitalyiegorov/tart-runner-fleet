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
func (w *parkedCleanupWorld) ResumeStoppedCleanup(context.Context, string, string, time.Time) error {
	w.queued = true
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
		if err := service.RecoverStoppedCleanup(context.Background(), letter, instance, w); err != nil {
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
