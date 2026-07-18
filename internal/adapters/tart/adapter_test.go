package tart

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

type memoryOwnership struct {
	mu     sync.Mutex
	data   map[string]operations.Ownership
	putErr error
	getErr error
}

func (m *memoryOwnership) PutOwnership(_ context.Context, name string, ownership operations.Ownership) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.putErr != nil {
		return m.putErr
	}
	if current, ok := m.data[name]; ok && current != ownership {
		return operations.ErrConflict
	}
	m.data[name] = ownership
	return nil
}

func (m *memoryOwnership) Ownership(_ context.Context, name string) (operations.Ownership, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getErr != nil {
		return operations.Ownership{}, m.getErr
	}
	ownership, ok := m.data[name]
	if !ok {
		return operations.Ownership{}, operations.ErrNotFound
	}
	return ownership, nil
}

type fakeConfirmation struct {
	confirmation operations.DeletionConfirmation
	err          error
}

func (f fakeConfirmation) ConfirmDeletion(context.Context, string) (operations.DeletionConfirmation, error) {
	return f.confirmation, f.err
}

type fakeRunner struct {
	mu            sync.Mutex
	vms           map[string]VM
	configs       map[string]VMConfig
	commands      [][]string
	lostResponse  map[string]bool
	listError     error
	listOutput    []byte
	commandError  map[string]error
	startError    error
	startNoEffect bool
	setNoEffect   bool
	startHook     func()
	observeError  error
}

func (f *fakeRunner) Run(_ context.Context, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, append([]string(nil), args...))
	if len(args) == 0 {
		return nil, errors.New("missing command")
	}
	switch args[0] {
	case "list":
		if f.listError != nil {
			return nil, f.listError
		}
		if f.listOutput != nil {
			return f.listOutput, nil
		}
		vms := make([]VM, 0, len(f.vms))
		for _, vm := range f.vms {
			vms = append(vms, vm)
		}
		return json.Marshal(vms)
	case "clone":
		if err := f.commandError["clone"]; err != nil {
			f.listError = f.observeError
			return nil, err
		}
		name := args[2]
		f.vms[name] = VM{Name: name, Source: "local"}
		config := f.configs[args[1]]
		if config.CPU == 0 || config.Memory == 0 {
			config = VMConfig{CPU: 2, Memory: 4096, Disk: 20}
		}
		f.configs[name] = config
		if f.lostResponse["clone"] {
			return nil, context.DeadlineExceeded
		}
	case "get":
		if err := f.commandError["get"]; err != nil {
			return nil, err
		}
		config, ok := f.configs[args[1]]
		if !ok {
			return nil, operations.ErrNotFound
		}
		config.Running = f.vms[args[1]].Running
		return json.Marshal(config)
	case "set":
		if err := f.commandError["set"]; err != nil && !f.lostResponse["set"] {
			return nil, err
		}
		config := f.configs[args[1]]
		for i := 2; i+1 < len(args); i += 2 {
			value, valueErr := strconv.Atoi(args[i+1])
			if valueErr != nil {
				return nil, valueErr
			}
			switch args[i] {
			case "--cpu":
				config.CPU = value
			case "--memory":
				config.Memory = value
			case "--disk-size":
				config.Disk = value
			default:
				return nil, errors.New("unexpected set flag")
			}
		}
		if !f.setNoEffect {
			f.configs[args[1]] = config
		}
		if f.lostResponse["set"] {
			return nil, context.DeadlineExceeded
		}
	case "stop":
		if err := f.commandError["stop"]; err != nil {
			f.listError = f.observeError
			return nil, err
		}
		vm := f.vms[args[1]]
		vm.Running = false
		f.vms[args[1]] = vm
		if f.lostResponse["stop"] {
			return nil, context.DeadlineExceeded
		}
	case "delete":
		if err := f.commandError["delete"]; err != nil {
			f.listError = f.observeError
			return nil, err
		}
		delete(f.vms, args[1])
		delete(f.configs, args[1])
		if f.lostResponse["delete"] {
			return nil, context.DeadlineExceeded
		}
	}
	return nil, nil
}

