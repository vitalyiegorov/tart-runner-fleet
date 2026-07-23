// Package lifecycle executes one owned runner's external lifecycle while
// keeping state persistence and every external system behind narrow ports.
package lifecycle

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/tart"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

const (
	// OperationProvision and OperationDrain intentionally match the durable
	// operation kinds emitted by reconcile.Controller today.
	OperationProvision = "clone"
	OperationDrain     = "deregister"
	defaultWorkFolder  = "_work"
	bootstrapHelper    = "/usr/local/libexec/tart-runner-fleet-bootstrap"
	maxJITBytes        = 1 << 20
)

// Stage is a bounded, non-secret failure code suitable for durable state and
// logs. Raw adapter errors must never be persisted because a child process may
// reflect credential material in its output.
type Stage string

const (
	StageClone      Stage = "clone"
	StageStart      Stage = "start"
	StageReady      Stage = "ready"
	StageAcquire    Stage = "acquire_jit"
	StageBootstrap  Stage = "bootstrap"
	StageRegister   Stage = "register"
	StageGuard      Stage = "drain_guard"
	StageDeregister Stage = "deregister"
	StageConfirm    Stage = "confirm_inactive"
	StageStop       Stage = "stop"
	StageDelete     Stage = "delete"
	StagePersist    Stage = "persist"
)

type stageError struct{ stage Stage }

func (e stageError) Error() string { return "runner lifecycle failed at " + string(e.stage) }

// StateChange is a compare-and-swap request. Implementations must atomically
// check both the state and version; the executor never assumes direct mutation
// is safe.
type StateChange struct {
	InstanceID      string
	ExpectedState   operations.State
	ExpectedVersion int64
	NextState       operations.State
	FailureCode     string
}

type StateStore interface {
	Instance(context.Context, string) (operations.Instance, error)
	Advance(context.Context, StateChange) (operations.Instance, error)
}

// VMControl is implemented by tart.Adapter.
type VMControl interface {
	Clone(context.Context, tart.Request) error
	Start(context.Context, string, operations.Ownership) error
	Stop(context.Context, string, operations.Ownership) error
	Delete(context.Context, string, operations.Ownership) error
	// Running reports the VM's current power state; an absent VM is not
	// running. Recovery drains re-verify their premise against this before
	// every destructive step.
	Running(context.Context, string) (bool, error)
}

type Readiness interface {
	Wait(context.Context, operations.Instance) error
}

type Registration interface {
	Registered(context.Context, string) (bool, error)
	ResetRegistration(context.Context, string) error
	AcquireAndGenerateJIT(context.Context, int64, string, string) (*githubscaleset.JITSecret, error)
}

type Bootstrapper interface {
	Bootstrap(context.Context, string, *githubscaleset.JITSecret) error
}

// DrainControl must derive both answers from fresh GitHub runner/job state.
// Deregister is required to be idempotent for an already absent runner.
type DrainControl interface {
	SafeToDeregister(context.Context, operations.Instance) (bool, error)
	Deregister(context.Context, operations.Instance) error
	ConfirmDeletion(context.Context, string) (operations.DeletionConfirmation, error)
	// RunnerRegistered reports whether the instance's runner is currently
	// registered on GitHub, without folding in demand state.
	RunnerRegistered(context.Context, operations.Instance) (bool, error)
	// JobStarted reports whether fresh demand state shows a workflow job has
	// begun executing on this runner. A stalled-assignment recovery drain aborts
	// the moment it becomes true: the assignment materialized into real work.
	JobStarted(context.Context, operations.Instance) (bool, error)
}

// ProvisionExecutor is a restartable state machine. One durable outbox
// operation remains incomplete until the runner is online; after a crash, the
// durable instance state selects the first effect that still needs observing.
type ProvisionExecutor struct {
	State                    StateStore
	VM                       VMControl
	Ready                    Readiness
	Registration             Registration
	Bootstrap                Bootstrapper
	Bases                    map[domain.Platform]string
	DiskGiB                  map[domain.ProfileID]int
	WorkFolder               string
	RegistrationTimeout      time.Duration
	RegistrationPollInterval time.Duration
	After                    func(time.Duration) <-chan time.Time
}

