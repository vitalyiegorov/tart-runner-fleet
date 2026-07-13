package lifecycle

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/tart"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

type memoryState struct {
	instance   operations.Instance
	changes    []StateChange
	advanceErr error
}

func (s *memoryState) Instance(_ context.Context, id string) (operations.Instance, error) {
	if id != s.instance.ID {
		return operations.Instance{}, operations.ErrNotFound
	}
	return s.instance, nil
}

func (s *memoryState) Advance(_ context.Context, change StateChange) (operations.Instance, error) {
	if s.advanceErr != nil {
		return operations.Instance{}, s.advanceErr
	}
	if change.InstanceID != s.instance.ID || change.ExpectedState != s.instance.State || change.ExpectedVersion != s.instance.Version ||
		!change.ExpectedState.CanTransitionTo(change.NextState) {
		return operations.Instance{}, operations.ErrConflict
	}
	s.changes = append(s.changes, change)
	s.instance.State = change.NextState
	s.instance.Version++
	s.instance.LastError = change.FailureCode
	return s.instance, nil
}

type fakeVM struct {
	calls                       *[]string
	cloneErr, startErr, stopErr error
	deleteErr                   error
}

func (v fakeVM) Clone(_ context.Context, request tart.Request) error {
	*v.calls = append(*v.calls, "clone:"+request.Base+":"+request.Name)
	return v.cloneErr
}
func (v fakeVM) Start(_ context.Context, name string, _ operations.Ownership) error {
	*v.calls = append(*v.calls, "start:"+name)
	return v.startErr
}
func (v fakeVM) Stop(_ context.Context, name string, _ operations.Ownership) error {
	*v.calls = append(*v.calls, "stop:"+name)
	return v.stopErr
}
func (v fakeVM) Delete(_ context.Context, name string, _ operations.Ownership) error {
	*v.calls = append(*v.calls, "delete:"+name)
	return v.deleteErr
}

type fakeReady struct {
	calls *[]string
	err   error
}

func (r fakeReady) Wait(_ context.Context, instance operations.Instance) error {
	*r.calls = append(*r.calls, "ready:"+instance.ID)
	return r.err
}

type fakeRegistration struct {
	calls         *[]string
	registered    bool
	delayedChecks int
	secret        *githubscaleset.JITSecret
	registeredErr error
	acquireErr    error
}

func (r *fakeRegistration) Registered(_ context.Context, name string) (bool, error) {
	*r.calls = append(*r.calls, "registered:"+name)
	if r.delayedChecks > 0 {
		r.delayedChecks--
		return false, r.registeredErr
	}
	return r.registered, r.registeredErr
}
func (r *fakeRegistration) AcquireAndGenerateJIT(_ context.Context, requestID int64, name, workFolder string) (*githubscaleset.JITSecret, error) {
	*r.calls = append(*r.calls, "acquire:"+name+":"+workFolder)
	if requestID <= 0 {
		return nil, operations.ErrInvalid
	}
	return r.secret, r.acquireErr
}

type fakeBootstrap struct {
	calls        *[]string
	registration *fakeRegistration
	err          error
}

func (b fakeBootstrap) Bootstrap(_ context.Context, name string, secret *githubscaleset.JITSecret) error {
	*b.calls = append(*b.calls, "bootstrap:"+name)
	if secret == nil || secret.Reveal() == "" {
		return operations.ErrInvalid
	}
	if b.err == nil {
		b.registration.registered = true
	}
	return b.err
}

func lifecycleInstance(state operations.State) operations.Instance {
	return operations.Instance{
		ID: "trf-small-1", Repo: "owner/repo", Platform: domain.PlatformLinux, Profile: "small", Route: "linux-small",
		Resources: domain.Resources{CPU: 1, MemoryMB: 2048, Slots: 1},
		Demand:    domain.DemandKey{Repo: "owner/repo", RunID: 11, Attempt: 1, JobID: 41}, State: state, Version: 3,
		Ownership: operations.Ownership{ControllerID: "controller", ResourceID: "owner/repo/11/1/41", OperationID: "spawn"},
	}
}

