package lifecycle

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

type missingState struct{}

func (missingState) Instance(context.Context, string) (operations.Instance, error) {
	return operations.Instance{}, operations.ErrNotFound
}
func (missingState) Advance(context.Context, StateChange) (operations.Instance, error) {
	return operations.Instance{}, operations.ErrNotFound
}

type stuckState struct{ instance operations.Instance }

func (s stuckState) Instance(context.Context, string) (operations.Instance, error) {
	return s.instance, nil
}
func (s stuckState) Advance(context.Context, StateChange) (operations.Instance, error) {
	return s.instance, nil
}

func provisionFixture(stateValue operations.State) (ProvisionExecutor, *memoryState, *fakeVM, *fakeRegistration, *fakeReady, *fakeBootstrap) {
	calls := []string{}
	state := &memoryState{instance: lifecycleInstance(stateValue)}
	vm := &fakeVM{calls: &calls}
	registration := &fakeRegistration{calls: &calls, secret: githubscaleset.NewJITSecret("jit")}
	ready := &fakeReady{calls: &calls}
	bootstrap := &fakeBootstrap{calls: &calls, registration: registration}
	executor := ProvisionExecutor{State: state, VM: vm, Ready: ready, Registration: registration, Bootstrap: bootstrap,
		Bases:               map[domain.Platform]string{domain.PlatformLinux: "linux-base"},
		RegistrationTimeout: time.Millisecond, RegistrationPollInterval: time.Millisecond}
	return executor, state, vm, registration, ready, bootstrap
}

func expectStage(t *testing.T, err error, stage Stage, state *memoryState, expectedState operations.State) {
	t.Helper()
	if err == nil || err.Error() != safeError(stage).Error() {
		t.Fatalf("error=%v want stage=%s", err, stage)
	}
	if state.instance.State != expectedState || state.instance.LastError != "" || len(state.changes) != 0 {
		t.Fatalf("state=%#v", state.instance)
	}
}

func TestProvisionExecutorValidationCancellationAndTerminals(t *testing.T) {
	valid := operations.Operation{Kind: OperationProvision, ResourceID: "trf-small-1"}
	for name, executor := range map[string]ProvisionExecutor{
		"nil state": {},
	} {
		t.Run(name, func(t *testing.T) {
			if err := executor.Execute(context.Background(), valid); !errors.Is(err, operations.ErrInvalid) {
				t.Fatalf("error=%v", err)
			}
		})
	}
	executor, _, _, _, _, _ := provisionFixture(operations.StatePlanned)
	for _, operation := range []operations.Operation{{Kind: "bad", ResourceID: "trf-small-1"}, {Kind: OperationProvision}} {
		if err := executor.Execute(context.Background(), operation); !errors.Is(err, operations.ErrInvalid) {
			t.Fatalf("invalid operation error=%v", err)
		}
	}
	if err := (ProvisionExecutor{State: missingState{}}).Execute(context.Background(), valid); err == nil || err.Error() != safeError(StagePersist).Error() {
		t.Fatalf("missing state error=%v", err)
	}
	bad, _, _, _, _, _ := provisionFixture(operations.StatePlanned)
	badState := bad.State.(*memoryState)
	badState.instance.Ownership = operations.Ownership{}
	if err := bad.Execute(context.Background(), valid); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("invalid ownership error=%v", err)
	}

	canceled, _, _, _, _, _ := provisionFixture(operations.StatePlanned)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := canceled.Execute(ctx, valid); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled error=%v", err)
	}
	for _, terminal := range []operations.State{operations.StateAssigned, operations.StateRunning} {
		t.Run(string(terminal), func(t *testing.T) {
			executor, _, _, _, _, _ := provisionFixture(terminal)
			if err := executor.Execute(context.Background(), valid); err != nil {
				t.Fatal(err)
			}
		})
	}
	executor, _, _, _, _, _ = provisionFixture(operations.StateFailed)
	if err := executor.Execute(context.Background(), valid); err == nil || err.Error() != safeError(StagePersist).Error() {
		t.Fatalf("failed provision completed: %v", err)
	}
	executor, _, _, _, _, _ = provisionFixture(operations.StateDeregistering)
	if err := executor.Execute(context.Background(), valid); !errors.Is(err, operations.ErrConflict) {
		t.Fatalf("invalid state error=%v", err)
	}
}