func (e ProvisionExecutor) Execute(ctx context.Context, operation operations.Operation) error {
	if e.State == nil || operation.Kind != OperationProvision || operation.ResourceID == "" {
		return operations.ErrInvalid
	}
	instance, err := e.State.Instance(ctx, operation.ResourceID)
	if err != nil {
		return safeError(StagePersist)
	}
	if err := validateOwned(instance, operation.ResourceID); err != nil {
		return err
	}
	for range 8 { // six states plus a strict corruption guard
		if err := ctx.Err(); err != nil {
			return err
		}
		switch instance.State {
		case operations.StateAssigned, operations.StateRunning:
			return nil
		case operations.StateDraining, operations.StateDeregistering, operations.StateStopping, operations.StateDeleted:
			// A demand completion may supersede provisioning before the guest
			// registers. Completing the provision effect releases only its
			// explicitly dependent cleanup operation; it never resurrects work.
			return nil
		case operations.StateOnlineIdle:
			// Every provision operation owns one concrete GitHub job demand. A
			// registered JIT runner is therefore already reserved, never warm
			// idle capacity. Repair legacy persisted online_idle instances before
			// the scheduler can mistake them for drainable handoff capacity.
			instance, err = e.advance(ctx, instance, operations.StateAssigned)
		case operations.StateFailed:
			// A failed lifecycle must never complete its outbox effect: doing so
			// would release dependent operations as if provisioning succeeded.
			return safeError(StagePersist)
		case operations.StatePlanned:
			base := e.Bases[instance.Platform]
			if e.VM == nil || tart.ValidateName(base) != nil {
				return e.fail(ctx, instance, StageClone)
			}
			if err := e.VM.Clone(ctx, tart.Request{
				Name: instance.ID, Base: base, CPU: instance.Resources.CPU, MemoryMB: instance.Resources.MemoryMB,
				DiskGB: e.DiskGiB[instance.Profile], Ownership: instance.Ownership,
			}); err != nil {
				return e.fail(ctx, instance, StageClone)
			}
			instance, err = e.advance(ctx, instance, operations.StateCloning)
		case operations.StateCloning:
			if e.VM == nil {
				return e.fail(ctx, instance, StageStart)
			}
			if err := e.VM.Start(ctx, instance.ID, instance.Ownership); err != nil {
				return e.fail(ctx, instance, StageStart)
			}
			instance, err = e.advance(ctx, instance, operations.StateBooting)
		case operations.StateBooting:
			if e.Ready == nil {
				return e.fail(ctx, instance, StageReady)
			}
			if err := e.Ready.Wait(ctx, instance); err != nil {
				return e.fail(ctx, instance, StageReady)
			}
			instance, err = e.advance(ctx, instance, operations.StateReachable)
		case operations.StateReachable:
			if e.Registration == nil || e.Bootstrap == nil {
				return e.fail(ctx, instance, StageAcquire)
			}
			registered, observeErr := e.Registration.Registered(ctx, instance.ID)
			if observeErr != nil {
				return e.fail(ctx, instance, StageRegister)
			}
			if registered {
				// GenerateJIT creates a broker-side runner record before the guest
				// listener starts. A Reachable instance therefore cannot trust an
				// existing record after a crashed/failed bootstrap; replace it.
				if resetErr := e.Registration.ResetRegistration(ctx, instance.ID); resetErr != nil {
					return e.fail(ctx, instance, StageRegister)
				}
			}
			secret, acquireErr := e.Registration.AcquireAndGenerateJIT(ctx, instance.Demand.JobID, instance.ID, e.workFolder())
			if acquireErr != nil || secret == nil || secret.Reveal() == "" {
				if secret != nil {
					secret.Destroy()
				}
				return e.fail(ctx, instance, StageAcquire)
			}
			bootstrapErr := e.Bootstrap.Bootstrap(ctx, instance.ID, secret)
			secret.Destroy()
			if bootstrapErr != nil {
				// Best-effort immediate cleanup prevents the reservation created by
				// GenerateJIT from masquerading as a live listener on retry. Even if
				// cleanup fails, the next Reachable attempt retries it before acquire.
				_ = e.Registration.ResetRegistration(ctx, instance.ID)
				return e.fail(ctx, instance, StageBootstrap)
			}
			instance, err = e.advance(ctx, instance, operations.StateRegistering)
		case operations.StateRegistering:
			if e.Registration == nil {
				return e.fail(ctx, instance, StageRegister)
			}
			if observeErr := e.waitRegistered(ctx, instance.ID); observeErr != nil {
				// A controller shutdown or operation deadline leaves the durable
				// instance resumable in registering. The worker can safely retry it
				// after restart without generating another JIT configuration.
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return e.fail(ctx, instance, StageRegister)
			}
			instance, err = e.advance(ctx, instance, operations.StateAssigned)
		default:
			return operations.ErrConflict
		}
		if err != nil {
			return safeError(StagePersist)
		}
	}
	return safeError(StagePersist)
}

