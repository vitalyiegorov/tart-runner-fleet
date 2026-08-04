package tart

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
	Now                       func() time.Time
	Poller                    Poller
	mu                        sync.Mutex
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
		instances = append(instances, executor.Instance{Name: item.Name, Running: item.Running, Source: item.Source})
	}
	return instances, nil
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
	if vm.Running {
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
	} else if a.LinuxNestedVirtualization {
		// Apple's Virtualization framework offers nested virtualization to
		// Linux guests only (M3+); tart rejects --nested for macOS guests.
		args = append(args, "--nested")
	}
	started, err := a.runner().Start(ctx, args...)
	if err != nil {
		return err
	}
	deadline := a.poller().Now().Add(a.startTimeout())
	for a.poller().Now().Before(deadline) {
		vm, err = a.ownedVM(ctx, name, ownership)
		if err == nil && vm.Running {
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

func (a *Adapter) isMacOSVM(name string) bool {
	for _, prefix := range a.MacOSVMPrefixes {
		if prefix != "" && strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func (a *Adapter) Stop(ctx context.Context, name string, ownership operations.Ownership) error {
	if err := validateName(name); err != nil {
		return err
	}
	vm, err := a.ownedVM(ctx, name, ownership)
	if errors.Is(err, operations.ErrNotFound) {
		return nil
	}
	if err != nil || !vm.Running {
		return err
	}
	commandCtx, cancel := context.WithTimeout(ctx, a.timeout())
	_, commandErr := a.runner().Run(commandCtx, "stop", name)
	cancel()
	if commandErr == nil {
		return nil
	}
	vm, observeErr := a.ownedVM(ctx, name, ownership)
	if errors.Is(observeErr, operations.ErrNotFound) || (observeErr == nil && !vm.Running) {
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
	if vm.Running {
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

// Running reports the VM's current power state; an absent VM is not running.
func (a *Adapter) Running(ctx context.Context, name string) (bool, error) {
	vm, err := a.find(ctx, name)
	if errors.Is(err, operations.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return vm.Running, nil
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
