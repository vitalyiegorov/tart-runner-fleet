package tart

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/executor"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

type ErrorKind string

const (
	ErrorTimeout      ErrorKind = "timeout"
	ErrorNotFound     ErrorKind = "not_found"
	ErrorAlreadyExist ErrorKind = "already_exists"
	ErrorPermission   ErrorKind = "permission"
	ErrorCommand      ErrorKind = "command"
	ErrorUncertain    ErrorKind = "uncertain"
	ErrorHostQuota    ErrorKind = "host_quota"
)

type Error struct {
	Op       string
	Kind     ErrorKind
	ExitCode int
	Stderr   string
	Err      error
}

func (e *Error) Error() string {
	return fmt.Sprintf("tart %s: %s: %v", e.Op, e.Kind, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// StartedCommand is a handle to a detached, already-started process. It lets
// callers poll for an early exit (and inspect its bounded output) without
// blocking on the process for the lifetime of the VM.
type StartedCommand interface {
	Exited() bool
	Output() []byte
}

// Runner is the tart command line: the neutral argument-vector primitive every
// CLI-shelling backend shares, plus the detached start this adapter needs to
// poll a `tart run` that never returns.
type Runner interface {
	executor.CommandRunner
	Start(context.Context, ...string) (StartedCommand, error)
}

type Poller interface {
	Now() time.Time
	Wait(context.Context, time.Duration) error
}

type RealPoller struct{}

func (RealPoller) Now() time.Time { return time.Now().UTC() }

func (RealPoller) Wait(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type ExecRunner struct {
	Binary string
}

func (r ExecRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	binary := r.Binary
	if binary == "" {
		binary = "tart"
	}
	// #nosec G204 -- Binary is a trusted adapter dependency; arguments never pass through a shell.
	command := exec.CommandContext(ctx, binary, args...)
	output, err := command.CombinedOutput()
	if err != nil {
		return output, classify(args, output, err, ctx.Err())
	}
	return output, nil
}

// startedCommand is the ExecRunner-backed StartedCommand: it tracks whether
// the detached process has exited and, if so, its bounded combined output.
type startedCommand struct {
	mu     sync.Mutex
	output []byte
	exited bool
}

func (s *startedCommand) Exited() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exited
}

func (s *startedCommand) Output() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.output...)
}

// boundedWriter keeps the first N bytes of combined output; enough to
// classify startup failures without unbounded growth on long-lived runs.
type boundedWriter struct {
	mu    sync.Mutex
	limit int
	data  []byte
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if remaining := w.limit - len(w.data); remaining > 0 {
		if len(p) < remaining {
			remaining = len(p)
		}
		w.data = append(w.data, p[:remaining]...)
	}
	return len(p), nil
}

func (r ExecRunner) Start(ctx context.Context, args ...string) (StartedCommand, error) {
	binary := r.Binary
	if binary == "" {
		binary = "tart"
	}
	// #nosec G204 -- Binary is a trusted adapter dependency; arguments never pass through a shell.
	command := exec.CommandContext(context.WithoutCancel(ctx), binary, args...)
	configureDetached(command)
	writer := &boundedWriter{limit: 8192}
	command.Stdout = writer
	command.Stderr = writer
	started := &startedCommand{}
	if err := command.Start(); err != nil {
		return nil, classify(args, nil, err, ctx.Err())
	}
	go func() {
		_ = command.Wait()
		writer.mu.Lock()
		output := append([]byte(nil), writer.data...)
		writer.mu.Unlock()
		started.mu.Lock()
		started.output = output
		started.exited = true
		started.mu.Unlock()
	}()
	return started, nil
}

func classify(args []string, output []byte, err, contextErr error) error {
	op := strings.Join(args, " ")
	kind := ErrorCommand
	text := strings.ToLower(string(output))
	if errors.Is(contextErr, context.DeadlineExceeded) {
		kind = ErrorTimeout
	} else if strings.Contains(text, "not found") || strings.Contains(text, "does not exist") {
		kind = ErrorNotFound
	} else if strings.Contains(text, "already exists") {
		kind = ErrorAlreadyExist
	} else if strings.Contains(text, "permission") || strings.Contains(text, "not authorized") {
		kind = ErrorPermission
	}
	exitCode := -1
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		exitCode = exitError.ExitCode()
	}
	return &Error{Op: op, Kind: kind, ExitCode: exitCode, Stderr: string(output), Err: err}
}