func (e ProvisionExecutor) waitRegistered(ctx context.Context, name string) error {
	waitCtx, cancel := context.WithTimeout(ctx, e.registrationTimeout())
	defer cancel()
	for {
		registered, err := e.Registration.Registered(waitCtx, name)
		if err != nil {
			return err
		}
		if registered {
			return nil
		}
		select {
		case <-waitCtx.Done():
			return waitCtx.Err()
		case <-e.after(e.registrationPollInterval()):
		}
	}
}

func (e ProvisionExecutor) registrationTimeout() time.Duration {
	if e.RegistrationTimeout <= 0 {
		return 30 * time.Second
	}
	return e.RegistrationTimeout
}

func (e ProvisionExecutor) registrationPollInterval() time.Duration {
	if e.RegistrationPollInterval <= 0 {
		return 250 * time.Millisecond
	}
	return e.RegistrationPollInterval
}

func (e ProvisionExecutor) after(delay time.Duration) <-chan time.Time {
	if e.After == nil {
		return time.After(delay)
	}
	return e.After(delay)
}

func (e ProvisionExecutor) workFolder() string {
	if e.WorkFolder == "" {
		return defaultWorkFolder
	}
	return e.WorkFolder
}

func (e ProvisionExecutor) advance(ctx context.Context, instance operations.Instance, next operations.State) (operations.Instance, error) {
	return e.State.Advance(ctx, StateChange{InstanceID: instance.ID, ExpectedState: instance.State, ExpectedVersion: instance.Version, NextState: next})
}

func (e ProvisionExecutor) fail(_ context.Context, _ operations.Instance, stage Stage) error {
	return safeError(stage)
}

// DrainExecutor completes GitHub deregistration and Tart destruction before
// its durable operation may be acknowledged. This preserves scheduler drain
// dependencies: a cross-platform spawn cannot start after mere deregistration
// while the old VM still exists.
type DrainExecutor struct {
	State                    StateStore
	VM                       VMControl
	Control                  DrainControl
	ConfirmationMaxAge       time.Duration
	ConfirmationTimeout      time.Duration
	ConfirmationPollInterval time.Duration
	Now                      func() time.Time
	After                    func(time.Duration) <-chan time.Time
}

