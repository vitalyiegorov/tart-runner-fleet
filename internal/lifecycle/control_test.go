package lifecycle

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

type fakeScaleControl struct {
	registered bool
	acquired   int64
	generated  int
	deregister int
	err        error
}

func (f *fakeScaleControl) Registered(context.Context, string) (bool, error) {
	return f.registered, f.err
}
func (f *fakeScaleControl) AcquireAndGenerateJIT(_ context.Context, id int64, _, _ string) (*githubscaleset.JITSecret, error) {
	f.acquired = id
	return githubscaleset.NewJITSecret("jit"), f.err
}
func (f *fakeScaleControl) GenerateJIT(context.Context, string, string) (*githubscaleset.JITSecret, error) {
	f.generated++
	return githubscaleset.NewJITSecret("jit"), f.err
}
func (f *fakeScaleControl) Deregister(context.Context, string) error {
	f.deregister++
	f.registered = false
	return f.err
}

func TestControlRouterGeneratesJITDirectlyForPreassignedDemand(t *testing.T) {
	state := &memoryState{instance: lifecycleInstance(operations.StateReachable)}
	state.instance.Demand.JobID = 1<<62 | 7
	source := &fakeScaleControl{}
	router := ControlRouter{State: state, Sources: map[SourceKey]SourceBinding{
		{Repo: state.instance.Repo, Profile: state.instance.Profile}: {StoreKey: 9, Source: source},
	}}
	secret, err := router.AcquireAndGenerateJIT(context.Background(), state.instance.Demand.JobID, state.instance.ID, "_work")
	if err != nil || secret == nil {
		t.Fatalf("preassigned JIT = %v, %v", secret, err)
	}
	secret.Destroy()
	if source.generated != 1 || source.acquired != 0 {
		t.Fatalf("direct/acquire calls = %d/%d", source.generated, source.acquired)
	}
}

type fakeDemandReader struct {
	record operations.DemandRecord
	err    error
}

func (f fakeDemandReader) DemandRecord(context.Context, int64, int64) (operations.DemandRecord, error) {
	return f.record, f.err
}

func TestControlRouterResolvesRegistrationAndFreshDrainState(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	state := &memoryState{instance: lifecycleInstance(operations.StateDraining)}
	source := &fakeScaleControl{registered: true}
	router := ControlRouter{State: state, Demand: fakeDemandReader{record: operations.DemandRecord{Status: operations.DemandJobCompleted}},
		Sources: map[SourceKey]SourceBinding{{Repo: "owner/repo", Profile: domain.ProfileID("small")}: {StoreKey: 9, Source: source}},
		Now:     func() time.Time { return now }}
	registered, err := router.Registered(context.Background(), state.instance.ID)
	if err != nil || !registered {
		t.Fatalf("Registered() = %v, %v", registered, err)
	}
	if err := router.ResetRegistration(context.Background(), state.instance.ID); err != nil || source.registered || source.deregister != 1 {
		t.Fatalf("ResetRegistration() = %v registered=%v count=%d", err, source.registered, source.deregister)
	}
	secret, err := router.AcquireAndGenerateJIT(context.Background(), state.instance.Demand.JobID, state.instance.ID, "_work")
	if err != nil || secret == nil || source.acquired != state.instance.Demand.JobID {
		t.Fatalf("Acquire() = %v, %v", secret, err)
	}
	secret.Destroy()
	safe, err := router.SafeToDeregister(context.Background(), state.instance)
	if err != nil || !safe {
		t.Fatalf("SafeToDeregister() = %v, %v", safe, err)
	}
	if err := router.Deregister(context.Background(), state.instance); err != nil || source.deregister != 2 {
		t.Fatalf("Deregister() = %v count=%d", err, source.deregister)
	}
	confirmation, err := router.ConfirmDeletion(context.Background(), state.instance.ID)
	if err != nil || !confirmation.Safe(now, time.Second) {
		t.Fatalf("ConfirmDeletion() = %#v, %v", confirmation, err)
	}
}

func TestControlRouterFailsClosedForUnknownOrActiveDemand(t *testing.T) {
	state := &memoryState{instance: lifecycleInstance(operations.StateDraining)}
	router := ControlRouter{State: state}
	if _, err := router.Registered(context.Background(), state.instance.ID); !errors.Is(err, operations.ErrUncertain) {
		t.Fatalf("unknown source = %v", err)
	}
	source := &fakeScaleControl{}
	router.Sources = map[SourceKey]SourceBinding{{Repo: state.instance.Repo, Profile: state.instance.Profile}: {StoreKey: 1, Source: source}}
	router.Demand = fakeDemandReader{record: operations.DemandRecord{Status: operations.DemandJobStarted}}
	if safe, err := router.SafeToDeregister(context.Background(), state.instance); err != nil || safe {
		t.Fatalf("active demand = %v, %v", safe, err)
	}
	if _, err := router.AcquireAndGenerateJIT(context.Background(), 999, state.instance.ID, "_work"); !errors.Is(err, operations.ErrConflict) {
		t.Fatalf("wrong request = %v", err)
	}
}