type OwnershipRegistry interface {
	PutOwnership(context.Context, string, operations.Ownership) error
	Ownership(context.Context, string) (operations.Ownership, error)
}

type ConfirmationProvider interface {
	ConfirmDeletion(context.Context, string) (operations.DeletionConfirmation, error)
}

// vm is one row of `tart list --format json`. It stays unexported: the shape
// callers see is executor.Instance, and this adapter's decoding of one CLI's
// JSON is nobody else's business.
type vm struct {
	Name    string `json:"Name"`
	Running bool   `json:"Running"`
	Source  string `json:"Source"`
}

// sourceLocal is the Source `tart list` reports for a VM in the node's own VM
// store. Everything else it enumerates is a cached image — an OCI reference with
// no local configuration and no power to read.
const sourceLocal = "local"

// ConfigReader corroborates a backend reading that a local VM is not running,
// against the one file that reading is computed from.
//
// It exists because `tart` 2.32.1 answers `"Running": false` for a VM whose
// `config.json` it could not open — `running()` swallows the error (ADR 0042) —
// so the fleet cannot tell a powered-off machine from an unreadable one. This
// port asks the same question `tart` asked and reports the answer it discarded.
type ConfigReader interface {
	// ReadConfig opens and reads the named local VM's configuration. A nil error
	// means the file was readable at that instant, so a "not running" reading
	// beside it is a measurement rather than a swallowed failure.
	ReadConfig(ctx context.Context, name string) error
}

// HomeConfigReader reads `<home>/vms/<name>/config.json`, which is where `tart`
// keeps a local VM's configuration and therefore the exact file its own power
// reading opens.
type HomeConfigReader struct {
	// Home is the tart home directory. Empty means the one the tart CLI itself
	// would use: $TART_HOME, or ~/.tart.
	Home string
}

// TartHome answers where this node's tart keeps its VMs, by tart's own rule.
func TartHome() string {
	if home := os.Getenv("TART_HOME"); home != "" {
		return home
	}
	dir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, ".tart")
}

func (r HomeConfigReader) ReadConfig(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	home := r.Home
	if home == "" {
		home = TartHome()
	}
	if home == "" {
		return errors.New("tart home is unknown")
	}
	if err := validateName(name); err != nil {
		return err
	}
	// #nosec G304 -- the path is the node's own tart home joined with a name the
	// fleet's instance grammar has already validated; no external value reaches it.
	contents, err := os.ReadFile(filepath.Join(home, "vms", name, "config.json"))
	if err != nil {
		return err
	}
	if len(contents) == 0 {
		return errors.New("empty vm configuration")
	}
	return nil
}

// classifyPowerRead folds a configuration-read failure into the closed
// vocabulary an operator can compare across occurrences.
//
// It is total: an unrecognised failure is PowerReadOther, never PowerReadOK,
// because "I do not know what went wrong" is still not a power reading.
func classifyPowerRead(err error) domain.PowerReadReason {
	switch {
	case err == nil:
		return domain.PowerReadOK
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return domain.PowerReadTimeout
	case errors.Is(err, fs.ErrNotExist):
		return domain.PowerReadMissing
	case errors.Is(err, fs.ErrPermission):
		return domain.PowerReadPermission
	case errors.Is(err, syscall.EMFILE), errors.Is(err, syscall.ENFILE):
		return domain.PowerReadDescriptors
	case errors.Is(err, syscall.ENOMEM):
		return domain.PowerReadMemory
	case errors.Is(err, syscall.EINTR), errors.Is(err, syscall.EAGAIN):
		return domain.PowerReadInterrupted
	case errors.Is(err, syscall.EIO), errors.Is(err, syscall.ENXIO), errors.Is(err, syscall.ENODEV):
		return domain.PowerReadIO
	default:
		return domain.PowerReadOther
	}
}

