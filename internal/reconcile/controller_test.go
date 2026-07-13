package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

type fakeStore struct {
	state                           operations.SchedulerState
	instances                       map[string]operations.Instance
	applied                         []operations.Plan
	stateErr, instanceErr, applyErr error
}

func (f *fakeStore) SchedulerState(context.Context) (operations.SchedulerState, error) {
	return f.state, f.stateErr
}
func (f *fakeStore) Instance(_ context.Context, id string) (operations.Instance, error) {
	if f.instanceErr != nil {
		return operations.Instance{}, f.instanceErr
	}
	instance, ok := f.instances[id]
	if !ok {
		return operations.Instance{}, operations.ErrNotFound
	}
	return instance, nil
}
func (f *fakeStore) ApplyPlan(_ context.Context, plan operations.Plan) (bool, error) {
	if f.applyErr != nil {
		return false, f.applyErr
	}
	f.applied = append(f.applied, plan)
	return true, nil
}

var controllerNow = time.Date(2026, 7, 12, 20, 0, 0, 0, time.UTC)

func readyPlan(ops ...scheduler.Operation) scheduler.Plan {
	return scheduler.Plan{ID: "plan-123", Status: scheduler.PlanReady, Operations: ops, Next: scheduler.State{DRRCursor: "owner/repo"}}
}

func TestControllerObserveDoesNotWrite(t *testing.T) {
	store := &fakeStore{}
	applied, err := (Controller{Store: store, ControllerID: "controller", Mode: Observe}).Commit(context.Background(), readyPlan(), "7", controllerNow)
	if err != nil || applied || len(store.applied) != 0 {
		t.Fatalf("Commit() = %v, %v, writes=%d", applied, err, len(store.applied))
	}
}

func TestControllerShadowPersistsDecisionWithoutEffects(t *testing.T) {
	store := &fakeStore{state: operations.SchedulerState{Version: 4}}
	controller := Controller{Store: store, ControllerID: "controller", Mode: Shadow}
	applied, err := controller.Commit(context.Background(), readyPlan(scheduler.Operation{ID: "spawn", Kind: scheduler.OperationSpawn}), "cursor", controllerNow)
	if err != nil || !applied || len(store.applied) != 1 {
		t.Fatalf("Commit() = %v, %v, plans=%d", applied, err, len(store.applied))
	}
	got := store.applied[0]
	if len(got.Operations) != 0 || len(got.Instances) != 0 || got.Scheduler.Version != 5 || got.Scheduler.ObservationCursor != "cursor" {
		t.Fatalf("shadow durable plan = %#v", got)
	}
	if !strings.Contains(string(got.Scheduler.Data), "owner/repo") {
		t.Fatalf("scheduler state = %s", got.Scheduler.Data)
	}
}

// Regression: an idle shadow tick normalizes the durable scheduler snapshot on
// its first pass. Replaying the same deterministic scheduler plan must then be
// a no-op. Rewriting it with a new timestamp/version reuses the same plan ID
// with a different digest, which the durable store correctly rejects.
func TestControllerSkipsIdenticalNoOpPlanReplay(t *testing.T) {
	store := &fakeStore{state: operations.SchedulerState{
		Data:              json.RawMessage(`{}`),
		Reservations:      json.RawMessage(`[]`),
		DeficitRoundRobin: json.RawMessage(`{}`),
	}}
	controller := Controller{Store: store, ControllerID: "controller", Mode: Shadow}
	plan := readyPlan()

	applied, err := controller.Commit(context.Background(), plan, "", controllerNow)
	if err != nil || !applied || len(store.applied) != 1 {
		t.Fatalf("first Commit() = %v, %v, plans=%d", applied, err, len(store.applied))
	}
	store.state = store.applied[0].Scheduler

	applied, err = controller.Commit(context.Background(), plan, "", controllerNow.Add(time.Second))
	if err != nil || applied || len(store.applied) != 1 {
		t.Fatalf("replay Commit() = %v, %v, plans=%d", applied, err, len(store.applied))
	}
}

