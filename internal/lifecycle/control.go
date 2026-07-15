package lifecycle

import (
	"context"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

type ScaleSetControl interface {
	Registered(context.Context, string) (bool, error)
	AcquireAndGenerateJIT(context.Context, int64, string, string) (*githubscaleset.JITSecret, error)
	Deregister(context.Context, string) error
}

type directJITControl interface {
	GenerateJIT(context.Context, string, string) (*githubscaleset.JITSecret, error)
}

type DemandReader interface {
	DemandRecord(context.Context, int64, int64) (operations.DemandRecord, error)
}

type SourceKey struct {
	Repo    string
	Profile domain.ProfileID
}

type SourceBinding struct {
	StoreKey int64
	Source   ScaleSetControl
}

// ControlRouter binds a durable instance to the exact GitHub registration
// scope/profile that emitted its demand. It is intentionally not keyed by the
// server's numeric scale-set ID, which may collide across installations.
type ControlRouter struct {
	State   StateStore
	Demand  DemandReader
	Sources map[SourceKey]SourceBinding
	Now     func() time.Time
}

func (r ControlRouter) Registered(ctx context.Context, name string) (bool, error) {
	_, binding, err := r.resolve(ctx, name)
	if err != nil {
		return false, err
	}
	return binding.Source.Registered(ctx, name)
}

func (r ControlRouter) ResetRegistration(ctx context.Context, name string) error {
	_, binding, err := r.resolve(ctx, name)
	if err != nil {
		return err
	}
	return binding.Source.Deregister(ctx, name)
}

func (r ControlRouter) AcquireAndGenerateJIT(ctx context.Context, requestID int64, name, workFolder string) (*githubscaleset.JITSecret, error) {
	instance, binding, err := r.resolve(ctx, name)
	if err != nil {
		return nil, err
	}
	if requestID <= 0 || requestID != instance.Demand.JobID {
		return nil, operations.ErrConflict
	}
	if githubscaleset.IsPreassignedRequestID(requestID) {
		direct, ok := binding.Source.(directJITControl)
		if !ok {
			return nil, operations.ErrInvalid
		}
		return direct.GenerateJIT(ctx, name, workFolder)
	}
	return binding.Source.AcquireAndGenerateJIT(ctx, requestID, name, workFolder)
}

func (r ControlRouter) SafeToDeregister(ctx context.Context, instance operations.Instance) (bool, error) {
	binding, err := r.binding(instance)
	if err != nil {
		return false, err
	}
	if instance.DrainPhase == operations.DrainPhaseStoppedRecovery {
		registered, registrationErr := binding.Source.Registered(ctx, instance.ID)
		return !registered, registrationErr
	}
	if instance.DrainPhase == operations.DrainPhaseInactiveRecovery {
		registered, registrationErr := binding.Source.Registered(ctx, instance.ID)
		if registrationErr != nil || registered {
			return false, registrationErr
		}
	}
	record, err := r.demand(ctx, binding, instance)
	if err != nil {
		return false, err
	}
	return record.Status == operations.DemandJobCompleted, nil
}

func (r ControlRouter) Deregister(ctx context.Context, instance operations.Instance) error {
	binding, err := r.binding(instance)
	if err != nil {
		return err
	}
	return binding.Source.Deregister(ctx, instance.ID)
}

func (r ControlRouter) ConfirmDeletion(ctx context.Context, name string) (operations.DeletionConfirmation, error) {
	instance, binding, err := r.resolve(ctx, name)
	if err != nil {
		return operations.DeletionConfirmation{}, err
	}
	registered, err := binding.Source.Registered(ctx, name)
	if err != nil {
		return operations.DeletionConfirmation{}, err
	}
	if instance.DrainPhase == operations.DrainPhaseStoppedRecovery {
		return operations.DeletionConfirmation{Fresh: true, RunnerInactive: !registered,
			JobsInactive: !registered, ObservedAt: r.now()}, nil
	}
	record, err := r.demand(ctx, binding, instance)
	if err != nil {
		return operations.DeletionConfirmation{}, err
	}
	return operations.DeletionConfirmation{Fresh: true, RunnerInactive: !registered,
		JobsInactive: record.Status == operations.DemandJobCompleted, ObservedAt: r.now()}, nil
}

func (r ControlRouter) resolve(ctx context.Context, name string) (operations.Instance, SourceBinding, error) {
	if r.State == nil || name == "" {
		return operations.Instance{}, SourceBinding{}, operations.ErrInvalid
	}
	instance, err := r.State.Instance(ctx, name)
	if err != nil {
		return operations.Instance{}, SourceBinding{}, err
	}
	binding, err := r.binding(instance)
	return instance, binding, err
}

func (r ControlRouter) binding(instance operations.Instance) (SourceBinding, error) {
	binding, ok := r.Sources[SourceKey{Repo: instance.Repo, Profile: instance.Profile}]
	if !ok || binding.StoreKey <= 0 || binding.Source == nil {
		return SourceBinding{}, operations.ErrUncertain
	}
	return binding, nil
}

func (r ControlRouter) demand(ctx context.Context, binding SourceBinding, instance operations.Instance) (operations.DemandRecord, error) {
	if r.Demand == nil || instance.Demand.JobID <= 0 {
		return operations.DemandRecord{}, operations.ErrUncertain
	}
	return r.Demand.DemandRecord(ctx, binding.StoreKey, instance.Demand.JobID)
}

func (r ControlRouter) now() time.Time {
	if r.Now == nil {
		return time.Now().UTC()
	}
	return r.Now().UTC()
}
