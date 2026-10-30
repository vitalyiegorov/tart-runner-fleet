// Package hostprocess is the execution technology of a host-process node (ADR
// 0090): one ephemeral just-in-time runner per job, run as a process on the Mac
// itself under a dedicated runner user, with the job leasing its own simulator
// from the host's simlock worker.
//
// The daemon never runs a job directly. Every verb is one argument vector,
//
//	sudo -n -u <runnerUser> -- <launcher> <verb> ...
//
// and the root-installed launcher (internal/hostlauncher) does the work as the
// runner user. So this adapter is a translation, not a second state machine:
// the launcher is idempotent per verb, owns the per-instance directories, and
// reports power from a lock the job's processes hold. What stays here is what
// must stay on the daemon's side of the boundary: the durable ownership record,
// written before the instance exists and re-checked before every mutation, and
// the fresh runner-inactive confirmation Delete requires (ADR 0007, 0042).
//
//	Create    launcher create <n> <template> <controller> <resource> <operation>
//	Start     launcher start <n> <operation>
//	Stop      launcher stop <n> <operation> <grace seconds>
//	Terminate launcher stop <n> <operation> 0
//	Delete    launcher delete <n> <operation>       (after fresh confirmation)
//	Reap      launcher delete <n> <operation>       (operator authority, refuses running)
//	Power     launcher list
//	List      launcher list
package hostprocess

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/executor"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/hostlauncher"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// SudoPath is the one sudo the daemon invokes; it is not resolved through PATH.
const SudoPath = "/usr/bin/sudo"

// Runner is the launcher behind sudo. It is an executor.CommandRunner, so the
// lifecycle's readiness poll and guest-liveness probe (`exec <n> true`) run
// through it unchanged.
type Runner struct {
	User     string
	Launcher string
	// Sudo overrides SudoPath; tests point it at a stub.
	Sudo string
}

func (r Runner) command(ctx context.Context, args []string) *exec.Cmd {
	sudo := r.Sudo
	if sudo == "" {
		sudo = SudoPath
	}
	argv := append([]string{"-n", "-u", r.User, "--", r.Launcher}, args...)
	// #nosec G204 -- a fixed sudo, a validated user and launcher path, and
	// arguments the adapter assembled from validated names. No shell.
	return exec.CommandContext(ctx, sudo, argv...)
}

// Run executes one launcher verb and returns its standard output. A failure
// keeps the launcher's exit status, which is all the adapter classifies on.
func (r Runner) Run(ctx context.Context, args ...string) ([]byte, error) {
	return r.command(ctx, args).Output()
}

// StdinRunner is the same launcher as the lifecycle's JIT bootstrap channel
// (lifecycle.StdinRunner). The configuration is written to sudo's standard input
// and from there to the launcher's; it never appears in an argument vector, an
// environment, or a file, and the launcher's output is discarded because nothing
// it could say is needed here.
type StdinRunner struct{ Runner Runner }

func (r StdinRunner) Run(ctx context.Context, stdin io.Reader, args ...string) error {
	command := r.Runner.command(ctx, args)
	command.Stdin = stdin
	return command.Run()
}

// OwnershipRegistry is the durable proof this controller owns an instance.
type OwnershipRegistry interface {
	PutOwnership(context.Context, string, operations.Ownership) error
	Ownership(context.Context, string) (operations.Ownership, error)
}

// ConfirmationProvider is the fresh runner-inactive evidence Delete requires.
type ConfirmationProvider interface {
	ConfirmDeletion(context.Context, string) (operations.DeletionConfirmation, error)
}

// Adapter is the host-process implementation of executor.Backend.
type Adapter struct {
	Runner             executor.CommandRunner
	Ownership          OwnershipRegistry
	Confirmation       ConfirmationProvider
	CommandTimeout     time.Duration
	StopTimeout        time.Duration
	ConfirmationMaxAge time.Duration
	Now                func() time.Time
}

// geteuid is the daemon's own effective user, read once by Healthy.
var geteuid = os.Geteuid