func TestSchedulerStateMatchRequiresEveryDurableFieldAndValidJSON(t *testing.T) {
	next := scheduler.State{DRRCursor: "owner/repo"}
	stateJSON, _ := json.Marshal(next)
	reservationJSON, _ := json.Marshal(next.Reservation)
	drrJSON, _ := json.Marshal(map[string]string{"cursor": next.DRRCursor})
	current := operations.SchedulerState{Data: stateJSON, Reservations: reservationJSON, DeficitRoundRobin: drrJSON, ObservationCursor: "cursor"}

	if matched, err := schedulerStateMatches(current, next, "cursor"); err != nil || !matched {
		t.Fatalf("exact state match = %v, %v", matched, err)
	}
	for _, tt := range []struct {
		name string
		edit func(*operations.SchedulerState)
	}{
		{name: "state", edit: func(value *operations.SchedulerState) { value.Data = json.RawMessage(`{}`) }},
		{name: "reservations", edit: func(value *operations.SchedulerState) { value.Reservations = json.RawMessage(`[]`) }},
		{name: "deficit round robin", edit: func(value *operations.SchedulerState) { value.DeficitRoundRobin = json.RawMessage(`{}`) }},
		{name: "cursor", edit: func(value *operations.SchedulerState) { value.ObservationCursor = "old" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			changed := current
			tt.edit(&changed)
			if matched, err := schedulerStateMatches(changed, next, "cursor"); err != nil || matched {
				t.Fatalf("changed state match = %v, %v", matched, err)
			}
		})
	}
	for _, invalid := range []json.RawMessage{json.RawMessage(`{"cursor":`), json.RawMessage(`{} {}`)} {
		changed := current
		changed.DeficitRoundRobin = invalid
		if matched, err := schedulerStateMatches(changed, next, "cursor"); err == nil || matched {
			t.Fatalf("invalid state match = %v, %v", matched, err)
		}
	}
}

func TestControllerAuthorityTranslatesSpawnAndDependentDrain(t *testing.T) {
	ownership := operations.Ownership{ControllerID: "controller", ResourceID: "old", OperationID: "birth"}
	store := &fakeStore{state: operations.SchedulerState{Version: 2}, instances: map[string]operations.Instance{
		"old-vm": {ID: "old-vm", State: operations.StateOnlineIdle, Version: 7, Ownership: ownership},
	}}
	drain := scheduler.Operation{ID: "drain-op", Kind: scheduler.OperationDrain, Instance: "old-vm", Profile: "builder", Route: "macos-builder"}
	spawn := scheduler.Operation{ID: "spawn-op", Kind: scheduler.OperationSpawn,
		Demand: domain.DemandKey{Repo: "owner/repo", RunID: 11, Attempt: 2, JobID: 33}, Profile: "maestro", Route: "macos-maestro", DependsOn: []string{"drain-op"}}
	controller := Controller{Store: store, ControllerID: "controller", Mode: Authority, Profiles: map[domain.ProfileID]domain.Profile{
		"maestro": {ID: "maestro", Route: "macos-maestro", Platform: domain.PlatformMacOS, Resources: domain.Resources{CPU: 4, MemoryMB: 7168, Slots: 1}},
	}}
	applied, err := controller.Commit(context.Background(), readyPlan(drain, spawn), "message-9", controllerNow)
	if err != nil || !applied {
		t.Fatalf("Commit() = %v, %v", applied, err)
	}
	got := store.applied[0]
	if got.ExpectedSchedulerVersion != 2 || got.Scheduler.Version != 3 || len(got.Instances) != 2 || len(got.Operations) != 2 {
		t.Fatalf("durable plan = %#v", got)
	}
	if got.Instances[0].ExpectedState != operations.StateOnlineIdle || got.Instances[0].Instance.State != operations.StateDraining {
		t.Fatalf("drain intent = %#v", got.Instances[0])
	}
	if got.Instances[1].ExpectedVersion != -1 || got.Instances[1].Instance.State != operations.StatePlanned {
		t.Fatalf("spawn intent = %#v", got.Instances[1])
	}
	if got.Instances[1].Instance.Repo != "owner/repo" || got.Instances[1].Instance.Resources.CPU != 4 {
		t.Fatalf("spawn metadata = %#v", got.Instances[1].Instance)
	}
	if !reflect.DeepEqual(got.Operations[1].DependsOn, []string{"drain-op"}) || got.Operations[0].Kind != "deregister" || got.Operations[1].Kind != "clone" {
		t.Fatalf("outbox operations = %#v", got.Operations)
	}
	var payload map[string]any
	if err := json.Unmarshal(got.Operations[1].Payload, &payload); err != nil || payload["repo"] != "owner/repo" {
		t.Fatalf("payload = %s, %v", got.Operations[1].Payload, err)
	}
}