func (f *fakeRunner) Start(_ context.Context, args ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, append([]string(nil), args...))
	if f.startError != nil {
		return f.startError
	}
	if f.startHook != nil {
		f.startHook()
	}
	if f.startNoEffect {
		return nil
	}
	vm := f.vms[args[1]]
	vm.Running = true
	f.vms[args[1]] = vm
	return nil
}

func testAdapter(now time.Time) (*Adapter, *fakeRunner, *memoryOwnership, operations.Ownership) {
	runner := &fakeRunner{vms: map[string]VM{}, configs: map[string]VMConfig{}, lostResponse: map[string]bool{}, commandError: map[string]error{}}
	registry := &memoryOwnership{data: map[string]operations.Ownership{}}
	ownership := operations.Ownership{ControllerID: "controller", ResourceID: "resource", OperationID: "operation"}
	adapter := &Adapter{
		Runner: runner, Ownership: registry,
		Confirmation:   fakeConfirmation{confirmation: operations.DeletionConfirmation{Fresh: true, RunnerInactive: true, JobsInactive: true, ObservedAt: now}},
		CommandTimeout: time.Second, StartTimeout: time.Second, ConfirmationMaxAge: time.Minute, Now: func() time.Time { return now },
	}
	return adapter, runner, registry, ownership
}