// Healthy is the preflight a host-process node passes before any mode above
// observe: the daemon is not root, and one `list` succeeds — which proves sudo
// admits the daemon to the launcher as the runner user without a password, the
// launcher's root-owned configuration loads, and the launcher is not being run
// by the runner user itself.
func (a *Adapter) Healthy(ctx context.Context) error {
	if geteuid() == 0 {
		return errors.New("a host-process node refuses to run its daemon as root")
	}
	_, err := a.List(ctx)
	return err
}

// launcherError maps a launcher exit status onto the fleet's bounded
// vocabulary. The launcher's text is kept for an operator but decides nothing.
func launcherError(verb string, err error) error {
	if err == nil {
		return nil
	}
	sentinel := err
	var status interface{ ExitCode() int }
	if errors.As(err, &status) {
		switch status.ExitCode() {
		case hostlauncher.ExitNotFound:
			sentinel = operations.ErrNotFound
		case hostlauncher.ExitConflict, hostlauncher.ExitRunning:
			sentinel = operations.ErrConflict
		case hostlauncher.ExitUsage:
			sentinel = operations.ErrInvalid
		}
	}
	return fmt.Errorf("host launcher %s: %w", verb, errors.Join(sentinel, err))
}

func (a *Adapter) run(ctx context.Context, extra time.Duration, args ...string) ([]byte, error) {
	timeout := a.CommandTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	commandCtx, cancel := context.WithTimeout(ctx, timeout+extra)
	defer cancel()
	output, err := a.Runner.Run(commandCtx, args...)
	return output, launcherError(args[0], err)
}

// List reports every instance under the launcher's root. A row whose power the
// launcher could not read is InstancePowerUnknown with a reason, never stopped.
func (a *Adapter) List(ctx context.Context) ([]executor.Instance, error) {
	output, err := a.run(ctx, 0, "list")
	var entries []hostlauncher.Entry
	if err == nil {
		err = json.Unmarshal(output, &entries)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", operations.ErrUncertain, err)
	}
	instances := make([]executor.Instance, 0, len(entries))
	for _, entry := range entries {
		instance := executor.Instance{Name: entry.Name, Source: entry.Image}
		switch entry.Power {
		case hostlauncher.PowerRunning:
			instance.Power = domain.InstancePowerRunning
		case hostlauncher.PowerStopped:
			instance.Power = domain.InstancePowerStopped
		default:
			instance.Unreadable = domain.PowerReadFailure{Reason: domain.PowerReadOther}
		}
		instances = append(instances, instance)
	}
	return instances, nil
}

// Power reports one instance's power; an instance the launcher does not list is
// proven absent, and an unreadable listing is unknown.
func (a *Adapter) Power(ctx context.Context, name string) (domain.InstancePower, error) {
	instances, err := a.List(ctx)
	if err != nil {
		return domain.InstancePowerUnknown, err
	}
	for _, instance := range instances {
		if instance.Name == name {
			return instance.Power, nil
		}
	}
	return domain.InstancePowerAbsent, nil
}

// Create records ownership, then has the launcher clone the runner template
// into a fresh instance. The launcher's create is idempotent for the operation
// that made the instance and refuses any other, so a lost response is retried
// rather than re-observed here.
func (a *Adapter) Create(ctx context.Context, spec executor.InstanceSpec) error {
	if domain.ValidateInstanceName(spec.Name) != nil || !filepath.IsAbs(spec.Image) ||
		filepath.Clean(spec.Image) != spec.Image || !spec.Ownership.Valid() || spec.CPU <= 0 || spec.MemoryMB <= 0 {
		return operations.ErrInvalid
	}
	if err := a.Ownership.PutOwnership(ctx, spec.Name, spec.Ownership); err != nil {
		return err
	}
	_, err := a.run(ctx, 0, "create", spec.Name, spec.Image, spec.Ownership.ControllerID,
		spec.Ownership.ResourceID, spec.Ownership.OperationID)
	return err
}

// owned re-reads the durable ownership record before any mutation. The
// operation it names is also what the launcher checks against the instance's
// own marker, so neither side can act on an instance the other did not make.
func (a *Adapter) owned(ctx context.Context, name string, expected operations.Ownership) error {
	if domain.ValidateInstanceName(name) != nil {
		return operations.ErrInvalid
	}
	actual, err := a.Ownership.Ownership(ctx, name)
	if err == nil && actual != expected {
		err = operations.ErrConflict
	}
	return err
}