func TestProvisionExecutorEveryStageFailsWithBoundedCode(t *testing.T) {
	operation := operations.Operation{Kind: OperationProvision, ResourceID: "trf-small-1"}
	tests := []struct {
		name   string
		state  operations.State
		stage  Stage
		mutate func(*ProvisionExecutor, *fakeVM, *fakeRegistration, *fakeReady, *fakeBootstrap)
	}{
		{"missing vm clone", operations.StatePlanned, StageClone, func(e *ProvisionExecutor, _ *fakeVM, _ *fakeRegistration, _ *fakeReady, _ *fakeBootstrap) { e.VM = nil }},
		{"invalid base", operations.StatePlanned, StageClone, func(e *ProvisionExecutor, _ *fakeVM, _ *fakeRegistration, _ *fakeReady, _ *fakeBootstrap) {
			e.Bases[domain.PlatformLinux] = "../bad"
		}},
		{"missing vm start", operations.StateCloning, StageStart, func(e *ProvisionExecutor, _ *fakeVM, _ *fakeRegistration, _ *fakeReady, _ *fakeBootstrap) { e.VM = nil }},
		{"start", operations.StateCloning, StageStart, func(_ *ProvisionExecutor, vm *fakeVM, _ *fakeRegistration, _ *fakeReady, _ *fakeBootstrap) {
			vm.startErr = errors.New("raw")
		}},
		{"missing ready", operations.StateBooting, StageReady, func(e *ProvisionExecutor, _ *fakeVM, _ *fakeRegistration, _ *fakeReady, _ *fakeBootstrap) {
			e.Ready = nil
		}},
		{"ready", operations.StateBooting, StageReady, func(_ *ProvisionExecutor, _ *fakeVM, _ *fakeRegistration, ready *fakeReady, _ *fakeBootstrap) {
			ready.err = errors.New("raw")
		}},
		{"missing registration", operations.StateReachable, StageAcquire, func(e *ProvisionExecutor, _ *fakeVM, _ *fakeRegistration, _ *fakeReady, _ *fakeBootstrap) {
			e.Registration = nil
		}},
		{"missing bootstrap", operations.StateReachable, StageAcquire, func(e *ProvisionExecutor, _ *fakeVM, _ *fakeRegistration, _ *fakeReady, _ *fakeBootstrap) {
			e.Bootstrap = nil
		}},
		{"observe", operations.StateReachable, StageRegister, func(_ *ProvisionExecutor, _ *fakeVM, registration *fakeRegistration, _ *fakeReady, _ *fakeBootstrap) {
			registration.registeredErr = errors.New("raw")
		}},
		{"reset ghost registration", operations.StateReachable, StageRegister, func(_ *ProvisionExecutor, _ *fakeVM, registration *fakeRegistration, _ *fakeReady, _ *fakeBootstrap) {
			registration.registered = true
			registration.resetErr = errors.New("raw")
		}},
		{"acquire", operations.StateReachable, StageAcquire, func(_ *ProvisionExecutor, _ *fakeVM, registration *fakeRegistration, _ *fakeReady, _ *fakeBootstrap) {
			registration.acquireErr = errors.New("raw")
		}},
		{"nil jit", operations.StateReachable, StageAcquire, func(_ *ProvisionExecutor, _ *fakeVM, registration *fakeRegistration, _ *fakeReady, _ *fakeBootstrap) {
			registration.secret = nil
		}},
		{"empty jit", operations.StateReachable, StageAcquire, func(_ *ProvisionExecutor, _ *fakeVM, registration *fakeRegistration, _ *fakeReady, _ *fakeBootstrap) {
			registration.secret = githubscaleset.NewJITSecret("")
		}},
		{"bootstrap", operations.StateReachable, StageBootstrap, func(_ *ProvisionExecutor, _ *fakeVM, _ *fakeRegistration, _ *fakeReady, bootstrap *fakeBootstrap) {
			bootstrap.err = errors.New("raw")
		}},
		{"missing final registration", operations.StateRegistering, StageRegister, func(e *ProvisionExecutor, _ *fakeVM, _ *fakeRegistration, _ *fakeReady, _ *fakeBootstrap) {
			e.Registration = nil
		}},
		{"not registered", operations.StateRegistering, StageRegister, func(_ *ProvisionExecutor, _ *fakeVM, _ *fakeRegistration, _ *fakeReady, _ *fakeBootstrap) {}},
		{"registration error", operations.StateRegistering, StageRegister, func(_ *ProvisionExecutor, _ *fakeVM, registration *fakeRegistration, _ *fakeReady, _ *fakeBootstrap) {
			registration.registeredErr = errors.New("raw")
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor, state, vm, registration, ready, bootstrap := provisionFixture(test.state)
			test.mutate(&executor, vm, registration, ready, bootstrap)
			err := executor.Execute(context.Background(), operation)
			expectStage(t, err, test.stage, state, test.state)
			if strings.Contains(err.Error(), "raw") {
				t.Fatalf("raw error leaked: %v", err)
			}
		})
	}
}

