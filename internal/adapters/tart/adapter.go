package tart

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

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

type Runner interface {
	Run(context.Context, ...string) ([]byte, error)
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

type VM struct {
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
	Now                      func() time.Time
	Poller                   Poller
	mu                       sync.Mutex
}

type Request struct {
	Name      string
	Base      string
	CPU       int
	MemoryMB  int
	DiskGB    int
	Ownership operations.Ownership
}

var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func ValidateName(name string) error {
	if !validName.MatchString(name) || name == "." || name == ".." {
		return fmt.Errorf("%w: invalid Tart VM name", operations.ErrInvalid)
	}
	return nil
}

func (a *Adapter) List(ctx context.Context) ([]VM, error) {
	ctx, cancel := context.WithTimeout(ctx, a.timeout())
	defer cancel()
	output, err := a.runner().Run(ctx, "list", "--format", "json")
	if err != nil {
		return nil, err
	}
	var vms []VM
	if err := json.Unmarshal(output, &vms); err != nil {
		return nil, &Error{Op: "list", Kind: ErrorUncertain, ExitCode: -1, Stderr: string(output), Err: err}
	}
	return vms, nil
}

func (a *Adapter) Clone(ctx context.Context, request Request) error {
	if err := ValidateName(request.Name); err != nil {
		return err
	}
	if err := ValidateName(request.Base); err != nil {
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
		_, commandErr := a.runner().Run(commandCtx, "clone", request.Base, request.Name)
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

func (a *Adapter) ensureResources(ctx context.Context, request Request) error {
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

func resourcesMatch(config VMConfig, request Request) bool {
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
	if err := ValidateName(name); err != nil {
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
	if err := ValidateName(name); err != nil {
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
	if err := ValidateName(name); err != nil {
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

func (a *Adapter) existsOwned(ctx context.Context, name string, ownership operations.Ownership) (bool, error) {
	_, err := a.ownedVM(ctx, name, ownership)
	if errors.Is(err, operations.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (a *Adapter) ownedVM(ctx context.Context, name string, expected operations.Ownership) (VM, error) {
	actual, err := a.Ownership.Ownership(ctx, name)
	if err != nil {
		return VM{}, err
	}
	if actual != expected {
		return VM{}, operations.ErrConflict
	}
	return a.find(ctx, name)
}

func (a *Adapter) find(ctx context.Context, name string) (VM, error) {
	vms, err := a.List(ctx)
	if err != nil {
		return VM{}, err
	}
	for _, vm := range vms {
		if vm.Name == name {
			return vm, nil
		}
	}
	return VM{}, operations.ErrNotFound
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