// Start spawns the instance's supervisor and returns once it holds the
// instance. A running instance is success; a finished one is a conflict,
// because an ephemeral instance never runs a second job (ADR 0010).
func (a *Adapter) Start(ctx context.Context, name string, ownership operations.Ownership) error {
	if err := a.owned(ctx, name, ownership); err != nil {
		return err
	}
	_, err := a.run(ctx, 0, "start", name, ownership.OperationID)
	return err
}

// Stop sends the process group SIGTERM, waits the grace period, then SIGKILL.
// An absent or stopped instance is success.
func (a *Adapter) Stop(ctx context.Context, name string, ownership operations.Ownership) error {
	return a.stop(ctx, name, ownership, a.grace())
}

// Terminate is Stop with no grace: SIGKILL to the group (ADR 0039).
func (a *Adapter) Terminate(ctx context.Context, name string, ownership operations.Ownership) error {
	return a.stop(ctx, name, ownership, 0)
}

func (a *Adapter) stop(ctx context.Context, name string, ownership operations.Ownership, grace time.Duration) error {
	if err := a.owned(ctx, name, ownership); err != nil {
		return err
	}
	_, err := a.run(ctx, grace, "stop", name, ownership.OperationID, strconv.Itoa(int(grace/time.Second)))
	return err
}

// Destroy terminates and deletes, under exactly Delete's evidence.
func (a *Adapter) Destroy(ctx context.Context, name string, ownership operations.Ownership) error {
	if err := a.Terminate(ctx, name, ownership); err != nil {
		return err
	}
	return a.Delete(ctx, name, ownership)
}

// Delete removes the instance after fresh deletion confirmation. The
// confirmation is taken before the stop and again after it, so a job that
// started in between can never be ended by the deletion.
func (a *Adapter) Delete(ctx context.Context, name string, ownership operations.Ownership) error {
	if err := a.owned(ctx, name, ownership); err != nil {
		return err
	}
	power, err := a.Power(ctx, name)
	if err != nil || power == domain.InstancePowerAbsent {
		return err
	}
	for _, step := range []func() error{a.confirmed(ctx, name), func() error { return a.Stop(ctx, name, ownership) },
		a.confirmed(ctx, name)} {
		if err := step(); err != nil {
			return err
		}
	}
	_, err = a.run(ctx, 0, "delete", name, ownership.OperationID)
	return err
}

func (a *Adapter) confirmed(ctx context.Context, name string) func() error {
	return func() error {
		if a.Confirmation == nil {
			return operations.ErrUncertain
		}
		confirmation, err := a.Confirmation.ConfirmDeletion(ctx, name)
		if err == nil && !confirmation.Safe(a.now(), a.confirmationMaxAge()) {
			err = operations.ErrUncertain
		}
		return err
	}
}

// Reap removes a stopped, owned instance under operator authority, without the
// runner-inactive confirmation a leaked registration can never supply. It never
// stops anything: the launcher's delete refuses a running instance, and an
// instance whose power cannot be read is refused here.
func (a *Adapter) Reap(ctx context.Context, name string, ownership operations.Ownership) error {
	if err := a.owned(ctx, name, ownership); err != nil {
		return err
	}
	power, err := a.Power(ctx, name)
	if err == nil && power != domain.InstancePowerStopped && power != domain.InstancePowerAbsent {
		err = operations.ErrConflict
	}
	if err != nil {
		return err
	}
	_, err = a.run(ctx, 0, "delete", name, ownership.OperationID)
	return err
}

func (a *Adapter) grace() time.Duration {
	if a.StopTimeout < time.Second {
		return 10 * time.Second
	}
	return a.StopTimeout.Truncate(time.Second)
}

func (a *Adapter) confirmationMaxAge() time.Duration {
	if a.ConfirmationMaxAge <= 0 {
		return 30 * time.Second
	}
	return a.ConfirmationMaxAge
}

func (a *Adapter) now() time.Time {
	if a.Now == nil {
		return time.Now().UTC()
	}
	return a.Now().UTC()
}