type VMConfig struct {
	Running bool `json:"Running"`
	CPU     int  `json:"CPU"`
	Memory  int  `json:"Memory"`
	Disk    int  `json:"Disk"`
}

type Adapter struct {
	Runner                   Runner
	Ownership                OwnershipRegistry
	Confirmation             ConfirmationProvider
	CommandTimeout           time.Duration
	StartTimeout             time.Duration
	ConfirmationMaxAge       time.Duration
	MacOSVMPrefixes          []string
	MacOSRootDiskOptions     string
	MacOSSharedDirectoryPath string
	// LinuxNestedVirtualization passes --nested so Linux guests can host their
	// own hypervisor (KVM for Android emulators); requires M3+ silicon, which
	// the controller does not verify — tart fails the start when the host
	// cannot nest. macOS guests never get the flag: Apple's Virtualization
	// framework does not support nesting them and tart rejects it at boot.
	LinuxNestedVirtualization bool
	// LinuxSerialLogDirectory is where a Linux guest's serial console is written
	// on the host, one file per instance. Empty is off, which is what every node
	// runs today and is byte-for-byte the argument vector this adapter has always
	// passed.
	//
	// It exists because issue #236's guest kernel panicked and the panic reached
	// nobody: the base image's cmdline named `console=ttyAMA0` while the VM exposes
	// `hvc*`, so there was no console to capture, and the adapter passed no sink to
	// capture it into. Both halves are needed, and the guest half ships in the base
	// image rather than here (ADR 0040).
	LinuxSerialLogDirectory string
	Now                     func() time.Time
	Poller                  Poller
	// ConfigReader corroborates a "not running" enumeration against the file the
	// enumeration is computed from. Nil is this node's own tart home, which is
	// what every production wiring uses.
	ConfigReader ConfigReader
	mu           sync.Mutex
}

// validateName applies the fleet-wide instance grammar of
// domain.ValidateInstanceName and reports a rejection in this adapter's own
// error vocabulary, so a bad name stays an invalid operation rather than a
// command failure.
func validateName(name string) error {
	if domain.ValidateInstanceName(name) != nil {
		return fmt.Errorf("%w: invalid Tart VM name", operations.ErrInvalid)
	}
	return nil
}

func (a *Adapter) List(ctx context.Context) ([]executor.Instance, error) {
	ctx, cancel := context.WithTimeout(ctx, a.timeout())
	defer cancel()
	output, err := a.runner().Run(ctx, "list", "--format", "json")
	if err != nil {
		return nil, err
	}
	var vms []vm
	if err := json.Unmarshal(output, &vms); err != nil {
		return nil, &Error{Op: "list", Kind: ErrorUncertain, ExitCode: -1, Stderr: string(output), Err: err}
	}
	instances := make([]executor.Instance, 0, len(vms))
	for _, item := range vms {
		power, unreadable := a.power(ctx, item)
		instances = append(instances, executor.Instance{Name: item.Name, Power: power,
			Unreadable: unreadable, Source: item.Source})
	}
	return instances, nil
}

// power classifies one enumerated VM, and is the whole of issue #252 in this
// adapter.
//
// `tart` computes its `Running` field by opening and locking the VM's
// `config.json`, and `running()` returns false for EVERY failure to do so
// (ADR 0042 reproduced it on this host with `chmod 000`). A false therefore means
// either "powered off" or "I could not look", and only the first of those is a
// premise a destructive recovery may rest on. So a false is corroborated against
// the same file: readable means the reading is a measurement, unreadable means
// nothing was established and the fleet says which failure it was.
//
// A true needs no corroboration. `tart` cannot report a VM running by failing to
// read it — the failure path answers false — so the only reading that can be
// manufactured out of an error is the hostile one.
func (a *Adapter) power(ctx context.Context, item vm) (domain.InstancePower, domain.PowerReadFailure) {
	if item.Running {
		return domain.InstancePowerRunning, domain.PowerReadFailure{}
	}
	if !strings.EqualFold(item.Source, sourceLocal) {
		// A cached OCI image is not a machine: it has no local configuration and
		// cannot be running. Corroborating it would report every base image the node
		// has pulled as unreadable.
		return domain.InstancePowerStopped, domain.PowerReadFailure{}
	}
	started := a.poller().Now()
	if err := a.configReader().ReadConfig(ctx, item.Name); err != nil {
		return domain.InstancePowerUnknown, domain.PowerReadFailure{
			Reason: classifyPowerRead(err), Latency: a.poller().Now().Sub(started)}
	}
	return domain.InstancePowerStopped, domain.PowerReadFailure{}
}

