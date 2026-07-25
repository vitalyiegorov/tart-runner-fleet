package lifecycle

import (
	"context"
	"errors"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// RunnerBusy is deliberately keyed by the runner, not by the demand the VM was
// spawned for: GitHub's scale-set brokering may hand a runner a different
// matching job, and it records the runner it chose on THAT job's demand row.
// JobStarted/JobActive read the spawned-for demand only, which is why a
// completed spawned-for demand said "idle" while the runner was mid-flight on a
// brokered job in the 2026-07-25 incident.
func TestControlRouterRunnerBusyReadsRunnerScopedEvidence(t *testing.T) {
	instance := lifecycleInstance(operations.StateDraining)
	binding := map[SourceKey]SourceBinding{
		{Repo: instance.Repo, Profile: instance.Profile}: {StoreKey: 9, Source: &fakeScaleControl{}},
	}
	for _, test := range []struct {
		name    string
		sources map[SourceKey]SourceBinding
		demand  DemandReader
		want    bool
		wantErr error
	}{
		{name: "a job executing on the runner is busy", sources: binding,
			demand: fakeDemandReader{runnerJob: true}, want: true},
		{name: "no job executing on the runner is idle", sources: binding,
			demand: fakeDemandReader{}},
		{name: "an unreadable observation fails closed", sources: binding,
			demand: fakeDemandReader{runnerErr: context.DeadlineExceeded}, wantErr: context.DeadlineExceeded},
		{name: "an unbound scale set is uncertain, never idle",
			demand: fakeDemandReader{runnerJob: true}, wantErr: operations.ErrUncertain},
		{name: "a missing demand reader is uncertain, never idle", sources: binding,
			wantErr: operations.ErrUncertain},
	} {
		t.Run(test.name, func(t *testing.T) {
			router := ControlRouter{State: &memoryState{instance: instance}, Demand: test.demand, Sources: test.sources}
			busy, err := router.RunnerBusy(context.Background(), instance)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("err = %v, want %v", err, test.wantErr)
			}
			if busy != test.want {
				t.Fatalf("busy = %v, want %v", busy, test.want)
			}
		})
	}
}

// The query must be scoped to the instance's own scale-set binding and its own
// runner name: GitHub only brokers a scale set's jobs to that scale set's
// runners, so widening the scope would import a foreign fleet's evidence.
func TestControlRouterRunnerBusyScopesToTheInstanceBinding(t *testing.T) {
	instance := lifecycleInstance(operations.StateDraining)
	var query runnerJobQuery
	router := ControlRouter{State: &memoryState{instance: instance},
		Demand: fakeDemandReader{runnerQuery: &query},
		Sources: map[SourceKey]SourceBinding{
			{Repo: instance.Repo, Profile: domain.ProfileID("small")}: {StoreKey: 9, Source: &fakeScaleControl{}},
		}}
	if _, err := router.RunnerBusy(context.Background(), instance); err != nil {
		t.Fatal(err)
	}
	if query.scaleSetID != 9 || query.runner != instance.ID {
		t.Fatalf("busy evidence queried with %#v, want scale set 9 and runner %s", query, instance.ID)
	}
}