func TestProvisionExecutorRunsOwnedLifecycleInOrder(t *testing.T) {
	calls := []string{}
	secret := githubscaleset.NewJITSecret("jit-value")
	registration := &fakeRegistration{calls: &calls, secret: secret}
	state := &memoryState{instance: lifecycleInstance(operations.StatePlanned)}
	executor := ProvisionExecutor{
		State: state, VM: fakeVM{calls: &calls}, Ready: fakeReady{calls: &calls}, Registration: registration,
		Bootstrap: fakeBootstrap{calls: &calls, registration: registration}, Bases: map[domain.Platform]string{domain.PlatformLinux: "linux-base"},
	}
	operation := operations.Operation{Kind: OperationProvision, ResourceID: state.instance.ID}
	if err := executor.Execute(context.Background(), operation); err != nil {
		t.Fatal(err)
	}
	wantCalls := []string{
		"clone:linux-base:trf-small-1", "start:trf-small-1", "ready:trf-small-1", "registered:trf-small-1",
		"acquire:trf-small-1:_work", "bootstrap:trf-small-1", "registered:trf-small-1",
	}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("calls=%#v", calls)
	}
	wantStates := []operations.State{operations.StateCloning, operations.StateBooting, operations.StateReachable, operations.StateRegistering, operations.StateOnlineIdle}
	if got := changedStates(state.changes); !reflect.DeepEqual(got, wantStates) {
		t.Fatalf("states=%#v", got)
	}
	if secret.Reveal() != "" {
		t.Fatal("JIT secret survived bootstrap")
	}
}

func TestProvisionExecutorResumesWithoutSecondJITOrVMEffect(t *testing.T) {
	calls := []string{}
	registration := &fakeRegistration{calls: &calls, registered: true, secret: githubscaleset.NewJITSecret("unused")}
	state := &memoryState{instance: lifecycleInstance(operations.StateRegistering)}
	executor := ProvisionExecutor{State: state, VM: fakeVM{calls: &calls}, Ready: fakeReady{calls: &calls}, Registration: registration,
		Bootstrap: fakeBootstrap{calls: &calls, registration: registration}, Bases: map[domain.Platform]string{domain.PlatformLinux: "linux-base"}}
	operation := operations.Operation{Kind: OperationProvision, ResourceID: state.instance.ID}
	if err := executor.Execute(context.Background(), operation); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{"registered:trf-small-1"}) || state.instance.State != operations.StateOnlineIdle {
		t.Fatalf("resume calls=%#v state=%s", calls, state.instance.State)
	}
	if err := executor.Execute(context.Background(), operation); err != nil || len(calls) != 1 {
		t.Fatalf("terminal replay calls=%#v err=%v", calls, err)
	}
}

func TestProvisionExecutorWaitsForDelayedRegistration(t *testing.T) {
	calls := []string{}
	registration := &fakeRegistration{calls: &calls, delayedChecks: 3, secret: githubscaleset.NewJITSecret("jit")}
	state := &memoryState{instance: lifecycleInstance(operations.StatePlanned)}
	executor := ProvisionExecutor{
		State: state, VM: fakeVM{calls: &calls}, Ready: fakeReady{calls: &calls}, Registration: registration,
		Bootstrap: fakeBootstrap{calls: &calls, registration: registration}, Bases: map[domain.Platform]string{domain.PlatformLinux: "linux-base"},
		RegistrationTimeout: time.Second, RegistrationPollInterval: time.Millisecond,
		After: func(time.Duration) <-chan time.Time {
			ready := make(chan time.Time, 1)
			ready <- time.Unix(1, 0)
			return ready
		},
	}
	if err := executor.Execute(context.Background(), operations.Operation{Kind: OperationProvision, ResourceID: state.instance.ID}); err != nil {
		t.Fatal(err)
	}
	registeredChecks := 0
	for _, call := range calls {
		if call == "registered:"+state.instance.ID {
			registeredChecks++
		}
	}
	if registeredChecks != 4 || state.instance.State != operations.StateOnlineIdle {
		t.Fatalf("registration checks=%d state=%s calls=%v", registeredChecks, state.instance.State, calls)
	}
}

func TestProvisionExecutorRegistrationWaitIsBoundedAndCancellationSafe(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		calls := []string{}
		registration := &fakeRegistration{calls: &calls}
		state := &memoryState{instance: lifecycleInstance(operations.StateRegistering)}
		executor := ProvisionExecutor{State: state, Registration: registration,
			RegistrationTimeout: 2 * time.Millisecond, RegistrationPollInterval: time.Millisecond}
		err := executor.Execute(context.Background(), operations.Operation{Kind: OperationProvision, ResourceID: state.instance.ID})
		if err == nil || err.Error() != safeError(StageRegister).Error() || state.instance.State != operations.StateFailed {
			t.Fatalf("timeout error=%v state=%s calls=%v", err, state.instance.State, calls)
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		calls := []string{}
		registration := &fakeRegistration{calls: &calls}
		state := &memoryState{instance: lifecycleInstance(operations.StateRegistering)}
		ctx, cancel := context.WithCancel(context.Background())
		executor := ProvisionExecutor{State: state, Registration: registration,
			RegistrationTimeout: time.Second, RegistrationPollInterval: time.Millisecond,
			After: func(time.Duration) <-chan time.Time {
				cancel()
				return make(chan time.Time)
			},
		}
		err := executor.Execute(ctx, operations.Operation{Kind: OperationProvision, ResourceID: state.instance.ID})
		if !errors.Is(err, context.Canceled) || state.instance.State != operations.StateRegistering || len(state.changes) != 0 {
			t.Fatalf("cancellation error=%v state=%s changes=%v", err, state.instance.State, state.changes)
		}
	})
}

