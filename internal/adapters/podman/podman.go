// Package podman is node B's execution technology: one rootless, unprivileged,
// ephemeral container per job, driven through the `podman` command line.
//
// It is the second implementation of `executor.Backend` (ADR 0034 §1, issue
// #139) and it deliberately has the same shape as `internal/adapters/tart`: a
// CLI-shelling adapter whose only process primitive is an argument vector, whose
// every mutating verb re-checks durable ownership before it acts, and whose
// failures are classified into the same bounded vocabulary so the layers above
// cannot tell the two nodes apart.
//
//	Create   podman create --name <n> --cpus <c> --memory <m>m --label ... <image> <hold>
//	Start    podman start <n>
//	Stop     podman stop --time <s> <n>
//	Delete   podman rm --force <n>
//	Reap     podman rm <n>                     (operator authority, refuses running)
//	Running  podman ps --all --format json     (through List)
//	List     podman ps --all --format json
//
// Three things are true here that are not true of Tart, and each one is a
// deliberate choice rather than an omission.
//
// A container is created, never adopted. Tart clones a base VM and can find its
// own clone again after a lost response, because a Tart VM is a directory that
// survives the process that made it. A container that already holds a name the
// scheduler just minted was made by something else, or by an incarnation whose
// job has already run in it, and ADR 0010 forbids reuse either way. So Create
// treats a name collision as its own: only a container carrying this operation's
// `trf.operation` label is one this call may leave standing.
//
// The container must idle. `lifecycle.StdinBootstrapper` pipes the JIT runner
// configuration into the guest with `podman exec -i`, and `exec` needs a running
// container, so the image's process is a hold command (`sleep infinity` by
// default) that does nothing until the bootstrap helper is executed inside it.
// That is why the runner never starts from the image entrypoint.
//
// `/dev/kvm` is granted to named profiles only. `executor.InstanceSpec` carries
// no profile — `tests/contract/executor_port_test.go` pins its field set, and a
// field there is a field every backend must answer for — so the grant is derived
// from the instance-name prefix `trf-<profile>-` minted by
// `reconcile.Controller`, which is exactly how the Tart adapter already
// recognises a macOS guest. ADR 0034 and docs/MULTI_NODE_PLAN.md give the device
// to the Android emulator profile and to nothing else.
package podman

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/executor"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// ErrorKind is the bounded failure vocabulary this adapter classifies a podman
// exit into. It is deliberately the same set of distinctions the Tart adapter
// draws, because the lifecycle above both backends retries and parks on the
// distinction and not on the runtime.
type ErrorKind string

const (
	ErrorTimeout      ErrorKind = "timeout"
	ErrorNotFound     ErrorKind = "not_found"
	ErrorAlreadyExist ErrorKind = "already_exists"
	ErrorPermission   ErrorKind = "permission"
	ErrorCommand      ErrorKind = "command"
	ErrorUncertain    ErrorKind = "uncertain"
	// ErrorUnhealthy is a container runtime that is absent, unusable, or running
	// as root. It is separate from ErrorPermission because the remedy is an
	// operator installing or fixing podman on the node, not a permission on one
	// object.
	ErrorUnhealthy ErrorKind = "unhealthy"
)

type Error struct {
	Op       string
	Kind     ErrorKind
	ExitCode int
	Stderr   string
	Err      error
}