func TestProvisionExecutorObservedRegistrationCustomFolderAndPersistFailure(t *testing.T) {
	operation := operations.Operation{Kind: OperationProvision, ResourceID: "trf-small-1"}
	executor, state, _, registration, _, _ := provisionFixture(operations.StateReachable)
	registration.registered = true
	if err := executor.Execute(context.Background(), operation); err != nil || state.instance.State != operations.StateAssigned {
		t.Fatalf("observed registration state=%s err=%v", state.instance.State, err)
	}

	executor, _, _, registration, _, _ = provisionFixture(operations.StateReachable)
	executor.WorkFolder = "custom-work"
	if err := executor.Execute(context.Background(), operation); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, call := range *registration.calls {
		found = found || strings.Contains(call, ":custom-work")
	}
	if !found {
		t.Fatalf("calls=%v", *registration.calls)
	}

	executor, state, _, _, _, _ = provisionFixture(operations.StatePlanned)
	state.advanceErr = errors.New("database unavailable")
	err := executor.Execute(context.Background(), operation)
	if err == nil || err.Error() != safeError(StagePersist).Error() || state.instance.State != operations.StatePlanned {
		t.Fatalf("persist error=%v state=%s", err, state.instance.State)
	}
}

func drainFixture(stateValue operations.State) (DrainExecutor, *memoryState, *fakeVM, *fakeDrainControl) {
	now := time.Unix(2000, 0).UTC()
	calls := []string{}
	safe := operations.DeletionConfirmation{Fresh: true, RunnerInactive: true, JobsInactive: true, ObservedAt: now}
	state := &memoryState{instance: lifecycleInstance(stateValue)}
	vm := &fakeVM{calls: &calls}
	control := &fakeDrainControl{calls: &calls, safe: true, confirmations: []operations.DeletionConfirmation{safe, safe}}
	return DrainExecutor{State: state, VM: vm, Control: control, Now: func() time.Time { return now }, ConfirmationMaxAge: time.Minute,
		ConfirmationTimeout: time.Millisecond, ConfirmationPollInterval: time.Millisecond}, state, vm, control
}