func TestProvisionFailureIsSanitizedAndDurablyFailed(t *testing.T) {
	calls := []string{}
	state := &memoryState{instance: lifecycleInstance(operations.StatePlanned)}
	executor := ProvisionExecutor{State: state, VM: fakeVM{calls: &calls, cloneErr: errors.New("top-secret backend detail")},
		Ready: fakeReady{calls: &calls}, Bases: map[domain.Platform]string{domain.PlatformLinux: "linux-base"}}
	err := executor.Execute(context.Background(), operations.Operation{Kind: OperationProvision, ResourceID: state.instance.ID})
	if err == nil || strings.Contains(err.Error(), "top-secret") {
		t.Fatalf("unsafe error=%v", err)
	}
	if state.instance.State != operations.StateFailed || state.instance.LastError != string(StageClone) {
		t.Fatalf("failed state=%#v", state.instance)
	}
}

func TestProvisionExternalFailureRemainsResumableAcrossAttempts(t *testing.T) {
	calls := []string{}
	secret := githubscaleset.NewJITSecret("jit-value")
	registration := &fakeRegistration{calls: &calls, secret: secret}
	vm := &fakeVM{calls: &calls, cloneErr: errors.New("transient clone failure")}
	state := &memoryState{instance: lifecycleInstance(operations.StatePlanned)}
	executor := ProvisionExecutor{
		State: state, VM: vm, Ready: fakeReady{calls: &calls}, Registration: registration,
		Bootstrap: fakeBootstrap{calls: &calls, registration: registration}, Bases: map[domain.Platform]string{domain.PlatformLinux: "linux-base"},
	}
	operation := operations.Operation{Kind: OperationProvision, ResourceID: state.instance.ID}
	if err := executor.Execute(context.Background(), operation); err == nil || state.instance.State != operations.StatePlanned {
		t.Fatalf("first attempt error=%v state=%s", err, state.instance.State)
	}
	vm.cloneErr = nil
	if err := executor.Execute(context.Background(), operation); err != nil || state.instance.State != operations.StateOnlineIdle {
		t.Fatalf("recovery error=%v state=%s calls=%v", err, state.instance.State, calls)
	}
}

type fakeDrainControl struct {
	calls         *[]string
	safe          bool
	guardErr      error
	deregisterErr error
	confirmations []operations.DeletionConfirmation
	confirmErr    error
}

func (d *fakeDrainControl) SafeToDeregister(_ context.Context, instance operations.Instance) (bool, error) {
	*d.calls = append(*d.calls, "guard:"+instance.ID)
	return d.safe, d.guardErr
}
func (d *fakeDrainControl) Deregister(_ context.Context, instance operations.Instance) error {
	*d.calls = append(*d.calls, "deregister:"+instance.ID)
	return d.deregisterErr
}
func (d *fakeDrainControl) ConfirmDeletion(_ context.Context, instance string) (operations.DeletionConfirmation, error) {
	*d.calls = append(*d.calls, "confirm:"+instance)
	if d.confirmErr != nil {
		return operations.DeletionConfirmation{}, d.confirmErr
	}
	if len(d.confirmations) == 0 {
		return operations.DeletionConfirmation{}, nil
	}
	confirmation := d.confirmations[0]
	d.confirmations = d.confirmations[1:]
	return confirmation, nil
}

func TestDrainExecutorDeregistersStopsAndDeletesInOrder(t *testing.T) {
	now := time.Unix(1000, 0).UTC()
	calls := []string{}
	safe := operations.DeletionConfirmation{Fresh: true, RunnerInactive: true, JobsInactive: true, ObservedAt: now}
	control := &fakeDrainControl{calls: &calls, safe: true, confirmations: []operations.DeletionConfirmation{safe, safe}}
	state := &memoryState{instance: lifecycleInstance(operations.StateDraining)}
	executor := DrainExecutor{State: state, VM: fakeVM{calls: &calls}, Control: control, Now: func() time.Time { return now }, ConfirmationMaxAge: time.Minute}
	operation := operations.Operation{Kind: OperationDrain, ResourceID: state.instance.ID}
	if err := executor.Execute(context.Background(), operation); err != nil {
		t.Fatal(err)
	}
	wantCalls := []string{"guard:trf-small-1", "deregister:trf-small-1", "confirm:trf-small-1", "stop:trf-small-1", "confirm:trf-small-1", "delete:trf-small-1"}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("calls=%#v", calls)
	}
	wantStates := []operations.State{operations.StateDeregistering, operations.StateStopping, operations.StateDeleted}
	if got := changedStates(state.changes); !reflect.DeepEqual(got, wantStates) {
		t.Fatalf("states=%#v", got)
	}
	if err := executor.Execute(context.Background(), operation); err != nil || !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("delete replay calls=%#v err=%v", calls, err)
	}
}