func (a *Adapter) configReader() ConfigReader {
	if a.ConfigReader == nil {
		return HomeConfigReader{}
	}
	return a.ConfigReader
}

// Create clones the base VM named by spec.Image and sizes it to the spec. It is
// the Tart implementation of executor.Backend's Create verb; "clone" survives as
// the durable operation kind and failure stage, which are persisted state.
func (a *Adapter) Create(ctx context.Context, request executor.InstanceSpec) error {
	if err := validateName(request.Name); err != nil {
		return err
	}
	if err := validateName(request.Image); err != nil {
		return err
	}
	if !request.Ownership.Valid() {
		return operations.ErrInvalid
	}
	if request.CPU <= 0 || request.MemoryMB <= 0 || request.DiskGB < 0 {
		return operations.ErrInvalid
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.Ownership.PutOwnership(ctx, request.Name, request.Ownership); err != nil {
		return err
	}
	exists, err := a.existsOwned(ctx, request.Name, request.Ownership)
	if err != nil {
		return err
	}
	if !exists {
		commandCtx, cancel := context.WithTimeout(ctx, a.timeout())
		_, commandErr := a.runner().Run(commandCtx, "clone", request.Image, request.Name)
		cancel()
		if commandErr != nil {
			exists, observeErr := a.existsOwned(ctx, request.Name, request.Ownership)
			if observeErr != nil {
				return &Error{Op: "clone", Kind: ErrorUncertain, ExitCode: -1, Err: errors.Join(commandErr, observeErr)}
			}
			if !exists {
				return commandErr
			}
		}
	}
	return a.ensureResources(ctx, request)
}

func (a *Adapter) ensureResources(ctx context.Context, request executor.InstanceSpec) error {
	config, err := a.getConfig(ctx, request.Name)
	if err != nil {
		return err
	}
	if resourcesMatch(config, request) {
		return nil
	}
	if config.Running {
		return &Error{Op: "set", Kind: ErrorUncertain, ExitCode: -1, Err: errors.New("running VM resource drift")}
	}

	args := []string{"set", request.Name, "--cpu", strconv.Itoa(request.CPU), "--memory", strconv.Itoa(request.MemoryMB)}
	if request.DiskGB > 0 && config.Disk < request.DiskGB {
		args = append(args, "--disk-size", strconv.Itoa(request.DiskGB))
	}
	commandCtx, cancel := context.WithTimeout(ctx, a.timeout())
	_, commandErr := a.runner().Run(commandCtx, args...)
	cancel()
	observed, observeErr := a.getConfig(ctx, request.Name)
	if observeErr == nil && resourcesMatch(observed, request) {
		return nil
	}
	if observeErr != nil {
		return &Error{Op: "set", Kind: ErrorUncertain, ExitCode: -1, Err: errors.Join(commandErr, observeErr)}
	}
	if commandErr != nil {
		return commandErr
	}
	return &Error{Op: "set", Kind: ErrorUncertain, ExitCode: -1, Err: errors.New("resource resize was not observed")}
}

func resourcesMatch(config VMConfig, request executor.InstanceSpec) bool {
	diskMatches := request.DiskGB == 0 || config.Disk >= request.DiskGB
	return config.CPU == request.CPU && config.Memory == request.MemoryMB && diskMatches
}

func (a *Adapter) getConfig(ctx context.Context, name string) (VMConfig, error) {
	commandCtx, cancel := context.WithTimeout(ctx, a.timeout())
	output, err := a.runner().Run(commandCtx, "get", name, "--format", "json")
	cancel()
	if err != nil {
		return VMConfig{}, err
	}
	var config VMConfig
	if err := json.Unmarshal(output, &config); err != nil || config.CPU <= 0 || config.Memory <= 0 {
		return VMConfig{}, &Error{Op: "get", Kind: ErrorUncertain, ExitCode: -1, Stderr: string(output), Err: errors.Join(err, errors.New("invalid VM configuration"))}
	}
	return config, nil
}

func (a *Adapter) Start(ctx context.Context, name string, ownership operations.Ownership) error {
	if err := validateName(name); err != nil {
		return err
	}
	vm, err := a.ownedVM(ctx, name, ownership)
	if err != nil {
		return err
	}
	if vm.Power == domain.InstancePowerRunning {
		return nil
	}
	args := []string{"run", name, "--no-graphics", "--no-audio", "--no-clipboard"}
	if a.isMacOSVM(name) {
		if a.MacOSRootDiskOptions != "" {
			args = append(args, "--root-disk-opts="+a.MacOSRootDiskOptions)
		}
		if a.MacOSSharedDirectoryPath != "" {
			args = append(args, "--dir=ci-shared:"+a.MacOSSharedDirectoryPath)
		}
	} else {
		if a.LinuxNestedVirtualization {
			// Apple's Virtualization framework offers nested virtualization to
			// Linux guests only (M3+); tart rejects --nested for macOS guests.
			args = append(args, "--nested")
		}
		path, serialErr := a.serialLogPath(name)
		if serialErr != nil {
			return serialErr
		}
		if path != "" {
			args = append(args, "--serial-path", path)
		}
	}
	started, err := a.runner().Start(ctx, args...)
	if err != nil {
		return err
	}
	deadline := a.poller().Now().Add(a.startTimeout())
	for a.poller().Now().Before(deadline) {
		vm, err = a.ownedVM(ctx, name, ownership)
		if err == nil && vm.Power == domain.InstancePowerRunning {
			return nil
		}
		if err != nil {
			return err
		}
		if started != nil && started.Exited() {
			output := started.Output()
			if strings.Contains(strings.ToLower(string(output)), "exceeds the system limit") {
				return &Error{Op: "run", Kind: ErrorHostQuota, ExitCode: -1, Stderr: string(output), Err: errors.New("macos vm quota exhausted; host reboot required (tart#1217)")}
			}
			return &Error{Op: "run", Kind: ErrorCommand, ExitCode: -1, Stderr: string(output), Err: errors.New("tart run exited before the vm was observed running")}
		}
		if err := a.poller().Wait(ctx, 25*time.Millisecond); err != nil {
			return err
		}
	}
	return &Error{Op: "run", Kind: ErrorTimeout, ExitCode: -1, Err: context.DeadlineExceeded}
}

// serialLogPath is where this instance's serial console goes, or the empty
// string when no directory is configured.
//
// The directory is created rather than assumed, because the alternative is a
// `tart run` that fails for every instance on a node whose operator made one
// typo, and a guest console is diagnostic rather than load-bearing. It fails the
// start rather than silently dropping the flag: a node that was configured to
// keep a panic trace and is not keeping one has a fact its operator needs, and
// discovering that after the next panic is the whole failure mode this exists to
// end.
//
// The per-instance file is created too, and for a harder reason than the
// directory: this fleet's own tart build refuses `--serial-path` with "Failed
// to open PTY" unless the path already exists, verified on a scratch clone
// before any node enabled the sink (issue #259). The append mode keeps an
// instance's earlier console across a daemon-driven restart of its VM.
func (a *Adapter) serialLogPath(name string) (string, error) {
	if a.LinuxSerialLogDirectory == "" {
		return "", nil
	}
	if err := os.MkdirAll(a.LinuxSerialLogDirectory, 0o750); err != nil {
		return "", &Error{Op: "run", Kind: ErrorPermission, ExitCode: -1, Err: err}
	}
	path := filepath.Join(a.LinuxSerialLogDirectory, name+".log")
	// #nosec G304 -- the directory is the operator's own configuration and the
	// basename is this instance's validated name; both are trusted inputs.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err == nil {
		// Nothing was written: the open only reserves the path this tart build
		// demands, so a failed close leaves nothing to roll back.
		err = file.Close()
	}
	if err != nil {
		return "", &Error{Op: "run", Kind: ErrorPermission, ExitCode: -1, Err: err}
	}
	return path, nil
}