func TestControlRouterAcceptsStoppedAssignmentRecoveryOnlyAfterRunnerDisappears(t *testing.T) {
	now := time.Unix(150, 0).UTC()
	state := &memoryState{instance: lifecycleInstance(operations.StateDraining)}
	state.instance.DrainPhase = 2
	source := &fakeScaleControl{}
	router := ControlRouter{State: state, Demand: fakeDemandReader{record: operations.DemandRecord{Status: operations.DemandJobAssigned}},
		Sources: map[SourceKey]SourceBinding{{Repo: state.instance.Repo, Profile: state.instance.Profile}: {StoreKey: 3, Source: source}},
		Now:     func() time.Time { return now }}
	if safe, err := router.SafeToDeregister(context.Background(), state.instance); err != nil || !safe {
		t.Fatalf("recovered drain guard = %v, %v", safe, err)
	}
	confirmation, err := router.ConfirmDeletion(context.Background(), state.instance.ID)
	if err != nil || !confirmation.Safe(now, time.Second) {
		t.Fatalf("recovered deletion confirmation = %#v, %v", confirmation, err)
	}
	source.registered = true
	if safe, err := router.SafeToDeregister(context.Background(), state.instance); err != nil || safe {
		t.Fatalf("registered recovery runner = %v, %v", safe, err)
	}
}

func TestControlRouterPropagatesUnavailableStateAndScopedObservations(t *testing.T) {
	ctx := context.Background()
	instance := lifecycleInstance(operations.StateDraining)
	state := &memoryState{instance: instance}
	source := &fakeScaleControl{}
	binding := SourceBinding{StoreKey: 7, Source: source}
	router := ControlRouter{
		State:   state,
		Sources: map[SourceKey]SourceBinding{{Repo: instance.Repo, Profile: instance.Profile}: binding},
	}

	for name, probe := range map[string]func() error{
		"registration": func() error { _, err := (ControlRouter{}).Registered(ctx, instance.ID); return err },
		"acquire": func() error {
			_, err := router.AcquireAndGenerateJIT(ctx, instance.Demand.JobID, "", "_work")
			return err
		},
		"confirmation": func() error { _, err := router.ConfirmDeletion(ctx, ""); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if err := probe(); !errors.Is(err, operations.ErrInvalid) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	missing := ControlRouter{State: missingState{}}
	if _, _, err := missing.resolve(ctx, instance.ID); !errors.Is(err, operations.ErrNotFound) {
		t.Fatalf("missing instance error = %v", err)
	}

	unknown := instance
	unknown.Profile = "unknown"
	if safe, err := router.SafeToDeregister(ctx, unknown); safe || !errors.Is(err, operations.ErrUncertain) {
		t.Fatalf("unknown drain binding = %v, %v", safe, err)
	}
	if err := router.Deregister(ctx, unknown); !errors.Is(err, operations.ErrUncertain) {
		t.Fatalf("unknown deregistration binding = %v", err)
	}

	if safe, err := router.SafeToDeregister(ctx, instance); safe || !errors.Is(err, operations.ErrUncertain) {
		t.Fatalf("missing demand observation = %v, %v", safe, err)
	}
	if _, err := router.ConfirmDeletion(ctx, instance.ID); !errors.Is(err, operations.ErrUncertain) {
		t.Fatalf("missing confirmation demand = %v", err)
	}

	observationErr := errors.New("observation unavailable")
	router.Demand = fakeDemandReader{err: observationErr}
	if safe, err := router.SafeToDeregister(ctx, instance); safe || !errors.Is(err, observationErr) {
		t.Fatalf("failed demand observation = %v, %v", safe, err)
	}
	if _, err := router.ConfirmDeletion(ctx, instance.ID); !errors.Is(err, observationErr) {
		t.Fatalf("failed confirmation demand = %v", err)
	}

	router.Demand = fakeDemandReader{record: operations.DemandRecord{Status: operations.DemandJobCompleted}}
	source.err = observationErr
	if _, err := router.ConfirmDeletion(ctx, instance.ID); !errors.Is(err, observationErr) {
		t.Fatalf("failed runner observation = %v", err)
	}
}

func TestControlRouterUsesCurrentUTCTimeWhenClockIsNotInjected(t *testing.T) {
	instance := lifecycleInstance(operations.StateDraining)
	source := &fakeScaleControl{}
	router := ControlRouter{
		State:  &memoryState{instance: instance},
		Demand: fakeDemandReader{record: operations.DemandRecord{Status: operations.DemandJobCompleted}},
		Sources: map[SourceKey]SourceBinding{
			{Repo: instance.Repo, Profile: instance.Profile}: {StoreKey: 1, Source: source},
		},
	}
	before := time.Now().UTC()
	confirmation, err := router.ConfirmDeletion(context.Background(), instance.ID)
	after := time.Now().UTC()
	if err != nil {
		t.Fatal(err)
	}
	if confirmation.ObservedAt.Before(before) || confirmation.ObservedAt.After(after) || confirmation.ObservedAt.Location() != time.UTC {
		t.Fatalf("observation time = %v, interval = [%v, %v]", confirmation.ObservedAt, before, after)
	}
}
