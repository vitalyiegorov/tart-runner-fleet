package operations

import (
	"context"
	"time"
)

type Store interface {
	Migrate(context.Context) error
	ApplyPlan(context.Context, Plan) (bool, error)
	SchedulerState(context.Context) (SchedulerState, error)
	ApplyDemandBatch(context.Context, int64, int64, []DemandEvent) (DemandBatchResult, error)
	ActiveDemands(context.Context, int64) ([]DemandRecord, error)
	DemandCursor(context.Context, int64) (int64, error)
	CreateInstance(context.Context, Instance) error
	Instance(context.Context, string) (Instance, error)
	LiveInstances(context.Context) ([]Instance, error)
	Transition(context.Context, Transition) (Instance, Operation, error)
	Claim(context.Context, string, int, time.Time, time.Duration) ([]Operation, error)
	RenewOperation(context.Context, string, string, time.Time, time.Duration) error
	Complete(context.Context, string, string, string, time.Time) (bool, error)
	Retry(context.Context, string, string, string, time.Time, bool) error
	RecoverExpired(context.Context, time.Time) (int64, error)
	AcquireLease(context.Context, string, string, time.Time, time.Duration) (Lease, error)
	RenewLease(context.Context, Lease, time.Time, time.Duration) (Lease, error)
	ReleaseLease(context.Context, Lease) error
	PutOwnership(context.Context, string, Ownership) error
	Ownership(context.Context, string) (Ownership, error)
}

type Confirmer interface {
	ConfirmDeletion(context.Context, string) (DeletionConfirmation, error)
}

type Drainer struct {
	Store              Store
	Confirmer          Confirmer
	ConfirmationMaxAge time.Duration
	Now                func() time.Time
}

func (d Drainer) Confirm(ctx context.Context, instance Instance, operation Operation) (Instance, Operation, error) {
	if instance.State != StateDraining || instance.DrainPhase != 1 {
		return Instance{}, Operation{}, ErrConflict
	}
	confirmation, err := d.Confirmer.ConfirmDeletion(ctx, instance.ID)
	if err != nil {
		return Instance{}, Operation{}, err
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	if !confirmation.Safe(now().UTC(), d.ConfirmationMaxAge) {
		return Instance{}, Operation{}, ErrUncertain
	}
	operation.AvailableAt = now().UTC()
	return d.Store.Transition(ctx, Transition{
		InstanceID:      instance.ID,
		ExpectedState:   StateDraining,
		ExpectedVersion: instance.Version,
		NextState:       StateDeregistering,
		DrainPhase:      2,
		Operation:       operation,
	})
}