func TestControllerFailClosedValidation(t *testing.T) {
	valid := readyPlan()
	tests := []struct {
		name       string
		controller Controller
		plan       scheduler.Plan
		now        time.Time
	}{
		{name: "nil store", controller: Controller{ControllerID: "c", Mode: Shadow}, plan: valid, now: controllerNow},
		{name: "no identity", controller: Controller{Store: &fakeStore{}, Mode: Shadow}, plan: valid, now: controllerNow},
		{name: "invalid mode", controller: Controller{Store: &fakeStore{}, ControllerID: "c", Mode: "bad"}, plan: valid, now: controllerNow},
		{name: "zero time", controller: Controller{Store: &fakeStore{}, ControllerID: "c", Mode: Shadow}, plan: valid},
		{name: "empty id", controller: Controller{Store: &fakeStore{}, ControllerID: "c", Mode: Shadow}, plan: scheduler.Plan{Status: scheduler.PlanReady}, now: controllerNow},
		{name: "invalid observation", controller: Controller{Store: &fakeStore{}, ControllerID: "c", Mode: Shadow}, plan: scheduler.Plan{ID: "x", Status: scheduler.PlanInvalidObservation}, now: controllerNow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := tt.controller.Commit(context.Background(), tt.plan, "", tt.now); err == nil {
				t.Fatal("Commit() unexpectedly succeeded")
			}
		})
	}
	blocked := scheduler.Plan{ID: "blocked", Status: scheduler.PlanBlockedObservation}
	if applied, err := (Controller{Store: &fakeStore{}, ControllerID: "c", Mode: Shadow}).Commit(context.Background(), blocked, "", controllerNow); err != nil || applied {
		t.Fatalf("blocked Commit() = %v, %v", applied, err)
	}
}

func TestControllerPropagatesStoreAndTranslationErrors(t *testing.T) {
	want := errors.New("store down")
	controller := Controller{Store: &fakeStore{stateErr: want}, ControllerID: "c", Mode: Authority}
	if _, err := controller.Commit(context.Background(), readyPlan(), "", controllerNow); !errors.Is(err, want) {
		t.Fatalf("state error = %v", err)
	}
	apply := Controller{Store: &fakeStore{applyErr: want}, ControllerID: "c", Mode: Shadow}
	if _, err := apply.Commit(context.Background(), readyPlan(), "", controllerNow); !errors.Is(err, want) {
		t.Fatalf("apply error = %v", err)
	}

	missing := Controller{Store: &fakeStore{instances: map[string]operations.Instance{}}, ControllerID: "c", Mode: Authority}
	plan := readyPlan(scheduler.Operation{ID: "d", Kind: scheduler.OperationDrain, Instance: "missing"})
	if _, err := missing.Commit(context.Background(), plan, "", controllerNow); !errors.Is(err, operations.ErrNotFound) {
		t.Fatalf("instance error = %v", err)
	}

	badStateStore := &fakeStore{instances: map[string]operations.Instance{"vm": {ID: "vm", State: operations.StateRunning}}}
	bad := Controller{Store: badStateStore, ControllerID: "c", Mode: Authority}
	if _, err := bad.Commit(context.Background(), readyPlan(scheduler.Operation{ID: "d", Kind: scheduler.OperationDrain, Instance: "vm"}), "", controllerNow); err == nil {
		t.Fatal("busy instance drain accepted")
	}
	if _, err := bad.Commit(context.Background(), readyPlan(scheduler.Operation{ID: "x", Kind: "unknown"}), "", controllerNow); err == nil {
		t.Fatal("unknown operation accepted")
	}
	spawn := scheduler.Operation{ID: "s", Kind: scheduler.OperationSpawn, Profile: "missing", Route: "tiered", Demand: domain.DemandKey{Repo: "o/r", RunID: 1, Attempt: 1, JobID: 1}}
	if _, err := bad.Commit(context.Background(), readyPlan(spawn), "", controllerNow); err == nil {
		t.Fatal("unknown spawn profile accepted")
	}
}