func (e DrainExecutor) Execute(ctx context.Context, operation operations.Operation) error {
	if e.State == nil || operation.Kind != OperationDrain || operation.ResourceID == "" {
		return operations.ErrInvalid
	}
	instance, err := e.State.Instance(ctx, operation.ResourceID)
	if err != nil {
		return safeError(StagePersist)
	}
	if err := validateOwned(instance, operation.ResourceID); err != nil {
		return err
	}
	for range 6 {
		if err := ctx.Err(); err != nil {
			return err
		}
		switch instance.State {
		case operations.StateDeleted:
			return nil
		case operations.StateFailed:
			// In particular, never release a cross-platform spawn that depends
			// on this drain while the failed VM may still exist.
			return safeError(StagePersist)
		case operations.StateDraining:
			if e.Control == nil {
				return e.fail(ctx, instance, StageGuard)
			}
			// A recovery drain is a standing kill order derived from a single
			// planning-time observation. Re-verify that premise against ground
			// truth on every attempt: one stale observation must never be able
			// to destroy a VM whose runner may be executing a job (2026-07-20
			// incident: a glitched power reading planned a stopped-recovery
			// drain of a busy runner; the registration guard refused 23 times,
			// then one transient runner-lookup miss released the kill).
			// Aborting on contrary evidence is always safe: if the instance is
			// genuinely reclaimable, the next inventory observation re-plans
			// the drain.
			switch instance.DrainPhase {
			case operations.DrainPhaseStoppedRecovery:
				if e.VM == nil {
					return e.fail(ctx, instance, StageGuard)
				}
				running, runningErr := e.VM.Running(ctx, instance.ID)
				if runningErr != nil {
					return e.fail(ctx, instance, StageGuard)
				}
				if running {
					return e.abort(ctx, instance)
				}
				// The VM is provably powered off, so deregistration cannot
				// interrupt work. Proceed without consulting the registration
				// lookup: a powered-off VM's lingering registration must not
				// delay reclaim, and a transient lookup miss must not gate it.
			case operations.DrainPhaseInactiveRecovery:
				registered, registeredErr := e.Control.RunnerRegistered(ctx, instance)
				if registeredErr != nil {
					return e.fail(ctx, instance, StageGuard)
				}
				if registered {
					return e.abort(ctx, instance)
				}
				safe, guardErr := e.Control.SafeToDeregister(ctx, instance)
				if guardErr != nil || !safe {
					return e.fail(ctx, instance, StageGuard)
				}
			case operations.DrainPhaseStalledAssignment:
				// The assignment deadline planned this kill from a single
				// observation that no job had started. Re-verify against fresh
				// demand state before deregistering the (idle) runner: if a job has
				// since started, the assignment materialized after all, so abort to
				// Running and let normal completion drain it. Deregistering an
				// assigned-but-not-started runner is safe — GitHub re-queues the job
				// to another runner, exactly the manual remedy the operator applied.
				started, startedErr := e.Control.JobStarted(ctx, instance)
				if startedErr != nil {
					return e.fail(ctx, instance, StageGuard)
				}
				if started {
					return e.abort(ctx, instance)
				}
			default:
				safe, guardErr := e.Control.SafeToDeregister(ctx, instance)
				if guardErr != nil || !safe {
					return e.fail(ctx, instance, StageGuard)
				}
			}
			if err := e.Control.Deregister(ctx, instance); err != nil {
				return e.fail(ctx, instance, StageDeregister)
			}
			if confirmErr := e.waitConfirmed(ctx, instance.ID); confirmErr != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return e.fail(ctx, instance, StageConfirm)
			}
			instance, err = e.advance(ctx, instance, operations.StateDeregistering)
		case operations.StateDeregistering:
			if e.VM == nil {
				return e.fail(ctx, instance, StageStop)
			}
			if err := e.VM.Stop(ctx, instance.ID, instance.Ownership); err != nil {
				return e.fail(ctx, instance, StageStop)
			}
			instance, err = e.advance(ctx, instance, operations.StateStopping)
		case operations.StateStopping:
			if e.VM == nil {
				return e.fail(ctx, instance, StageConfirm)
			}
			if confirmErr := e.waitConfirmed(ctx, instance.ID); confirmErr != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return e.fail(ctx, instance, StageConfirm)
			}
			if err := e.VM.Delete(ctx, instance.ID, instance.Ownership); err != nil {
				return e.fail(ctx, instance, StageDelete)
			}
			instance, err = e.advance(ctx, instance, operations.StateDeleted)
		default:
			return operations.ErrConflict
		}
		if err != nil {
			return safeError(StagePersist)
		}
	}
	return safeError(StagePersist)
}

// abort cancels a recovery drain whose premise fresh evidence has disproven.
// The instance returns to Running — the conservative busy state; demand
// projections advance it as real events arrive — and the drain operation is
// acknowledged as a completed no-op so it stops being retried.
func (e DrainExecutor) abort(ctx context.Context, instance operations.Instance) error {
	if _, err := e.advance(ctx, instance, operations.StateRunning); err != nil {
		return safeError(StagePersist)
	}
	return nil
}

