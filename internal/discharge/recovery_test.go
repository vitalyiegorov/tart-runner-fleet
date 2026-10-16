package discharge

import (
	"context"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

type recoveryConfirmation struct {
	value operations.DeletionConfirmation
	err   error
	calls int
}

func (f *recoveryConfirmation) ConfirmDeletion(context.Context, string) (operations.DeletionConfirmation, error) {
	f.calls++
	return f.value, f.err
}

type automaticRecoverer interface {
	RecoverStoppedCleanup(context.Context, operations.DeadLetter, domain.Instance, interface {
		ConfirmDeletion(context.Context, string) (operations.DeletionConfirmation, error)
	}) error
}

func (f *fakeStore) ResumeStoppedCleanup(_ context.Context, op, id string, at time.Time) error {
	f.requests = append(f.requests, operations.Discharge{OperationID: op, InstanceID: id, At: at})
	return f.err
}

func TestParkedStoppedCleanupRecoversWithoutOperator(t *testing.T) {
	store, vm := &fakeStore{}, &fakeVM{}
	svc, _ := service(store, vm, true)
	recovery, ok := any(svc).(automaticRecoverer)
	if !ok {
		t.Fatal("fleet has no automatic parked-cleanup recovery")
	}
	req := request(true)
	letter := operations.DeadLetter{OperationID: req.OperationID, ResourceID: req.InstanceID, Kind: "deregister", Code: "stop"}
	instance := domain.Instance{ID: req.InstanceID, State: domain.InstanceDraining, Power: domain.InstancePowerStopped}
	confirmation := &recoveryConfirmation{value: operations.DeletionConfirmation{Fresh: true, RunnerInactive: true, JobsInactive: true, ObservedAt: time.Unix(9000, 0)}}
	if err := recovery.RecoverStoppedCleanup(context.Background(), letter, instance, confirmation); err != nil {
		t.Fatal(err)
	}
	if len(store.requests) != 1 || len(vm.reaped) != 0 {
		t.Fatalf("cleanup not completed: requests=%v reaped=%v", store.requests, vm.reaped)
	}
}

func TestAutomaticCleanupPreservesEveryUnprovedOrBusyResource(t *testing.T) {
	for _, name := range []string{"power error", "confirmation error", "shadow", "no confirmation", "wrong kind", "wrong code", "progressing", "wrong instance", "running state", "running power", "unknown power", "unavailable confirmation", "busy runner", "busy job", "stale", "future", "store race", "power changed", "absent", "delete"} {
		t.Run(name, func(t *testing.T) {
			store, vm := &fakeStore{}, &fakeVM{}
			svc, _ := service(store, vm, true)
			req := request(true)
			letter := operations.DeadLetter{OperationID: req.OperationID, ResourceID: req.InstanceID, Kind: "deregister", Code: "stop"}
			instance := domain.Instance{ID: req.InstanceID, State: domain.InstanceDraining, Power: domain.InstancePowerStopped}
			confirm := &recoveryConfirmation{value: operations.DeletionConfirmation{Fresh: true, RunnerInactive: true, JobsInactive: true, ObservedAt: time.Unix(9000, 0)}}
			var input interface {
				ConfirmDeletion(context.Context, string) (operations.DeletionConfirmation, error)
			} = confirm
			wantResume, wantError := false, false
			switch name {
			case "power error":
				vm.runningErr = operations.ErrUncertain
				wantError = true
			case "confirmation error":
				confirm.err = operations.ErrUncertain
				wantError = true
			case "shadow":
				svc.Authority = false
			case "no confirmation":
				input = nil
			case "wrong kind":
				letter.Kind = "clone"
			case "wrong code":
				letter.Code = "deregister:runner_busy"
			case "progressing":
				letter.ResourceProgressing = true
			case "wrong instance":
				instance.ID = "other"
			case "running state":
				instance.State = domain.InstanceRunning
			case "running power":
				instance.Power = domain.InstancePowerRunning
			case "unknown power":
				instance.Power = domain.InstancePowerUnknown
			case "unavailable confirmation":
				confirm.value.Fresh = false
			case "busy runner":
				confirm.value.RunnerInactive = false
			case "busy job":
				confirm.value.JobsInactive = false
			case "stale":
				confirm.value.ObservedAt = confirm.value.ObservedAt.Add(-time.Minute)
			case "future":
				confirm.value.ObservedAt = confirm.value.ObservedAt.Add(time.Second)
			case "store race":
				store.err = operations.ErrResourceProgressing
				wantError = true
			case "power changed":
				vm.power = domain.InstancePowerRunning

			case "absent":
				instance.Power = domain.InstancePowerAbsent
				vm.power = domain.InstancePowerAbsent
				wantResume = true
			case "delete":
				letter.Code = "delete"
				wantResume = true
			}
			err := svc.RecoverStoppedCleanup(context.Background(), letter, instance, input)
			if (err != nil) != wantError {
				t.Fatalf("error=%v wantError=%v", err, wantError)
			}
			if len(vm.reaped) > 0 || ((len(store.requests) > 0 && err == nil) != wantResume) {
				t.Fatalf("reaped=%v", vm.reaped)
			}
		})
	}
}