func (e *Error) Error() string {
	return fmt.Sprintf("podman %s: %s: %v", e.Op, e.Kind, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// failure builds this adapter's error for a fault it diagnosed itself, rather
// than for a command that exited. Every such fault is reported with a -1 exit
// code, because there was no process to take one from.
func failure(op string, kind ErrorKind, err error) *Error {
	return &Error{Op: op, Kind: kind, ExitCode: -1, Err: err}
}

// ExecRunner is the production `podman` command line. It is an
// executor.CommandRunner and nothing more: unlike Tart's runner it needs no
// detached-start primitive, because `podman start` returns once the container is
// running instead of blocking for the guest's lifetime.
type ExecRunner struct {
	Binary string
}

// DefaultBinary is the podman executable an unconfigured adapter shells. It is
// a bare name resolved through PATH on purpose: rootless podman is installed
// per-distribution and the fleet does not get to decide where.
const DefaultBinary = "podman"

func (r ExecRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	binary := r.Binary
	if binary == "" {
		binary = DefaultBinary
	}
	// #nosec G204 -- Binary is a trusted adapter dependency; every external name
	// in args is validated before it gets here and nothing passes through a shell.
	output, err := exec.CommandContext(ctx, binary, args...).CombinedOutput()
	if err != nil {
		return output, classify(args, output, err, ctx.Err())
	}
	return output, nil
}

// podmanErrorKinds maps a fragment of podman's own stderr onto this adapter's
// vocabulary. The table is ordered, because "no such container" and "permission
// denied" can both appear in one message and the more specific fact wins.
//
// The fragments are podman's own, not Docker's: `no such container` and `no
// container with name or ID` for an absent object, `the container name "x" is
// already in use` for a collision, and `cannot find newuidmap` for a host whose
// rootless user namespaces are not set up.
var podmanErrorKinds = []struct {
	fragment string
	kind     ErrorKind
}{
	{"no such container", ErrorNotFound},
	{"no container with name or id", ErrorNotFound},
	{"no such object", ErrorNotFound},
	{"already in use", ErrorAlreadyExist},
	{"container already exists", ErrorAlreadyExist},
	{"permission denied", ErrorPermission},
	{"not permitted", ErrorPermission},
	{"cannot find newuidmap", ErrorUnhealthy},
	{"cannot connect to podman", ErrorUnhealthy},
	{"executable file not found", ErrorUnhealthy},
}

// classify turns one failed podman invocation into a bounded Error. A context
// deadline outranks every stderr fragment: a command the fleet cut short says
// nothing reliable about the container, and calling that "not found" would let a
// slow host masquerade as a clean absence.
func classify(args []string, output []byte, err, contextErr error) error {
	kind := ErrorCommand
	if errors.Is(contextErr, context.DeadlineExceeded) || errors.Is(contextErr, context.Canceled) {
		kind = ErrorTimeout
	} else if errors.Is(err, exec.ErrNotFound) {
		kind = ErrorUnhealthy
	} else {
		text := strings.ToLower(string(output))
		for _, candidate := range podmanErrorKinds {
			if strings.Contains(text, candidate.fragment) {
				kind = candidate.kind
				break
			}
		}
	}
	exitCode := -1
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		exitCode = exitError.ExitCode()
	}
	return &Error{Op: strings.Join(args, " "), Kind: kind, ExitCode: exitCode, Stderr: string(output), Err: err}
}

// OwnershipRegistry is the durable proof that this controller owns an instance.
// It is written before the container exists and re-read before every mutation,
// so a lost `podman create` response can never orphan a container the fleet
// cannot later prove is its own.
type OwnershipRegistry interface {
	PutOwnership(context.Context, string, operations.Ownership) error
	Ownership(context.Context, string) (operations.Ownership, error)
}

// ConfirmationProvider is the runner-inactive evidence Delete requires. Reap
// deliberately does not consult it; see reap.go.
type ConfirmationProvider interface {
	ConfirmDeletion(context.Context, string) (operations.DeletionConfirmation, error)
}

// container is one row of `podman ps --all --format json`. It stays unexported:
// callers see executor.Instance, and decoding one CLI's JSON is nobody else's
// business.
//
// `Names` is a list because a container may carry several; the fleet creates
// each with exactly one, and a container whose names do not include a name the
// fleet minted is simply not one of ours.
type container struct {
	Names  []string          `json:"Names"`
	Image  string            `json:"Image"`
	State  string            `json:"State"`
	Labels map[string]string `json:"Labels"`
}

// running reports whether podman's textual state means the container is
// executing. Podman reports `running` and `paused` for a live container and
// `created`, `exited`, `stopped`, `removing`, and `dead` for one that is not; a
// paused container still holds its resources, so it counts as running.
func (c container) running() bool {
	state := strings.ToLower(strings.TrimSpace(c.State))
	return state == "running" || state == "paused"
}

// info is the subset of `podman info --format json` that decides whether this
// node may execute anything at all.
type info struct {
	Host struct {
		Security struct {
			Rootless bool `json:"rootless"`
		} `json:"security"`
	} `json:"host"`
}

// Adapter is the Podman implementation of executor.Backend.
type Adapter struct {
	Runner       executor.CommandRunner
	Ownership    OwnershipRegistry
	Confirmation ConfirmationProvider
	// Image is the OCI reference every container is created from. It is also
	// carried in executor.InstanceSpec.Image; the adapter uses the spec and keeps
	// this field only so a node's configured image can be validated once, at
	// start, rather than on the first job.
	Image string
	// HoldCommand is the container's process: something that stays alive and does
	// nothing, so `podman exec` can bootstrap the runner inside it. Empty means
	// defaultHoldCommand.
	HoldCommand []string
	// KVMInstancePrefixes are the `trf-<profile>-` prefixes whose containers are
	// granted `--device /dev/kvm`. ADR 0034 gives the device to the Android
	// emulator profile and to no other, so this is a list of names and never a
	// boolean.
	KVMInstancePrefixes []string
	CommandTimeout      time.Duration
	StopTimeout         time.Duration
	ConfirmationMaxAge  time.Duration
	Now                 func() time.Time

	mu      sync.Mutex
	healthy bool
}

// defaultHoldCommand keeps the container alive with no runner in it. `sleep
// infinity` is coreutils, present in every Linux runner image the fleet will
// ever build, and it exits immediately when the container is stopped.
var defaultHoldCommand = []string{"sleep", "infinity"}

// ownershipLabelPrefix namespaces the labels this adapter writes onto every
// container it creates. The labels are not the fleet's proof of ownership — the
// durable registry is — but they let an operator read a container's owner off
// the host with `podman inspect` during an incident, which is exactly the
// question `docs/OPERATIONS.md` asks first.
const ownershipLabelPrefix = "trf."

// validateName applies the fleet-wide instance grammar of
// domain.ValidateInstanceName, which tests/contract asserts is a subset of
// podman's own container-name grammar, and reports a rejection as an invalid
// operation rather than as a command failure.
func validateName(name string) error {
	if domain.ValidateInstanceName(name) != nil {
		return fmt.Errorf("%w: invalid container name", operations.ErrInvalid)
	}
	return nil
}

// ValidateImage checks an OCI reference the way this adapter needs it checked:
// not for registry correctness, which only a registry can answer, but for the
// two properties that make a reference safe to put in an argument vector — it is
// non-empty, and it cannot be read as an option.
//
// It is exported because `internal/config` validates a node's configured image
// at decode time, so a typo is a refused configuration rather than a profile
// that never provisions.
func ValidateImage(image string) error {
	if strings.TrimSpace(image) != image || image == "" {
		return errors.New("container image reference is empty or padded")
	}
	if strings.HasPrefix(image, "-") {
		return errors.New("container image reference cannot begin with a dash")
	}
	if strings.ContainsAny(image, " \t\n\r\x00") {
		return errors.New("container image reference contains whitespace")
	}
	return nil
}

// Healthy is the fail-closed gate on a node whose execution technology may
// simply not be installed. It asks podman to describe itself and requires two
// answers: the command must succeed, and it must report a rootless runtime.
//
// Rootless is not a preference. ADR 0034 and docs/MULTI_NODE_PLAN.md put
// approved third-party code on this node, and a root-ful podman would run it
// with a root-owned daemon and a root-equivalent group. A node that cannot prove
// it is rootless refuses to execute rather than executing less safely than it
// promised.
//
// Success is remembered, so the probe costs one process for the life of the
// daemon; a failure is not, so a podman that comes back is picked up on the next
// attempt.
func (a *Adapter) Healthy(ctx context.Context) error {
	a.mu.Lock()
	cached := a.healthy
	a.mu.Unlock()
	if cached {
		return nil
	}
	commandCtx, cancel := context.WithTimeout(ctx, a.timeout())
	output, err := a.runner().Run(commandCtx, "info", "--format", "json")
	cancel()
	if err != nil {
		return failure("info", ErrorUnhealthy, err)
	}
	var described info
	if err := json.Unmarshal(output, &described); err != nil {
		return failure("info", ErrorUnhealthy, errors.New("podman info is not readable JSON"))
	}
	if !described.Host.Security.Rootless {
		return failure("info", ErrorUnhealthy, errors.New("podman is not rootless; this node refuses to run untrusted jobs as root"))
	}
	a.mu.Lock()
	a.healthy = true
	a.mu.Unlock()
	return nil
}

// List enumerates every container on this host, owned or not, so the fleet can
// detect an owned container that vanished and an untracked one it never created.
func (a *Adapter) List(ctx context.Context) ([]executor.Instance, error) {
	containers, err := a.containers(ctx)
	if err != nil {
		return nil, err
	}
	instances := make([]executor.Instance, 0, len(containers))
	for _, item := range containers {
		for _, name := range item.Names {
			instances = append(instances, executor.Instance{Name: name, Running: item.running(), Source: item.Image})
		}
	}
	return instances, nil
}

// containers is one reading of the whole host. Podman prints `[]` for an empty
// host on recent versions and the bare word `null` on some older ones; both are
// the same measurement — no containers — and neither is an unreadable answer, so
// both decode to an empty slice. Anything else is uncertain rather than empty,
// because an unreadable observation is not a measurement (AGENTS.md §4).
func (a *Adapter) containers(ctx context.Context) ([]container, error) {
	commandCtx, cancel := context.WithTimeout(ctx, a.timeout())
	output, err := a.runner().Run(commandCtx, "ps", "--all", "--format", "json")
	cancel()
	if err != nil {
		return nil, err
	}
	var decoded []container
	if err := json.Unmarshal(output, &decoded); err != nil {
		return nil, &Error{Op: "ps", Kind: ErrorUncertain, ExitCode: -1, Stderr: string(output), Err: err}
	}
	return decoded, nil
}

// Create brings one ephemeral container into existence from spec.Image and
// leaves it created but not started, sized to the spec.
//
// Ownership is recorded before the container exists, so a lost response cannot
// orphan it. The verb is idempotent for a container this controller already
// owns, and refuses one it does not: unlike a Tart clone, a container of this
// name that the fleet cannot prove is its own was made by something else, and
// adopting it would put a job into a filesystem with unknown history.
func (a *Adapter) Create(ctx context.Context, spec executor.InstanceSpec) error {
	if err := validateName(spec.Name); err != nil {
		return err
	}
	if err := ValidateImage(spec.Image); err != nil {
		return fmt.Errorf("%w: %s", operations.ErrInvalid, err)
	}
	if !spec.Ownership.Valid() || spec.CPU <= 0 || spec.MemoryMB <= 0 {
		return operations.ErrInvalid
	}
	if err := a.Healthy(ctx); err != nil {
		return err
	}
	if err := a.Ownership.PutOwnership(ctx, spec.Name, spec.Ownership); err != nil {
		return err
	}
	exists, err := a.isOurContainer(ctx, spec)
	if err != nil || exists {
		return err
	}
	commandCtx, cancel := context.WithTimeout(ctx, a.timeout())
	_, commandErr := a.runner().Run(commandCtx, a.createArgs(spec)...)
	cancel()
	if commandErr == nil {
		return nil
	}
	// A failed create is re-observed before it is believed: podman may have
	// created the container and failed to report it. An unreadable host stays
	// uncertain rather than becoming a second create attempt on a name that may
	// already exist.
	exists, observeErr := a.isOurContainer(ctx, spec)
	if observeErr != nil {
		return failure("create", ErrorUncertain, errors.Join(commandErr, observeErr))
	}
	if exists {
		return nil
	}
	return commandErr
}

// isOurContainer reports whether a container of this name already exists AND
// this exact operation created it. Together with the durable ownership write
// above it, it is the whole of the no-reuse rule.
//
// The durable record alone cannot answer the question. It is written before the
// container exists, so immediately afterwards it matches a container the fleet
// may never have made — and a container that already holds the name a scheduler
// just minted was put there by something outside the fleet, or by a previous
// operation whose job has already run in it. Either way its filesystem has
// unknown history, and ADR 0010 forbids reuse. So the container's own
// `trf.operation` label, which this adapter's create writes and nothing else
// does, is compared to the operation asking for it, and a mismatch is a conflict
// rather than an adoption.
func (a *Adapter) isOurContainer(ctx context.Context, spec executor.InstanceSpec) (bool, error) {
	found, err := a.findContainer(ctx, spec.Name)
	if errors.Is(err, operations.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if found.Labels[ownershipLabelPrefix+"operation"] != spec.Ownership.OperationID {
		return false, fmt.Errorf("%w: a container named %s already exists and this operation did not create it",
			operations.ErrConflict, spec.Name)
	}
	return true, nil
}

// createArgs is the whole container specification in one argument vector, which
// is what makes this adapter auditable: everything a job's container is allowed
// to do is on this line and nowhere else.
func (a *Adapter) createArgs(spec executor.InstanceSpec) []string {
	args := []string{"create", "--name", spec.Name,
		"--cpus", strconv.Itoa(spec.CPU),
		"--memory", strconv.Itoa(spec.MemoryMB) + "m",
		// The runner process is the container's reason to exist; when the job's
		// container is stopped nothing about it is worth restarting, and ADR 0010
		// says an ephemeral guest never comes back.
		"--restart", "no",
		// An init process reaps the zombies a CI job's tool-chain leaves behind,
		// which would otherwise accumulate under the hold command as PID 1.
		"--init",
	}
	for _, label := range ownershipLabels(spec.Ownership) {
		args = append(args, "--label", label)
	}
	if a.grantsKVM(spec.Name) {
		// ADR 0034: the Android emulator profile is the one workload that needs
		// hardware acceleration, and it gets a device grant rather than a
		// privileged container.
		args = append(args, "--device", "/dev/kvm")
	}
	args = append(args, spec.Image)
	return append(args, a.holdCommand()...)
}

// ownershipLabels renders the durable ownership triple as container labels, in a
// fixed order so one argument vector is one deterministic string in a test and
// in an operator's shell history.
func ownershipLabels(ownership operations.Ownership) []string {
	return []string{
		ownershipLabelPrefix + "controller=" + ownership.ControllerID,
		ownershipLabelPrefix + "resource=" + ownership.ResourceID,
		ownershipLabelPrefix + "operation=" + ownership.OperationID,
	}
}

// grantsKVM reports whether this instance's profile is one ADR 0034 grants
// /dev/kvm. The profile is read from the `trf-<profile>-` prefix
// reconcile.Controller mints, which is the same inference the Tart adapter makes
// to recognise a macOS guest.
func (a *Adapter) grantsKVM(name string) bool {
	for _, prefix := range a.KVMInstancePrefixes {
		if prefix != "" && strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// Start powers the container on and returns only once podman confirms it is
// running. Unlike `tart run`, `podman start` is not a process that lives as long
// as the guest, so there is no detached start and no poll loop here: the command
// returns when the container is up, and a fresh observation proves it.
func (a *Adapter) Start(ctx context.Context, name string, ownership operations.Ownership) error {
	if err := validateName(name); err != nil {
		return err
	}
	instance, err := a.ownedContainer(ctx, name, ownership)
	if err != nil {
		return err
	}
	if instance.Running {
		return nil
	}
	commandCtx, cancel := context.WithTimeout(ctx, a.timeout())
	_, commandErr := a.runner().Run(commandCtx, "start", name)
	cancel()
	instance, observeErr := a.ownedContainer(ctx, name, ownership)
	if observeErr == nil && instance.Running {
		return nil
	}
	if observeErr != nil {
		return failure("start", ErrorUncertain, errors.Join(commandErr, observeErr))
	}
	if commandErr != nil {
		return commandErr
	}
	return failure("start", ErrorUncertain, errors.New("podman start reported success but the container is not running"))
}

// Stop powers the container off. An absent container is success, because the
// fleet's durable cleanup depends on retrying this verb (ADR 0007).
func (a *Adapter) Stop(ctx context.Context, name string, ownership operations.Ownership) error {
	if err := validateName(name); err != nil {
		return err
	}
	instance, err := a.ownedContainer(ctx, name, ownership)
	if errors.Is(err, operations.ErrNotFound) {
		return nil
	}
	if err != nil || !instance.Running {
		return err
	}
	commandCtx, cancel := context.WithTimeout(ctx, a.timeout())
	_, commandErr := a.runner().Run(commandCtx, "stop", "--time", strconv.Itoa(a.stopSeconds()), name)
	cancel()
	if commandErr == nil {
		return nil
	}
	instance, observeErr := a.ownedContainer(ctx, name, ownership)
	if errors.Is(observeErr, operations.ErrNotFound) || (observeErr == nil && !instance.Running) {
		return nil
	}
	if observeErr != nil {
		return failure("stop", ErrorUncertain, errors.Join(commandErr, observeErr))
	}
	return commandErr
}

// Delete removes the container after fresh deletion confirmation. An absent
// container is success.
//
// `podman rm --force` stops a running container itself, but this verb still
// stops first and then re-confirms: the confirmation is evidence that no runner
// and no job is live, and taking it once before a stop would let a job that
// started in between be killed by the force flag.
func (a *Adapter) Delete(ctx context.Context, name string, ownership operations.Ownership) error {
	if err := validateName(name); err != nil {
		return err
	}
	instance, err := a.ownedContainer(ctx, name, ownership)
	if errors.Is(err, operations.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if a.Confirmation == nil {
		return failure("rm", ErrorUncertain, operations.ErrUncertain)
	}
	confirmation, err := a.Confirmation.ConfirmDeletion(ctx, name)
	if err != nil {
		return err
	}
	if !confirmation.Safe(a.now(), a.confirmationMaxAge()) {
		return failure("rm", ErrorUncertain, operations.ErrUncertain)
	}
	if instance.Running {
		if err := a.Stop(ctx, name, ownership); err != nil {
			return err
		}
		confirmation, err = a.Confirmation.ConfirmDeletion(ctx, name)
		if err != nil || !confirmation.Safe(a.now(), a.confirmationMaxAge()) {
			return failure("rm", ErrorUncertain, errors.Join(err, operations.ErrUncertain))
		}
	}
	return a.remove(ctx, "rm", name, "rm", "--force", name)
}

// remove runs one podman removal and re-observes the host when it fails, so a
// container that is gone is success no matter which way it went. It is shared by
// Delete and Reap because the difference between those verbs is entirely in the
// evidence they demand beforehand, never in the removal itself.
func (a *Adapter) remove(ctx context.Context, op, name string, args ...string) error {
	commandCtx, cancel := context.WithTimeout(ctx, a.timeout())
	_, commandErr := a.runner().Run(commandCtx, args...)
	cancel()
	if commandErr == nil {
		return nil
	}
	_, observeErr := a.find(ctx, name)
	if errors.Is(observeErr, operations.ErrNotFound) {
		return nil
	}
	if observeErr != nil {
		return failure(op, ErrorUncertain, errors.Join(commandErr, observeErr))
	}
	return commandErr
}

// Running reports the container's current power state; an absent container is
// not running.
func (a *Adapter) Running(ctx context.Context, name string) (bool, error) {
	instance, err := a.find(ctx, name)
	if errors.Is(err, operations.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return instance.Running, nil
}

// ownedContainer resolves a name to a live observation only after the durable
// ownership record matches. The order matters: a container the fleet does not
// own is a conflict whether or not it exists, so ownership is checked before the
// host is read.
func (a *Adapter) ownedContainer(ctx context.Context, name string, expected operations.Ownership) (executor.Instance, error) {
	actual, err := a.Ownership.Ownership(ctx, name)
	if err != nil {
		return executor.Instance{}, err
	}
	if actual != expected {
		return executor.Instance{}, operations.ErrConflict
	}
	return a.find(ctx, name)
}

func (a *Adapter) find(ctx context.Context, name string) (executor.Instance, error) {
	found, err := a.findContainer(ctx, name)
	if err != nil {
		return executor.Instance{}, err
	}
	return executor.Instance{Name: name, Running: found.running(), Source: found.Image}, nil
}

func (a *Adapter) findContainer(ctx context.Context, name string) (container, error) {
	containers, err := a.containers(ctx)
	if err != nil {
		return container{}, err
	}
	for _, item := range containers {
		for _, candidate := range item.Names {
			if candidate == name {
				return item, nil
			}
		}
	}
	return container{}, operations.ErrNotFound
}

func (a *Adapter) timeout() time.Duration {
	if a.CommandTimeout <= 0 {
		return 30 * time.Second
	}
	return a.CommandTimeout
}

// stopSeconds is how long podman lets the container's process finish before it
// is killed. It is whole seconds because that is podman's own `--time` unit.
func (a *Adapter) stopSeconds() int {
	if a.StopTimeout <= 0 {
		return 10
	}
	seconds := int(a.StopTimeout / time.Second)
	if seconds < 1 {
		return 1
	}
	return seconds
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

func (a *Adapter) runner() executor.CommandRunner {
	if a.Runner == nil {
		return ExecRunner{}
	}
	return a.Runner
}

func (a *Adapter) holdCommand() []string {
	if len(a.HoldCommand) == 0 {
		return defaultHoldCommand
	}
	return a.HoldCommand
}
