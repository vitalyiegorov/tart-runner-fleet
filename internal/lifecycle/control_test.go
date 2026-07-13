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
func (f *fakeScaleControl) Deregister(context.Context, string) error {
	f.deregister++
	f.registered = false
	return f.err
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
	secret, err := router.AcquireAndGenerateJIT(context.Background(), state.instance.Demand.JobID, state.instance.ID, "_work")
	if err != nil || secret == nil || source.acquired != state.instance.Demand.JobID {
		t.Fatalf("Acquire() = %v, %v", secret, err)
	}
	secret.Destroy()
	safe, err := router.SafeToDeregister(context.Background(), state.instance)
	if err != nil || !safe {
		t.Fatalf("SafeToDeregister() = %v, %v", safe, err)
	}
	if err := router.Deregister(context.Background(), state.instance); err != nil || source.deregister != 1 {
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