func (a *Adapter) isMacOSVM(name string) bool {
	for _, prefix := range a.MacOSVMPrefixes {
		if prefix != "" && strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// Stop asks the guest to power itself down, and gives `tart stop` an EXPLICIT
// graceful window that fits inside this adapter's command deadline.
//
// The window is the whole repair of the 2026-08-10 incident. `tart stop` waits
// `--timeout` seconds (default 30) for a guest-initiated shutdown and then
// forcefully terminates the VM itself. A bare `tart stop` under a
// `context.WithTimeout` therefore races tart's own escalation, and when the
// context wins, `exec.CommandContext` SIGKILLs the very process whose next act
// would have been to force the guest off. The daemon's stop could never be more
// forceful than "ask nicely until the deadline", while an operator's shell
// `tart stop` — bounded by nothing — escalated and returned exit 0 in about
// thirty seconds. Naming the window explicitly makes tart's escalation happen
// inside the deadline instead of being killed by it.
func (a *Adapter) Stop(ctx context.Context, name string, ownership operations.Ownership) error {
	return a.stop(ctx, name, ownership, a.gracefulStopSeconds())
}

// Terminate powers the guest off without waiting for it to agree. It is `tart
// stop --timeout 0`: tart forcefully terminates the VM immediately. Nothing is
// signalled but the guest's own virtual machine process, which is what an
// ephemeral runner's power button does.
func (a *Adapter) Terminate(ctx context.Context, name string, ownership operations.Ownership) error {
	return a.stop(ctx, name, ownership, 0)
}

// Destroy terminates the guest and deletes it, for a drain whose guest will not
// stop. It keeps every guard Delete keeps — durable ownership, and fresh
// deletion confirmation that the runner and its jobs are inactive — and differs
// only in refusing to wait for a shutdown the guest has already proved it will
// not perform.
func (a *Adapter) Destroy(ctx context.Context, name string, ownership operations.Ownership) error {
	if err := validateName(name); err != nil {
		return err
	}
	if err := a.Terminate(ctx, name, ownership); err != nil {
		return err
	}
	return a.Delete(ctx, name, ownership)
}

// stop runs one `tart stop` with an explicit graceful window and re-observes the
// VM before reporting a failure, so a stop that worked despite a command error
// is never retried as if it had not.
func (a *Adapter) stop(ctx context.Context, name string, ownership operations.Ownership, graceful int) error {
	if err := validateName(name); err != nil {
		return err
	}
	vm, err := a.ownedVM(ctx, name, ownership)
	if errors.Is(err, operations.ErrNotFound) {
		return nil
	}
	// Only a CONFIRMED stopped VM needs no stop. A power the backend could not
	// read is not "already off": skipping the command there would report a drain
	// as having stopped a guest it never asked to stop (issue #252).
	if err != nil || vm.Power == domain.InstancePowerStopped {
		return err
	}
	commandCtx, cancel := context.WithTimeout(ctx, a.timeout())
	_, commandErr := a.runner().Run(commandCtx, "stop", name, "--timeout", strconv.Itoa(graceful))
	cancel()
	if commandErr == nil {
		return nil
	}
	vm, observeErr := a.ownedVM(ctx, name, ownership)
	if errors.Is(observeErr, operations.ErrNotFound) || (observeErr == nil && vm.Power == domain.InstancePowerStopped) {
		return nil
	}
	if observeErr != nil {
		return &Error{Op: "stop", Kind: ErrorUncertain, ExitCode: -1, Err: errors.Join(commandErr, observeErr)}
	}
	return commandErr
}

func (a *Adapter) Delete(ctx context.Context, name string, ownership operations.Ownership) error {
	if err := validateName(name); err != nil {
		return err
	}
	vm, err := a.ownedVM(ctx, name, ownership)
	if errors.Is(err, operations.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if a.Confirmation == nil {
		return &Error{Op: "delete", Kind: ErrorUncertain, ExitCode: -1, Err: operations.ErrUncertain}
	}
	confirmation, err := a.Confirmation.ConfirmDeletion(ctx, name)
	if err != nil {
		return err
	}
	now := a.now()()
	if !confirmation.Safe(now, a.confirmationMaxAge()) {
		return &Error{Op: "delete", Kind: ErrorUncertain, ExitCode: -1, Err: operations.ErrUncertain}
	}
	// A power the backend could not read is stopped here too: Delete must not
	// remove a VM it cannot prove is off, so it asks for the stop it would ask for
	// if it knew the VM were running (issue #252). The stop is idempotent.
	if vm.Power != domain.InstancePowerStopped {
		if err := a.Stop(ctx, name, ownership); err != nil {
			return err
		}
		confirmation, err = a.Confirmation.ConfirmDeletion(ctx, name)
		if err != nil || !confirmation.Safe(a.now()(), a.confirmationMaxAge()) {
			return &Error{Op: "delete", Kind: ErrorUncertain, ExitCode: -1, Err: errors.Join(err, operations.ErrUncertain)}
		}
	}
	commandCtx, cancel := context.WithTimeout(ctx, a.timeout())
	_, commandErr := a.runner().Run(commandCtx, "delete", name)
	cancel()
	if commandErr == nil {
		return nil
	}
	_, observeErr := a.find(ctx, name)
	if errors.Is(observeErr, operations.ErrNotFound) {
		return nil
	}
	if observeErr != nil {
		return &Error{Op: "delete", Kind: ErrorUncertain, ExitCode: -1, Err: errors.Join(commandErr, observeErr)}
	}
	return commandErr
}

// Power reports the VM's current power state. A VM a successful enumeration did
// not list is proven absent; a VM it listed but could not read is Unknown, which
// is not permission to do anything.
func (a *Adapter) Power(ctx context.Context, name string) (domain.InstancePower, error) {
	vm, err := a.find(ctx, name)
	if errors.Is(err, operations.ErrNotFound) {
		return domain.InstancePowerAbsent, nil
	}
	if err != nil {
		return domain.InstancePowerUnknown, err
	}
	return vm.Power, nil
}

func (a *Adapter) existsOwned(ctx context.Context, name string, ownership operations.Ownership) (bool, error) {
	_, err := a.ownedVM(ctx, name, ownership)
	if errors.Is(err, operations.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (a *Adapter) ownedVM(ctx context.Context, name string, expected operations.Ownership) (executor.Instance, error) {
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
	instances, err := a.List(ctx)
	if err != nil {
		return executor.Instance{}, err
	}
	for _, instance := range instances {
		if instance.Name == name {
			return instance, nil
		}
	}
	return executor.Instance{}, operations.ErrNotFound
}

func (a *Adapter) timeout() time.Duration {
	if a.CommandTimeout <= 0 {
		return 30 * time.Second
	}
	return a.CommandTimeout
}

// gracefulStopSeconds is how long `tart stop` may wait for the guest before it
// forcefully terminates the VM itself. It is one third of the command deadline
// so that tart's own escalation, and the teardown that follows it, both fit
// inside the deadline this adapter imposes — the daemon must never be the thing
// that kills the process that was about to escalate. It never reaches zero,
// because a graceful stop that never asks the guest is Terminate, not Stop.
func (a *Adapter) gracefulStopSeconds() int {
	graceful := int(a.timeout().Seconds()) / 3
	if graceful < 1 {
		return 1
	}
	return graceful
}

func (a *Adapter) startTimeout() time.Duration {
	if a.StartTimeout <= 0 {
		return 3 * time.Minute
	}
	return a.StartTimeout
}

func (a *Adapter) confirmationMaxAge() time.Duration {
	if a.ConfirmationMaxAge <= 0 {
		return 30 * time.Second
	}
	return a.ConfirmationMaxAge
}

func (a *Adapter) now() func() time.Time {
	if a.Now == nil {
		return func() time.Time { return time.Now().UTC() }
	}
	return func() time.Time { return a.Now().UTC() }
}

func (a *Adapter) runner() Runner {
	if a.Runner == nil {
		return ExecRunner{}
	}
	return a.Runner
}

func (a *Adapter) poller() Poller {
	if a.Poller == nil {
		return RealPoller{}
	}
	return a.Poller
}