func TestDrainExecutorValidationCancellationAndTerminals(t *testing.T) {
	valid := operations.Operation{Kind: OperationDrain, ResourceID: "trf-small-1"}
	if err := (DrainExecutor{}).Execute(context.Background(), valid); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("nil state error=%v", err)
	}
	executor, _, _, _ := drainFixture(operations.StateDraining)
	for _, operation := range []operations.Operation{{Kind: "bad", ResourceID: valid.ResourceID}, {Kind: OperationDrain}} {
		if err := executor.Execute(context.Background(), operation); !errors.Is(err, operations.ErrInvalid) {
			t.Fatalf("invalid operation error=%v", err)
		}
	}
	if err := (DrainExecutor{State: missingState{}}).Execute(context.Background(), valid); err == nil || err.Error() != safeError(StagePersist).Error() {
		t.Fatalf("missing state error=%v", err)
	}
	executor, state, _, _ := drainFixture(operations.StateDraining)
	state.instance.ID = "../bad"
	valid.ResourceID = "../bad"
	if err := executor.Execute(context.Background(), valid); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("invalid instance error=%v", err)
	}
	valid.ResourceID = "trf-small-1"
	executor, _, _, _ = drainFixture(operations.StateDraining)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := executor.Execute(ctx, valid); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error=%v", err)
	}
	executor, _, _, _ = drainFixture(operations.StateDeleted)
	if err := executor.Execute(context.Background(), valid); err != nil {
		t.Fatal(err)
	}
	executor, _, _, _ = drainFixture(operations.StateFailed)
	if err := executor.Execute(context.Background(), valid); err == nil || err.Error() != safeError(StagePersist).Error() {
		t.Fatalf("failed drain completed and could release a dependent spawn: %v", err)
	}
	executor, _, _, _ = drainFixture(operations.StateRunning)
	if err := executor.Execute(context.Background(), valid); !errors.Is(err, operations.ErrConflict) {
		t.Fatalf("state error=%v", err)
	}
}

func TestDrainExecutorEveryStageFailsClosed(t *testing.T) {
	operation := operations.Operation{Kind: OperationDrain, ResourceID: "trf-small-1"}
	tests := []struct {
		name   string
		state  operations.State
		stage  Stage
		mutate func(*DrainExecutor, *fakeVM, *fakeDrainControl)
	}{
		{"missing control", operations.StateDraining, StageGuard, func(e *DrainExecutor, _ *fakeVM, _ *fakeDrainControl) { e.Control = nil }},
		{"unsafe", operations.StateDraining, StageGuard, func(_ *DrainExecutor, _ *fakeVM, control *fakeDrainControl) { control.safe = false }},
		{"guard error", operations.StateDraining, StageGuard, func(_ *DrainExecutor, _ *fakeVM, control *fakeDrainControl) { control.guardErr = errors.New("raw") }},
		{"deregister", operations.StateDraining, StageDeregister, func(_ *DrainExecutor, _ *fakeVM, control *fakeDrainControl) {
			control.deregisterErr = errors.New("raw")
		}},
		{"confirm error", operations.StateDraining, StageConfirm, func(_ *DrainExecutor, _ *fakeVM, control *fakeDrainControl) { control.confirmErr = errors.New("raw") }},
		{"missing stop vm", operations.StateDeregistering, StageStop, func(e *DrainExecutor, _ *fakeVM, _ *fakeDrainControl) { e.VM = nil }},
		{"stop", operations.StateDeregistering, StageStop, func(_ *DrainExecutor, vm *fakeVM, _ *fakeDrainControl) { vm.stopErr = errors.New("raw") }},
		{"missing delete vm", operations.StateStopping, StageConfirm, func(e *DrainExecutor, _ *fakeVM, _ *fakeDrainControl) { e.VM = nil }},
		{"delete confirm", operations.StateStopping, StageConfirm, func(_ *DrainExecutor, _ *fakeVM, control *fakeDrainControl) { control.confirmErr = errors.New("raw") }},
		{"delete", operations.StateStopping, StageDelete, func(_ *DrainExecutor, vm *fakeVM, _ *fakeDrainControl) { vm.deleteErr = errors.New("raw") }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor, state, vm, control := drainFixture(test.state)
			test.mutate(&executor, vm, control)
			err := executor.Execute(context.Background(), operation)
			expectStage(t, err, test.stage, state, test.state)
			if strings.Contains(err.Error(), "raw") {
				t.Fatalf("raw error leaked: %v", err)
			}
		})
	}
}

