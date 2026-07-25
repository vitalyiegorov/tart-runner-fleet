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
	// RunnerActiveJob reports whether any durable demand in the scale set shows a
	// workflow job currently executing on the named runner. The runner name — not
	// the demand a VM was spawned for — is the only safe key for "is this runner
	// busy", because scale-set brokering may hand a runner a different job.
	RunnerActiveJob(context.Context, int64, string) (bool, error)
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

func (r ControlRouter) RunnerRegistered(ctx context.Context, instance operations.Instance) (bool, error) {
	binding, err := r.binding(instance)
	if err != nil {
		return false, err
	}
	return binding.Source.Registered(ctx, instance.ID)
}

// JobStarted derives from the durable demand record whether a workflow job has
// begun executing on this runner. Only a JobStarted (or later) status implies
// live work; an assignment that never progressed past JobAssigned is a stalled
// zombie safe to reclaim.
func (r ControlRouter) JobStarted(ctx context.Context, instance operations.Instance) (bool, error) {
	status, err := r.demandStatus(ctx, instance)
	return status == operations.DemandJobStarted || status == operations.DemandJobCompleted, err
}

// JobActive reports whether fresh demand state shows a workflow job is
// currently executing on this runner — status exactly JobStarted, not a
// terminal completion. A lingering-runner recovery uses it as the inverse of
// its planning premise (the demand showed no active job): if a job is active
// again the drain aborts, otherwise the idle runner is reclaimed. Distinct from
// JobStarted, which treats a completed job as "started" for the assignment path.
func (r ControlRouter) JobActive(ctx context.Context, instance operations.Instance) (bool, error) {
	status, err := r.demandStatus(ctx, instance)
	return status == operations.DemandJobStarted, err
}

// RunnerBusy reports whether fresh durable demand evidence shows a workflow job
// currently executing on this instance's runner, whichever demand GitHub
// brokered to it. JobStarted/JobActive both read the ONE demand the VM was
// spawned for; that is the wrong key once brokering is decoupled from spawning,
// because the runner may be executing a sibling job whose completion the fleet
// is not waiting on. Scoping stays within the instance's own scale-set binding:
// GitHub only brokers a scale set's jobs to that scale set's runners.
func (r ControlRouter) RunnerBusy(ctx context.Context, instance operations.Instance) (bool, error) {
	binding, err := r.binding(instance)
	if err != nil {
		return false, err
	}
	if r.Demand == nil || instance.ID == "" {
		return false, operations.ErrUncertain
	}
	return r.Demand.RunnerActiveJob(ctx, binding.StoreKey, instance.ID)
}

func (r ControlRouter) demandStatus(ctx context.Context, instance operations.Instance) (operations.DemandEventKind, error) {
	binding, err := r.binding(instance)
	if err != nil {
		return "", err
	}
	record, err := r.demand(ctx, binding, instance)
	if err != nil {
		return "", err
	}
	return record.Status, nil
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
	if instance.DrainPhase == operations.DrainPhaseStoppedRecovery || instance.DrainPhase == operations.DrainPhaseStalledAssignment ||
		instance.DrainPhase == operations.DrainPhaseLingeringRunner {
		// Once the runner is deregistered the stalled/lingering runner is gone;
		// there is no fresh JobCompleted event to wait for (the job never started,
		// or its completion already passed without draining the instance). Derive
		// job inactivity from runner absence, exactly as stopped recovery does.
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
