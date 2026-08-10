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
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/executor"
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

// stageError is the only failure shape a lifecycle operation persists. The
// optional reason is a closed-vocabulary runner-administration code, never
// upstream text: a durable operation row and the operator API both render it.
type stageError struct {
	stage  Stage
	reason string
	// exhausted marks a failure no further attempt can change. It travels as a Go
	// error and never as text: the persisted message stays byte-identical to the
	// ordinary failure at the same stage, so FailureCode and every operator
	// surface keep reading exactly one closed code.
	exhausted bool
}

func (e stageError) Error() string {
	if e.reason == "" {
		return "runner lifecycle failed at " + string(e.stage)
	}
	return "runner lifecycle failed at " + string(e.stage) + " (" + e.reason + ")"
}

// Unwrap exposes an exhausted ladder as operations.ErrExhausted, which is what
// the durable worker parks on rather than retries.
func (e stageError) Unwrap() error {
	if e.exhausted {
		return operations.ErrExhausted
	}
	return nil
}

// FailureReason exposes the bounded diagnostic to programmatic callers, exactly
// as githubscaleset.SessionFailure does for the ingest path. An unrecognized
// reason is withheld rather than surfaced.
func (e stageError) FailureReason() string {
	if !githubscaleset.ValidRunnerFailureReason(e.reason) && !ValidBootstrapFailureReason(e.reason) {
		return ""
	}
	return e.reason
}

// CodeUnclassified is the withheld code for a durable failure string outside the
// closed vocabulary, so telemetry can aggregate failures without ever echoing
// stored text (an executor panic message, for instance).
const CodeUnclassified = "unclassified"

var failureStages = [...]Stage{StageClone, StageStart, StageReady, StageAcquire, StageBootstrap, StageRegister,
	StageGuard, StageDeregister, StageConfirm, StageStop, StageDelete, StagePersist}

// FailureCode maps a persisted lifecycle failure back to exactly one closed
// code, either "<stage>" or "<stage>:<reason>". It is total by exact match, so
// nothing an executor did not author can reach an operator surface.
func FailureCode(persisted string) string {
	for _, stage := range failureStages {
		if persisted == (stageError{stage: stage}).Error() {
			return string(stage)
		}
		for _, reason := range append(githubscaleset.RunnerFailureReasons(), BootstrapFailureReasons()...) {
			if persisted == (stageError{stage: stage, reason: reason}).Error() {
				return string(stage) + ":" + reason
			}
		}
	}
	return CodeUnclassified
}

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