func TestDrainExecutorDefaultsPersistenceAndHelpers(t *testing.T) {
	operation := operations.Operation{Kind: OperationDrain, ResourceID: "trf-small-1"}
	executor, state, _, _ := drainFixture(operations.StateDraining)
	state.advanceErr = errors.New("database unavailable")
	err := executor.Execute(context.Background(), operation)
	if err == nil || err.Error() != safeError(StagePersist).Error() || state.instance.State != operations.StateDraining {
		t.Fatalf("persist error=%v state=%s", err, state.instance.State)
	}
	executor, _, _, control := drainFixture(operations.StateDraining)
	executor.Now = nil
	executor.ConfirmationMaxAge = 0
	now := time.Now().UTC()
	control.confirmations = []operations.DeletionConfirmation{
		{Fresh: true, RunnerInactive: true, JobsInactive: true, ObservedAt: now},
		{Fresh: true, RunnerInactive: true, JobsInactive: true, ObservedAt: now},
	}
	if err := executor.Execute(context.Background(), operation); err != nil {
		t.Fatal(err)
	}
	if got := (ProvisionExecutor{}).registrationPollInterval(); got != 250*time.Millisecond {
		t.Fatalf("default registration poll interval=%s", got)
	}
	if got := (DrainExecutor{}).confirmationPollInterval(); got != 250*time.Millisecond {
		t.Fatalf("default confirmation poll interval=%s", got)
	}
	if err := (DrainExecutor{}).waitConfirmed(context.Background(), "vm"); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("nil control confirmation error=%v", err)
	}
	failed := lifecycleInstance(operations.StateFailed)
	before := len(state.changes)
	_ = executor.fail(context.Background(), failed, StageDelete)
	if len(state.changes) != before {
		t.Fatal("failed instance transitioned again")
	}
}

type blockingStdin struct{}

func (blockingStdin) Run(ctx context.Context, _ io.Reader, _ ...string) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestBootstrapAndExecRunnerFailureBranches(t *testing.T) {
	if err := (ExecStdinRunner{}).Run(context.Background(), nil); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("empty binary error=%v", err)
	}
	if err := (ExecStdinRunner{Binary: "/usr/bin/true"}).Run(context.Background(), strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if err := (ExecStdinRunner{Binary: "/definitely/missing"}).Run(context.Background(), nil); err == nil {
		t.Fatal("missing binary succeeded")
	}
	valid := githubscaleset.NewJITSecret("jit")
	if err := (StdinBootstrapper{}).Bootstrap(context.Background(), "vm", valid); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("nil runner error=%v", err)
	}
	for _, test := range []struct {
		name   string
		secret *githubscaleset.JITSecret
	}{
		{"nil", nil},
		{"empty", githubscaleset.NewJITSecret("")},
		{"large", githubscaleset.NewJITSecret(strings.Repeat("x", maxJITBytes+1))},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := (StdinBootstrapper{Runner: &captureStdin{}}).Bootstrap(context.Background(), "vm", test.secret)
			if !errors.Is(err, operations.ErrInvalid) {
				t.Fatalf("error=%v", err)
			}
		})
	}
	if err := (StdinBootstrapper{Runner: &captureStdin{}}).Bootstrap(context.Background(), "../vm", githubscaleset.NewJITSecret("jit")); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("bad name error=%v", err)
	}
	secret := githubscaleset.NewJITSecret("jit")
	err := (StdinBootstrapper{Runner: blockingStdin{}, Timeout: time.Millisecond}).Bootstrap(context.Background(), "vm", secret)
	if !errors.Is(err, context.DeadlineExceeded) || secret.Reveal() != "" {
		t.Fatalf("timeout error=%v secret=%q", err, secret.Reveal())
	}
}

func TestExecutorsBoundCorruptStoreProgress(t *testing.T) {
	calls := []string{}
	provisionInstance := lifecycleInstance(operations.StateCloning)
	provision := ProvisionExecutor{State: stuckState{instance: provisionInstance}, VM: fakeVM{calls: &calls}}
	if err := provision.Execute(context.Background(), operations.Operation{Kind: OperationProvision, ResourceID: provisionInstance.ID}); err == nil || err.Error() != safeError(StagePersist).Error() {
		t.Fatalf("provision loop guard error=%v", err)
	}
	drainInstance := lifecycleInstance(operations.StateDeregistering)
	drain := DrainExecutor{State: stuckState{instance: drainInstance}, VM: fakeVM{calls: &calls}}
	if err := drain.Execute(context.Background(), operations.Operation{Kind: OperationDrain, ResourceID: drainInstance.ID}); err == nil || err.Error() != safeError(StagePersist).Error() {
		t.Fatalf("drain loop guard error=%v", err)
	}
}