func (e DrainExecutor) waitConfirmed(ctx context.Context, id string) error {
	if e.Control == nil {
		return operations.ErrInvalid
	}
	waitCtx, cancel := context.WithTimeout(ctx, e.confirmationTimeout())
	defer cancel()
	for {
		confirmation, err := e.Control.ConfirmDeletion(waitCtx, id)
		if err != nil {
			return err
		}
		if confirmation.Safe(e.now(), e.confirmationMaxAge()) {
			return nil
		}
		select {
		case <-waitCtx.Done():
			return waitCtx.Err()
		case <-e.after(e.confirmationPollInterval()):
		}
	}
}

func (e DrainExecutor) advance(ctx context.Context, instance operations.Instance, next operations.State) (operations.Instance, error) {
	return e.State.Advance(ctx, StateChange{InstanceID: instance.ID, ExpectedState: instance.State, ExpectedVersion: instance.Version, NextState: next})
}

func (e DrainExecutor) fail(_ context.Context, _ operations.Instance, stage Stage) error {
	return safeError(stage)
}

func (e DrainExecutor) now() time.Time {
	if e.Now == nil {
		return time.Now().UTC()
	}
	return e.Now().UTC()
}

func (e DrainExecutor) confirmationMaxAge() time.Duration {
	if e.ConfirmationMaxAge <= 0 {
		return 30 * time.Second
	}
	return e.ConfirmationMaxAge
}

func (e DrainExecutor) confirmationTimeout() time.Duration {
	if e.ConfirmationTimeout <= 0 {
		return 30 * time.Second
	}
	return e.ConfirmationTimeout
}

func (e DrainExecutor) confirmationPollInterval() time.Duration {
	if e.ConfirmationPollInterval <= 0 {
		return 250 * time.Millisecond
	}
	return e.ConfirmationPollInterval
}

func (e DrainExecutor) after(delay time.Duration) <-chan time.Time {
	if e.After == nil {
		return time.After(delay)
	}
	return e.After(delay)
}

func validateOwned(instance operations.Instance, resourceID string) error {
	if instance.ID == "" || instance.ID != resourceID || !instance.Ownership.Valid() || !instance.SchedulingMetadataValid() ||
		tart.ValidateName(instance.ID) != nil {
		return operations.ErrInvalid
	}
	return nil
}

func safeError(stage Stage) error { return stageError{stage: stage} }

// StdinRunner is the only process boundary used for JIT bootstrap. The secret
// is supplied on standard input and never appears in argv or environment.
type StdinRunner interface {
	Run(context.Context, io.Reader, ...string) error
}

type ExecStdinRunner struct{ Binary string }

func (r ExecStdinRunner) Run(ctx context.Context, stdin io.Reader, args ...string) error {
	if r.Binary == "" {
		return operations.ErrInvalid
	}
	// #nosec G204 -- Binary is a trusted dependency and all external names are
	// validated before reaching this fixed argument vector.
	command := exec.CommandContext(ctx, r.Binary, args...)
	command.Stdin = stdin
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	return command.Run()
}

type StdinBootstrapper struct {
	Runner  StdinRunner
	Timeout time.Duration
}

func (b StdinBootstrapper) Bootstrap(ctx context.Context, name string, secret *githubscaleset.JITSecret) error {
	if tart.ValidateName(name) != nil || secret == nil || b.Runner == nil {
		return operations.ErrInvalid
	}
	encoded := secret.Reveal()
	defer secret.Destroy()
	if encoded == "" || len(encoded) > maxJITBytes {
		return operations.ErrInvalid
	}
	timeout := b.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := b.Runner.Run(commandCtx, strings.NewReader(encoded+"\n"), "exec", "-i", name, bootstrapHelper); err != nil {
		if commandCtx.Err() != nil {
			return commandCtx.Err()
		}
		return errors.New("runner bootstrap failed")
	}
	return nil
}