func TestDrainExecutorFailsClosedBeforeStopOrDelete(t *testing.T) {
	now := time.Unix(1000, 0).UTC()
	calls := []string{}
	control := &fakeDrainControl{calls: &calls, safe: true, confirmations: []operations.DeletionConfirmation{{Fresh: false}}}
	state := &memoryState{instance: lifecycleInstance(operations.StateDraining)}
	executor := DrainExecutor{State: state, VM: fakeVM{calls: &calls}, Control: control, Now: func() time.Time { return now }, ConfirmationMaxAge: time.Minute}
	err := executor.Execute(context.Background(), operations.Operation{Kind: OperationDrain, ResourceID: state.instance.ID})
	if err == nil || state.instance.State != operations.StateFailed || state.instance.LastError != string(StageConfirm) {
		t.Fatalf("error=%v state=%#v", err, state.instance)
	}
	for _, call := range calls {
		if strings.HasPrefix(call, "stop:") || strings.HasPrefix(call, "delete:") {
			t.Fatalf("destructive call after uncertain confirmation: %v", calls)
		}
	}
}

func TestDrainExternalFailureRemainsResumableAcrossAttempts(t *testing.T) {
	now := time.Unix(1000, 0).UTC()
	calls := []string{}
	safe := operations.DeletionConfirmation{Fresh: true, RunnerInactive: true, JobsInactive: true, ObservedAt: now}
	control := &fakeDrainControl{calls: &calls, safe: true, deregisterErr: errors.New("transient GitHub failure"), confirmations: []operations.DeletionConfirmation{safe, safe}}
	state := &memoryState{instance: lifecycleInstance(operations.StateDraining)}
	executor := DrainExecutor{State: state, VM: fakeVM{calls: &calls}, Control: control, Now: func() time.Time { return now }, ConfirmationMaxAge: time.Minute}
	operation := operations.Operation{Kind: OperationDrain, ResourceID: state.instance.ID}
	if err := executor.Execute(context.Background(), operation); err == nil || state.instance.State != operations.StateDraining {
		t.Fatalf("first attempt error=%v state=%s", err, state.instance.State)
	}
	control.deregisterErr = nil
	if err := executor.Execute(context.Background(), operation); err != nil || state.instance.State != operations.StateDeleted {
		t.Fatalf("recovery error=%v state=%s calls=%v", err, state.instance.State, calls)
	}
}

type captureStdin struct {
	args  []string
	stdin string
	err   error
}

func (c *captureStdin) Run(_ context.Context, stdin io.Reader, args ...string) error {
	c.args = append([]string(nil), args...)
	data, _ := io.ReadAll(stdin)
	c.stdin = string(data)
	return c.err
}

func TestStdinBootstrapperKeepsJITOutOfArgvAndDestroysIt(t *testing.T) {
	runner := &captureStdin{}
	secret := githubscaleset.NewJITSecret("jit-super-secret")
	bootstrap := StdinBootstrapper{Runner: runner}
	if err := bootstrap.Bootstrap(context.Background(), "trf-small-1", secret); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(runner.args, " "), "jit-super-secret") || runner.stdin != "jit-super-secret\n" {
		t.Fatalf("argv=%q stdin=%q", runner.args, runner.stdin)
	}
	wantArgs := []string{"exec", "-i", "trf-small-1", bootstrapHelper}
	if !reflect.DeepEqual(runner.args, wantArgs) {
		t.Fatalf("bootstrap argv=%q want=%q", runner.args, wantArgs)
	}
	if secret.Reveal() != "" {
		t.Fatal("secret not destroyed")
	}
	runner.err = errors.New("jit-super-secret from child")
	secret = githubscaleset.NewJITSecret("jit-super-secret")
	if err := bootstrap.Bootstrap(context.Background(), "trf-small-1", secret); err == nil || strings.Contains(err.Error(), "jit-super-secret") {
		t.Fatalf("child output leaked: %v", err)
	}
}

func changedStates(changes []StateChange) []operations.State {
	states := make([]operations.State, len(changes))
	for i, change := range changes {
		states[i] = change.NextState
	}
	return states
}