// VMControl is the slice of executor.Backend one runner's lifecycle needs: the
// mutating verbs and the power observation, and nothing that enumerates the
// host. Every executor.Backend satisfies it — tests/contract pins that — so the
// node's backend is chosen once in internal/daemon and never named here.
type VMControl interface {
	Create(context.Context, executor.InstanceSpec) error
	Start(context.Context, string, operations.Ownership) error
	Stop(context.Context, string, operations.Ownership) error
	// Terminate and Destroy are the second and third rungs of the stop ladder a
	// drain climbs when its guest will not power itself down (ADR 0039).
	Terminate(context.Context, string, operations.Ownership) error
	Destroy(context.Context, string, operations.Ownership) error
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

// Bootstrapper starts the preinstalled runner inside one guest. The final
// argument is what the assigned scale sets require of that guest's image; an
// empty list is every guest that predates issue #202 and produces exactly the
// argument vector it always did.
type Bootstrapper interface {
	Bootstrap(context.Context, string, *githubscaleset.JITSecret, []string) error
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
	// JobActive reports whether fresh demand state shows a workflow job is
	// currently executing (status exactly JobStarted, not completed). A
	// lingering-runner recovery drain aborts the moment it becomes true: the
	// idle runner picked up real work after the planning observation.
	JobActive(context.Context, operations.Instance) (bool, error)
	// RunnerBusy reports whether fresh evidence shows a workflow job executing on
	// this instance's RUNNER, whichever demand GitHub brokered to it. It is
	// deliberately not keyed by the demand the VM was spawned for: scale-set
	// brokering is decoupled from the fleet's demand-keyed spawning, so a runner
	// spawned for demand X may be executing a different matching job Y, and
	// "demand X completed" then says nothing about whether the runner is idle.
	RunnerBusy(context.Context, operations.Instance) (bool, error)
}

// ProvisionExecutor is a restartable state machine. One durable outbox
// operation remains incomplete until the runner is online; after a crash, the
// durable instance state selects the first effect that still needs observing.
type ProvisionExecutor struct {
	State        StateStore
	VM           VMControl
	Ready        Readiness
	Registration Registration
	Bootstrap    Bootstrapper
	Bases        map[domain.Platform]string
	DiskGiB      map[domain.ProfileID]int
	// Capabilities is what each profile's scale sets require of the guest image.
	// The instance carries its profile, not its scale set, and a profile may be
	// exposed by more than one scope, so this is the union over every scale set
	// routed to that profile — the conservative answer, and the only one derivable
	// from what an instance records.
	Capabilities             map[domain.ProfileID][]string
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
			image := e.Bases[instance.Platform]
			// The image is validated by the neutral rule, not by the instance-name
			// grammar it used to share with Tart: on a container node it is an OCI
			// reference, whose slashes and colons an instance name forbids. What a
			// valid image *is* beyond that is the backend's own question, and
			// internal/lifecycle is not allowed to have an opinion about it
			// (ADR 0034, amendment for issue #139).
			if e.VM == nil || domain.ValidateImageReference(image) != nil {
				return e.fail(ctx, instance, StageClone)
			}
			if err := e.VM.Create(ctx, executor.InstanceSpec{
				Name: instance.ID, Image: image, CPU: instance.Resources.CPU, MemoryMB: instance.Resources.MemoryMB,
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
			bootstrapErr := e.Bootstrap.Bootstrap(ctx, instance.ID, secret, e.Capabilities[instance.Profile])
			secret.Destroy()
			if bootstrapErr != nil {
				// Best-effort immediate cleanup prevents the reservation created by
				// GenerateJIT from masquerading as a live listener on retry. Even if
				// cleanup fails, the next Reachable attempt retries it before acquire.
				_ = e.Registration.ResetRegistration(ctx, instance.ID)
				return bootstrapFailure(bootstrapErr)
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

// bootstrapFailure keeps the one bounded diagnostic a failed bootstrap can carry
// and discards everything else. A guest that answered the capability question
// with "no" produced a closed-vocabulary reason, which travels into the durable
// operation row exactly as a runner-administration reason does; any other
// failure stays the bare stage it has always been, because the helper's output
// may reflect the JIT configuration it was given.
func bootstrapFailure(cause error) error {
	var staged stageError
	if errors.As(cause, &staged) && ValidBootstrapFailureReason(staged.reason) {
		return staged
	}
	return safeError(StageBootstrap)
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
			case operations.DrainPhaseLingeringRunner:
				// The idle-runner deadline planned this kill from a single
				// observation that the bound demand carried no active job. Re-verify
				// against fresh demand state before deregistering the (idle) runner:
				// if a job is now actively executing, the runner is genuinely busy, so
				// abort to Running and let normal completion drain it. Otherwise the
				// job has ended (or was cancelled before it began) and deregistering
				// is safe — GitHub re-queues any pending work to another runner.
				active, activeErr := e.Control.JobActive(ctx, instance)
				if activeErr != nil {
					return e.fail(ctx, instance, StageGuard)
				}
				if active {
					return e.abort(ctx, instance)
				}
			case operations.DrainPhaseOccupancyBudget:
				// The occupancy budget planned this reclaim from a fact no fresh
				// evidence can disprove: the instance has held its profile's vector
				// past the ceiling configured for it. Every other phase re-verifies a
				// claim that no work is happening and aborts when work turns out to be
				// happening; here the work IS the premise, so an abort on busy evidence
				// would make the budget unenforceable by construction.
				//
				// That is also why the guest is powered off BEFORE deregistration
				// rather than after it. GitHub refuses to remove a runner it considers
				// to be executing a job, and it will go on considering this one busy
				// for as long as the hung job runs, so deregistering first can only
				// retry until the operation dead-letters with the vector still held.
				// Stopping the ephemeral guest is the same graceful `stop` every drain
				// already performs — no signal is sent to anything, the VM is asked to
				// power down — and it is what ends the job. GitHub then reports the
				// job as a lost-communication failure, which is what an operator sees
				// and what the drain's own operation record explains.
				if e.VM == nil {
					return e.fail(ctx, instance, StageStop)
				}
				if err := e.VM.Stop(ctx, instance.ID, instance.Ownership); err != nil {
					return e.fail(ctx, instance, StageStop)
				}
			default:
				// An event drain is issued when the demand this VM was spawned for
				// reaches JobCompleted. Because GitHub's scale-set brokering is
				// decoupled from the fleet's demand-keyed spawning, that premise does
				// NOT imply the runner is finished: GitHub may have handed this runner a
				// different matching job (2026-07-25 incident: a builder spawned for an
				// iOS App Store submission was given the Android job instead, and the
				// iOS completion then issued an event drain while the Android build was
				// still running). Re-verify against runner-scoped evidence before any
				// destructive step and abort when a job is executing; the guarded
				// lingering-runner recovery reclaims the instance once its runner is
				// genuinely idle.
				if proceed, result := e.verifyRunnerIdle(ctx, instance); !proceed {
					return result
				}
				safe, guardErr := e.Control.SafeToDeregister(ctx, instance)
				if guardErr != nil || !safe {
					return e.fail(ctx, instance, StageGuard)
				}
			}
			if deregisterErr := e.Control.Deregister(ctx, instance); deregisterErr != nil {
				// GitHub refuses to deregister a runner that is executing a job. That
				// refusal is not a transient fault to retry: paired with fresh busy
				// evidence it disproves the drain's premise, so abort exactly as the
				// recovery phases do. Without busy evidence the refusal stays a
				// retryable deregister-stage failure. This is what bounds the incident's
				// 60+ attempt kill loop for every drain phase, not just the event drain.
				//
				// An occupancy-budget reclaim is the one exception, for the reason its
				// phase exists: a busy runner does not disprove that premise, it is the
				// premise. Aborting here would return the instance to Running with its
				// vector still held and the next tick would plan the same reap forever.
				if instance.DrainPhase != operations.DrainPhaseOccupancyBudget {
					if busy, busyErr := e.Control.RunnerBusy(ctx, instance); busyErr == nil && busy {
						return e.abort(ctx, instance)
					}
				}
				// ADR 0007 keeps owned cleanup retrying through a refusal rather than
				// abandoning an owned VM, so this failure may repeat for hours. It must
				// therefore say WHY: one closed-vocabulary runner-administration reason
				// travels with the stage into the durable operation row, which is what
				// the 2026-07-25 incident's 397 identical attempts could not do.
				return classifiedError(StageDeregister, deregisterErr)
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

// verifyRunnerIdle re-verifies the premise every drain of a live runner
// registration shares: no workflow job is executing on the runner itself.
// proceed=false means Execute must return the accompanying result — an
// acknowledged abort when fresh evidence shows a job running, or a guard-stage
// retry when that evidence cannot be read. It stays fail-closed: an unreadable
// observation neither kills nor aborts on a guess.
func (e DrainExecutor) verifyRunnerIdle(ctx context.Context, instance operations.Instance) (bool, error) {
	busy, err := e.Control.RunnerBusy(ctx, instance)
	if err != nil {
		return false, e.fail(ctx, instance, StageGuard)
	}
	if busy {
		return false, e.abort(ctx, instance)
	}
	return true, nil
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

// classifiedError records a stage failure together with the closed-vocabulary
// reason classified from the adapter error. RunnerFailureDetail is total, so the
// cause itself is never persisted and never has to be.
func classifiedError(stage Stage, cause error) error {
	return stageError{stage: stage, reason: githubscaleset.RunnerFailureDetail(cause)}
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
		domain.ValidateInstanceName(instance.ID) != nil {
		return operations.ErrInvalid
	}
	return nil
}

func safeError(stage Stage) error { return stageError{stage: stage} }

// exhaustedError is the same bounded stage failure, marked as one no further
// retry can change, so the durable operation parks and becomes dischargeable.
func exhaustedError(stage Stage) error { return stageError{stage: stage, exhausted: true} }

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

func (b StdinBootstrapper) Bootstrap(ctx context.Context, name string, secret *githubscaleset.JITSecret,
	capabilities []string) error {
	if domain.ValidateInstanceName(name) != nil || secret == nil || b.Runner == nil {
		return operations.ErrInvalid
	}
	encoded := secret.Reveal()
	defer secret.Destroy()
	if encoded == "" || len(encoded) > maxJITBytes {
		return operations.ErrInvalid
	}
	args, err := capabilityArguments([]string{"exec", "-i", name, bootstrapHelper}, capabilities)
	if err != nil {
		return operations.ErrInvalid
	}
	timeout := b.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := b.Runner.Run(commandCtx, strings.NewReader(encoded+"\n"), args...); err != nil {
		if commandCtx.Err() != nil {
			return commandCtx.Err()
		}
		// The guest's own account of a failed capability check, carried by exit
		// status alone so no child output ever reaches a durable row.
		if reason := guestCapabilityReason(err); reason != "" {
			return stageError{stage: StageBootstrap, reason: reason}
		}
		return errors.New("runner bootstrap failed")
	}
	return nil
}