func TestCloneLostResponseAndDuplicateAreIdempotent(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	adapter, runner, _, ownership := testAdapter(now)
	runner.lostResponse["clone"] = true
	request := Request{Name: "fleet-vm-1", Base: "linux-base", CPU: 1, MemoryMB: 2048, Ownership: ownership}
	if err := adapter.Clone(context.Background(), request); err != nil {
		t.Fatalf("lost clone response was not reconciled: %v", err)
	}
	runner.lostResponse["clone"] = false
	before := len(runner.commands)
	if err := adapter.Clone(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	for _, command := range runner.commands[before:] {
		if len(command) > 0 && command[0] == "clone" {
			t.Fatal("duplicate clone executed an effect")
		}
	}
}

func TestCloneAndDeleteSuccessfulCommandResponses(t *testing.T) {
	now := time.Unix(150, 0).UTC()
	adapter, runner, registry, ownership := testAdapter(now)
	if err := adapter.Clone(context.Background(), Request{Name: "vm", Base: "base", CPU: 1, MemoryMB: 2048, Ownership: ownership}); err != nil {
		t.Fatal(err)
	}
	registry.data["vm"] = ownership
	runner.vms["vm"] = VM{Name: "vm"}
	if err := adapter.Delete(context.Background(), "vm", ownership); err != nil {
		t.Fatal(err)
	}
}

func TestCloneEnforcesExactProfileResourcesAndDiskFloorIdempotently(t *testing.T) {
	now := time.Unix(175, 0).UTC()
	adapter, runner, _, ownership := testAdapter(now)
	request := Request{Name: "vm", Base: "base", CPU: 4, MemoryMB: 8192, DiskGB: 50, Ownership: ownership}

	if err := adapter.Clone(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Clone(context.Background(), request); err != nil {
		t.Fatal(err)
	}

	want := VMConfig{CPU: 4, Memory: 8192, Disk: 50}
	if got := runner.configs[request.Name]; got != want {
		t.Fatalf("VM config = %+v, want %+v", got, want)
	}
	setCommands := 0
	for _, command := range runner.commands {
		if len(command) > 0 && command[0] == "set" {
			setCommands++
		}
	}
	if setCommands != 1 {
		t.Fatalf("set commands = %d, want one exact resize", setCommands)
	}
}

func TestCloneNeverShrinksLargerBaseDisk(t *testing.T) {
	now := time.Unix(176, 0).UTC()
	adapter, runner, _, ownership := testAdapter(now)
	runner.configs["base"] = VMConfig{CPU: 4, Memory: 8192, Disk: 100}
	request := Request{Name: "vm", Base: "base", CPU: 4, MemoryMB: 8192, DiskGB: 50, Ownership: ownership}

	if err := adapter.Clone(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if got := runner.configs[request.Name].Disk; got != 100 {
		t.Fatalf("disk = %d, want inherited 100 GB", got)
	}
	for _, command := range runner.commands {
		if len(command) > 0 && command[0] == "set" {
			t.Fatalf("larger base disk triggered resize: %v", command)
		}
	}
}

func TestCloneResourceReconciliationHandlesLostResponseAndFailsClosed(t *testing.T) {
	now := time.Unix(180, 0).UTC()
	t.Run("lost resize response", func(t *testing.T) {
		adapter, runner, _, ownership := testAdapter(now)
		runner.lostResponse["set"] = true
		if err := adapter.Clone(context.Background(), Request{Name: "vm", Base: "base", CPU: 4, MemoryMB: 8192, Ownership: ownership}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("running drift", func(t *testing.T) {
		adapter, runner, registry, ownership := testAdapter(now)
		registry.data["vm"] = ownership
		runner.vms["vm"] = VM{Name: "vm", Running: true}
		runner.configs["vm"] = VMConfig{CPU: 2, Memory: 4096}
		if err := adapter.Clone(context.Background(), Request{Name: "vm", Base: "base", CPU: 4, MemoryMB: 8192, Ownership: ownership}); err == nil {
			t.Fatal("running resource drift was accepted")
		}
	})
	t.Run("resize command failure", func(t *testing.T) {
		adapter, runner, _, ownership := testAdapter(now)
		runner.commandError["set"] = errors.New("set failed")
		if err := adapter.Clone(context.Background(), Request{Name: "vm", Base: "base", CPU: 4, MemoryMB: 8192, Ownership: ownership}); err == nil {
			t.Fatal("resize failure was accepted")
		}
	})
	t.Run("resize not observed", func(t *testing.T) {
		adapter, runner, _, ownership := testAdapter(now)
		runner.setNoEffect = true
		if err := adapter.Clone(context.Background(), Request{Name: "vm", Base: "base", CPU: 4, MemoryMB: 8192, Ownership: ownership}); err == nil {
			t.Fatal("unobserved resize was accepted")
		}
	})
	t.Run("invalid configuration", func(t *testing.T) {
		adapter, runner, registry, ownership := testAdapter(now)
		registry.data["vm"] = ownership
		runner.vms["vm"] = VM{Name: "vm"}
		runner.configs["vm"] = VMConfig{}
		if err := adapter.Clone(context.Background(), Request{Name: "vm", Base: "base", CPU: 4, MemoryMB: 8192, Ownership: ownership}); err == nil {
			t.Fatal("invalid observed configuration was accepted")
		}
	})
}

func TestStartStopDeleteAndFailClosedConfirmation(t *testing.T) {
	now := time.Unix(200, 0).UTC()
	adapter, runner, registry, ownership := testAdapter(now)
	registry.data["vm"] = ownership
	runner.vms["vm"] = VM{Name: "vm"}
	if err := adapter.Start(context.Background(), "vm", ownership); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Start(context.Background(), "vm", ownership); err != nil {
		t.Fatal(err)
	}
	runner.lostResponse["stop"] = true
	if err := adapter.Stop(context.Background(), "vm", ownership); err != nil {
		t.Fatalf("lost stop response: %v", err)
	}
	runner.vms["vm"] = VM{Name: "vm"}
	runner.lostResponse["delete"] = true
	if err := adapter.Delete(context.Background(), "vm", ownership); err != nil {
		t.Fatalf("lost delete response: %v", err)
	}
	if err := adapter.Delete(context.Background(), "vm", ownership); err != nil {
		t.Fatal(err)
	}
	runner.vms["uncertain"] = VM{Name: "uncertain"}
	registry.data["uncertain"] = ownership
	adapter.Confirmation = fakeConfirmation{confirmation: operations.DeletionConfirmation{Fresh: false}}
	if err := adapter.Delete(context.Background(), "uncertain", ownership); err == nil {
		t.Fatal("uncertain deletion was allowed")
	} else {
		var typed *Error
		if !errors.As(err, &typed) || typed.Kind != ErrorUncertain {
			t.Fatalf("unexpected deletion error: %T %v", err, err)
		}
	}
	if _, ok := runner.vms["uncertain"]; !ok {
		t.Fatal("uncertain VM was deleted")
	}
}

func TestOwnershipConflictListUncertaintyAndNames(t *testing.T) {
	now := time.Unix(300, 0).UTC()
	adapter, runner, registry, ownership := testAdapter(now)
	registry.data["vm"] = ownership
	runner.vms["vm"] = VM{Name: "vm"}
	other := ownership
	other.OperationID = "other"
	if err := adapter.Start(context.Background(), "vm", other); !errors.Is(err, operations.ErrConflict) {
		t.Fatalf("ownership conflict ignored: %v", err)
	}
	for _, name := range []string{"", "../vm", "vm;touch-pwned", "vm name", stringsOf("x", 129)} {
		if err := ValidateName(name); err == nil {
			t.Fatalf("malicious name accepted: %q", name)
		}
	}
	if err := ValidateName("safe.vm-1"); err != nil {
		t.Fatal(err)
	}
	runner.listError = errors.New("api unavailable")
	if _, err := adapter.List(context.Background()); err == nil {
		t.Fatal("list error swallowed")
	}
	runner.listError = nil
	runner.commands = nil
	registry.data["trf-maestro-1"] = ownership
	runner.vms["trf-maestro-1"] = VM{Name: "trf-maestro-1"}
	adapter.MacOSVMPrefixes = []string{"trf-builder-", "trf-maestro-"}
	adapter.MacOSRootDiskOptions = "sync=none"
	adapter.MacOSSharedDirectoryPath = "/private/tmp/ci-shared"
	if err := adapter.Start(context.Background(), "trf-maestro-1", ownership); err != nil {
		t.Fatal(err)
	}
	want := []string{"run", "trf-maestro-1", "--no-graphics", "--no-audio", "--no-clipboard", "--root-disk-opts=sync=none", "--dir=ci-shared:/private/tmp/ci-shared"}
	found := false
	for _, command := range runner.commands {
		if reflect.DeepEqual(command, want) {
			found = true
		}
	}
	if !found {
		t.Fatalf("argv mismatch: %#v", runner.commands)
	}
	registry.data["trf-small-1"] = ownership
	runner.vms["trf-small-1"] = VM{Name: "trf-small-1"}
	runner.commands = nil
	if err := adapter.Start(context.Background(), "trf-small-1", ownership); err != nil {
		t.Fatal(err)
	}
	linuxWant := []string{"run", "trf-small-1", "--no-graphics", "--no-audio", "--no-clipboard"}
	found = false
	for _, command := range runner.commands {
		if reflect.DeepEqual(command, linuxWant) {
			found = true
		}
	}
	if !found {
		t.Fatalf("linux argv mismatch: %#v", runner.commands)
	}
}

func TestExecRunnerTimeoutIsTyped(t *testing.T) {
	script := filepath.Join(t.TempDir(), "slow")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 5\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := (ExecRunner{Binary: script}).Run(ctx, "list")
	var typed *Error
	if !errors.As(err, &typed) || typed.Kind != ErrorTimeout {
		t.Fatalf("expected timeout, got %T %v", err, err)
	}
}

func stringsOf(value string, count int) string {
	result := ""
	for range count {
		result += value
	}
	return result
}

type fakePoller struct {
	now     time.Time
	advance time.Duration
	waitErr error
}

func (p *fakePoller) Now() time.Time { return p.now }

func (p *fakePoller) Wait(context.Context, time.Duration) error {
	p.now = p.now.Add(p.advance)
	return p.waitErr
}

type sequenceConfirmation struct {
	values []operations.DeletionConfirmation
	errors []error
	index  int
}

func (s *sequenceConfirmation) ConfirmDeletion(context.Context, string) (operations.DeletionConfirmation, error) {
	index := s.index
	s.index++
	return s.values[index], s.errors[index]
}

func TestTypedErrorsClassifyAndRealRunners(t *testing.T) {
	base := errors.New("command failed")
	typed := &Error{Op: "list", Kind: ErrorCommand, Err: base}
	if typed.Error() == "" || !errors.Is(typed, base) {
		t.Fatal("typed error does not format or unwrap")
	}
	for name, test := range map[string]struct {
		output string
		ctxErr error
		kind   ErrorKind
	}{
		"timeout":       {ctxErr: context.DeadlineExceeded, kind: ErrorTimeout},
		"not found":     {output: "does not exist", kind: ErrorNotFound},
		"already":       {output: "already exists", kind: ErrorAlreadyExist},
		"permission":    {output: "permission denied", kind: ErrorPermission},
		"authorization": {output: "not authorized", kind: ErrorPermission},
		"command":       {output: "other", kind: ErrorCommand},
	} {
		t.Run(name, func(t *testing.T) {
			var got *Error
			if !errors.As(classify([]string{"run", "vm"}, []byte(test.output), base, test.ctxErr), &got) || got.Kind != test.kind || got.ExitCode != -1 {
				t.Fatalf("classification: %#v", got)
			}
		})
	}
	output, err := (ExecRunner{Binary: "/usr/bin/printf"}).Run(context.Background(), "ok")
	if err != nil || string(output) != "ok" {
		t.Fatalf("exec run success: %q %v", output, err)
	}
	_, err = (ExecRunner{Binary: "/bin/sh"}).Run(context.Background(), "-c", "echo 'not found' >&2; exit 7")
	var exit *Error
	if !errors.As(err, &exit) || exit.Kind != ErrorNotFound || exit.ExitCode != 7 {
		t.Fatalf("exit classification: %#v", exit)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := (ExecRunner{Binary: "/usr/bin/true"}).Start(canceled); err != nil {
		t.Fatalf("detached start inherited cancellation: %v", err)
	}
	if err := (ExecRunner{Binary: "/definitely/missing/tart"}).Start(context.Background()); err == nil {
		t.Fatal("start failure ignored")
	}
	_ = (ExecRunner{}).Start(context.Background())
	_, _ = (ExecRunner{}).Run(context.Background())
	if err := (RealPoller{}).Wait(context.Background(), time.Millisecond); err != nil || (RealPoller{}).Now().Location() != time.UTC {
		t.Fatalf("real poller timer: %v", err)
	}
	ctx, stop := context.WithCancel(context.Background())
	stop()
	if err := (RealPoller{}).Wait(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("real poller cancellation: %v", err)
	}
}

func TestAdapterCloneFailureBranches(t *testing.T) {
	now := time.Unix(400, 0).UTC()
	adapter, runner, registry, ownership := testAdapter(now)
	valid := Request{Name: "vm", Base: "base", CPU: 1, MemoryMB: 2048, Ownership: ownership}
	for name, request := range map[string]Request{
		"name":      {Name: "../bad", Base: "base", CPU: 1, MemoryMB: 2048, Ownership: ownership},
		"base":      {Name: "vm", Base: "../bad", CPU: 1, MemoryMB: 2048, Ownership: ownership},
		"ownership": {Name: "vm", Base: "base", CPU: 1, MemoryMB: 2048},
		"cpu":       {Name: "vm", Base: "base", MemoryMB: 2048, Ownership: ownership},
		"memory":    {Name: "vm", Base: "base", CPU: 1, Ownership: ownership},
		"disk":      {Name: "vm", Base: "base", CPU: 1, MemoryMB: 2048, DiskGB: -1, Ownership: ownership},
	} {
		t.Run(name, func(t *testing.T) {
			if err := adapter.Clone(context.Background(), request); !errors.Is(err, operations.ErrInvalid) {
				t.Fatalf("invalid clone: %v", err)
			}
		})
	}
	registry.putErr = errors.New("write failed")
	if err := adapter.Clone(context.Background(), valid); err == nil {
		t.Fatal("ownership write error ignored")
	}
	registry.putErr = nil
	runner.commandError["clone"] = errors.New("clone failed")
	if err := adapter.Clone(context.Background(), valid); err == nil {
		t.Fatal("clone command failure ignored")
	}
	runner.observeError = errors.New("list failed")
	runner.listError = nil
	if err := adapter.Clone(context.Background(), Request{Name: "vm2", Base: "base", CPU: 1, MemoryMB: 2048, Ownership: ownership}); err == nil {
		t.Fatal("clone observation uncertainty ignored")
	} else if typed := new(Error); !errors.As(err, &typed) || typed.Kind != ErrorUncertain {
		t.Fatalf("clone uncertainty type: %v", err)
	}
}

func TestAdapterStartFailureBranchesAndPolling(t *testing.T) {
	now := time.Unix(500, 0).UTC()
	adapter, runner, registry, ownership := testAdapter(now)
	if err := adapter.Start(context.Background(), "../bad", ownership); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("invalid start: %v", err)
	}
	if err := adapter.Start(context.Background(), "missing", ownership); !errors.Is(err, operations.ErrNotFound) {
		t.Fatalf("missing start: %v", err)
	}
	registry.data["vm"] = ownership
	runner.vms["vm"] = VM{Name: "vm"}
	runner.startError = errors.New("start failed")
	if err := adapter.Start(context.Background(), "vm", ownership); err == nil {
		t.Fatal("start command failure ignored")
	}
	runner.startError = nil
	runner.startNoEffect = true
	adapter.StartTimeout = time.Second
	adapter.Poller = &fakePoller{now: now, advance: time.Second}
	if err := adapter.Start(context.Background(), "vm", ownership); err == nil {
		t.Fatal("start timeout ignored")
	} else if typed := new(Error); !errors.As(err, &typed) || typed.Kind != ErrorTimeout {
		t.Fatalf("start timeout type: %v", err)
	}
	adapter.Poller = &fakePoller{now: now, advance: time.Millisecond, waitErr: context.Canceled}
	if err := adapter.Start(context.Background(), "vm", ownership); !errors.Is(err, context.Canceled) {
		t.Fatalf("poll cancellation ignored: %v", err)
	}
	adapter.Poller = &fakePoller{now: now, advance: time.Millisecond}
	runner.startHook = func() { registry.getErr = errors.New("ownership unavailable") }
	if err := adapter.Start(context.Background(), "vm", ownership); err == nil {
		t.Fatal("poll observation failure ignored")
	}
}

func TestAdapterStopFailureBranches(t *testing.T) {
	now := time.Unix(600, 0).UTC()
	adapter, runner, registry, ownership := testAdapter(now)
	if err := adapter.Stop(context.Background(), "../bad", ownership); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("invalid stop: %v", err)
	}
	if err := adapter.Stop(context.Background(), "missing", ownership); err != nil {
		t.Fatalf("missing stop is not idempotent: %v", err)
	}
	registry.data["vm"] = ownership
	runner.vms["vm"] = VM{Name: "vm"}
	if err := adapter.Stop(context.Background(), "vm", ownership); err != nil {
		t.Fatalf("stopped VM should be a no-op: %v", err)
	}
	other := ownership
	other.OperationID = "other"
	if err := adapter.Stop(context.Background(), "vm", other); !errors.Is(err, operations.ErrConflict) {
		t.Fatalf("stop ownership conflict: %v", err)
	}
	runner.vms["vm"] = VM{Name: "vm", Running: true}
	runner.commandError["stop"] = errors.New("stop failed")
	if err := adapter.Stop(context.Background(), "vm", ownership); err == nil {
		t.Fatal("stop command error ignored")
	}
	runner.listError = nil
	runner.observeError = errors.New("list unavailable")
	if err := adapter.Stop(context.Background(), "vm", ownership); err == nil {
		t.Fatal("stop observation uncertainty ignored")
	} else if typed := new(Error); !errors.As(err, &typed) || typed.Kind != ErrorUncertain {
		t.Fatalf("stop uncertainty type: %v", err)
	}
}

func TestAdapterDeleteFailureBranches(t *testing.T) {
	now := time.Unix(700, 0).UTC()
	adapter, runner, registry, ownership := testAdapter(now)
	if err := adapter.Delete(context.Background(), "../bad", ownership); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("invalid delete: %v", err)
	}
	if err := adapter.Delete(context.Background(), "missing", ownership); err != nil {
		t.Fatalf("missing delete is not idempotent: %v", err)
	}
	registry.data["vm"] = ownership
	runner.vms["vm"] = VM{Name: "vm"}
	adapter.Confirmation = nil
	if err := adapter.Delete(context.Background(), "vm", ownership); err == nil {
		t.Fatal("nil confirmation provider did not fail closed")
	}
	other := ownership
	other.OperationID = "other"
	if err := adapter.Delete(context.Background(), "vm", other); !errors.Is(err, operations.ErrConflict) {
		t.Fatalf("delete ownership conflict: %v", err)
	}
	adapter.Confirmation = fakeConfirmation{err: errors.New("github unavailable")}
	if err := adapter.Delete(context.Background(), "vm", ownership); err == nil {
		t.Fatal("confirmation error ignored")
	}
	adapter.Confirmation = fakeConfirmation{confirmation: operations.DeletionConfirmation{Fresh: true, RunnerInactive: true, JobsInactive: true, ObservedAt: now}}
	runner.vms["vm"] = VM{Name: "vm", Running: true}
	runner.commandError["stop"] = errors.New("stop failed")
	if err := adapter.Delete(context.Background(), "vm", ownership); err == nil {
		t.Fatal("stop failure during delete ignored")
	}
	runner.commandError["stop"] = nil
	sequence := &sequenceConfirmation{values: []operations.DeletionConfirmation{
		{Fresh: true, RunnerInactive: true, JobsInactive: true, ObservedAt: now},
		{},
	}, errors: []error{nil, errors.New("second confirmation failed")}}
	adapter.Confirmation = sequence
	if err := adapter.Delete(context.Background(), "vm", ownership); err == nil {
		t.Fatal("second confirmation uncertainty ignored")
	}
	runner.vms["vm"] = VM{Name: "vm"}
	adapter.Confirmation = fakeConfirmation{confirmation: operations.DeletionConfirmation{Fresh: true, RunnerInactive: true, JobsInactive: true, ObservedAt: now}}
	runner.commandError["delete"] = errors.New("delete failed")
	if err := adapter.Delete(context.Background(), "vm", ownership); err == nil {
		t.Fatal("delete command failure ignored")
	}
	runner.listError = nil
	runner.observeError = errors.New("list unavailable")
	if err := adapter.Delete(context.Background(), "vm", ownership); err == nil {
		t.Fatal("delete observation uncertainty ignored")
	} else if typed := new(Error); !errors.As(err, &typed) || typed.Kind != ErrorUncertain {
		t.Fatalf("delete uncertainty type: %v", err)
	}
}

func TestAdapterDefaultsAndInvalidList(t *testing.T) {
	adapter, runner, _, _ := testAdapter(time.Now().UTC())
	runner.listOutput = []byte("not-json")
	if _, err := adapter.List(context.Background()); err == nil {
		t.Fatal("invalid list JSON accepted")
	}
	defaults := &Adapter{}
	if defaults.timeout() != 30*time.Second || defaults.startTimeout() != 3*time.Minute || defaults.confirmationMaxAge() != 30*time.Second || defaults.now()().Location() != time.UTC || defaults.runner() == nil || defaults.poller() == nil {
		t.Fatal("adapter defaults mismatch")
	}
}
