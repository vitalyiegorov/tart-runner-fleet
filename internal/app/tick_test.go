package app

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/reconcile"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

type tickStore struct {
	fakeDemandStore
	state                           operations.SchedulerState
	instances                       map[string]operations.Instance
	plans                           []operations.Plan
	stateErr, instanceErr, applyErr error
	reseedErr                       error
	reseeded                        int
}

func (s *tickStore) SchedulerState(context.Context) (operations.SchedulerState, error) {
	return s.state, s.stateErr
}
func (s *tickStore) ReseedSchedulerState(context.Context) error {
	s.reseeded++
	if s.reseedErr != nil {
		return s.reseedErr
	}
	s.stateErr = nil
	return nil
}
func (s *tickStore) Instance(_ context.Context, id string) (operations.Instance, error) {
	if s.instanceErr != nil {
		return operations.Instance{}, s.instanceErr
	}
	v, ok := s.instances[id]
	if !ok {
		return operations.Instance{}, operations.ErrNotFound
	}
	return v, nil
}
func (s *tickStore) DrainGeneration(context.Context, string) (int, error) {
	return 0, nil
}

func (s *tickStore) SpawnGeneration(context.Context, domain.DemandKey) (int, error) {
	return 0, nil
}
func (s *tickStore) ApplyPlan(_ context.Context, plan operations.Plan) (bool, error) {
	if s.applyErr != nil {
		return false, s.applyErr
	}
	s.plans = append(s.plans, plan)
	return true, nil
}

type fakeInventory struct {
	instances domain.Observation[[]domain.Instance]
	host      domain.Observation[domain.Host]
}

type queueSummaryErrorStore struct{ *fakeDemandStore }

func (s queueSummaryErrorStore) QueuedGitHubJobs(context.Context, int64) ([]operations.GitHubJobObservation, error) {
	return nil, errors.New("REST inventory down")
}

func (f fakeInventory) Observe(context.Context) (domain.Observation[[]domain.Instance], domain.Observation[domain.Host]) {
	return f.instances, f.host
}

func tickConfig() scheduler.Config {
	return scheduler.Config{LinuxCapacity: domain.Resources{CPU: 2, MemoryMB: 4096, Slots: 1}, FairnessAge: 5 * time.Minute,
		RepoCaps: map[string]int{"owner/repo": 1}, Profiles: map[domain.ProfileID]domain.Profile{
			"small": {ID: "small", Route: "tiered", Platform: domain.PlatformLinux, Resources: domain.Resources{CPU: 2, MemoryMB: 4096, Slots: 1}},
		}}
}

func TestEngineTickPlansAndCommitsFreshSnapshot(t *testing.T) {
	now := time.Date(2026, 7, 12, 20, 0, 0, 0, time.UTC)
	prior, _ := json.Marshal(scheduler.State{DRRCursor: "other/repo"})
	store := &tickStore{state: operations.SchedulerState{Version: 2, Data: prior}, fakeDemandStore: fakeDemandStore{
		statistics: operations.DemandStatistics{MessageID: 1, Available: 1, ObservedAt: now}, records: []operations.DemandRecord{{
			Status: operations.DemandJobAvailable, RunnerRequestID: 10, Owner: "owner", Repository: "repo", WorkflowRunID: 8, QueueTime: now.Add(-time.Minute),
		}}}}
	binding := Binding{ScaleSetID: 1, Profile: tickConfig().Profiles["small"]}
	host := domain.Host{Available: tickConfig().LinuxCapacity, Pressure: domain.HostPressure{FreeDiskGB: 200, AdmissionAllowed: true}}
	engine := Engine{Store: store, Demand: DemandCoordinator{Store: store}, Inventory: fakeInventory{
		instances: domain.Fresh([]domain.Instance(nil), now), host: domain.Fresh(host, now),
	}, Config: tickConfig(), Bindings: []Binding{binding}, ControllerID: "controller", Mode: reconcile.Authority, Now: func() time.Time { return now }}
	result, err := engine.Tick(context.Background())
	if err != nil || !result.Applied || result.Plan.Status != scheduler.PlanReady || len(result.Plan.Operations) != 1 || len(store.plans) != 1 {
		t.Fatalf("Tick() = %#v, %v, durable=%d", result, err, len(store.plans))
	}
	if result.Host != host {
		t.Fatalf("Tick() host = %#v, want %#v", result.Host, host)
	}
	if result.Queues["small"].Count != 1 || result.Queues["small"].Oldest != now.Add(-time.Minute) {
		t.Fatalf("Tick() queues = %#v", result.Queues)
	}
}

func TestEngineTickReseedsMissingSchedulerStateAndRecovers(t *testing.T) {
	now := time.Date(2026, 7, 22, 20, 0, 0, 0, time.UTC)
	store := &tickStore{stateErr: operations.ErrSchedulerStateMissing}
	host := domain.Host{Available: tickConfig().LinuxCapacity, Pressure: domain.HostPressure{FreeDiskGB: 200, AdmissionAllowed: true}}
	engine := Engine{Store: store, Demand: DemandCoordinator{Store: store}, Inventory: fakeInventory{
		instances: domain.Fresh([]domain.Instance(nil), now), host: domain.Fresh(host, now),
	}, Config: tickConfig(), ControllerID: "controller", Mode: reconcile.Authority, Now: func() time.Time { return now }}
	result, err := engine.Tick(context.Background())
	if err != nil || store.reseeded != 1 {
		t.Fatalf("Tick() = %#v, %v, reseeded=%d", result, err, store.reseeded)
	}
}

func TestEngineTickPropagatesReseedFailure(t *testing.T) {
	now := time.Now().UTC()
	want := errors.New("reseed down")
	store := &tickStore{stateErr: operations.ErrSchedulerStateMissing, reseedErr: want}
	engine := Engine{Store: store, Demand: DemandCoordinator{Store: store}, Inventory: fakeInventory{
		instances: domain.Fresh([]domain.Instance(nil), now), host: domain.Fresh(domain.Host{Available: tickConfig().LinuxCapacity}, now),
	}, Config: tickConfig(), ControllerID: "controller", Mode: reconcile.Authority, Now: func() time.Time { return now }}
	if _, err := engine.Tick(context.Background()); !errors.Is(err, want) {
		t.Fatalf("Tick() reseed failure = %v", err)
	}
}

func TestEngineTickFailsClosedOnStaleInventory(t *testing.T) {
	now := time.Now().UTC()
	store := &tickStore{}
	engine := Engine{Store: store, Demand: DemandCoordinator{Store: store}, Inventory: fakeInventory{
		instances: domain.Fresh([]domain.Instance(nil), now), host: domain.Stale(domain.Host{}, now.Add(-time.Hour), "probe failed"),
	}, Config: tickConfig(), ControllerID: "controller", Mode: reconcile.Shadow, Now: func() time.Time { return now }}
	result, err := engine.Tick(context.Background())
	if err != nil || result.Applied || result.Plan.Status != scheduler.PlanBlockedObservation || len(store.plans) != 0 {
		t.Fatalf("Tick() = %#v, %v", result, err)
	}
}

func TestEngineTickValidationAndBoundaryErrors(t *testing.T) {
	now := time.Now().UTC()
	freshInventory := fakeInventory{instances: domain.Fresh([]domain.Instance(nil), now), host: domain.Fresh(domain.Host{Available: tickConfig().LinuxCapacity}, now)}
	want := errors.New("down")
	tests := []struct {
		name   string
		engine Engine
	}{
		{name: "nil store", engine: Engine{Inventory: freshInventory, Config: tickConfig(), ControllerID: "c", Mode: reconcile.Observe}},
		{name: "nil inventory", engine: Engine{Store: &tickStore{}, Config: tickConfig(), ControllerID: "c", Mode: reconcile.Observe}},
		{name: "bad controller", engine: Engine{Store: &tickStore{}, Inventory: freshInventory, Config: tickConfig(), Mode: reconcile.Observe}},
		{name: "bad mode", engine: Engine{Store: &tickStore{}, Inventory: freshInventory, Config: tickConfig(), ControllerID: "c", Mode: "bad"}},
		{name: "bad clock", engine: Engine{Store: &tickStore{}, Inventory: freshInventory, Config: tickConfig(), ControllerID: "c", Mode: reconcile.Observe, Now: func() time.Time { return time.Time{} }}},
		{name: "state error", engine: Engine{Store: &tickStore{stateErr: want}, Inventory: freshInventory, Config: tickConfig(), ControllerID: "c", Mode: reconcile.Observe}},
		{name: "corrupt state", engine: Engine{Store: &tickStore{state: operations.SchedulerState{Data: []byte("{")}}, Inventory: freshInventory, Config: tickConfig(), ControllerID: "c", Mode: reconcile.Observe}},
		{name: "demand error", engine: Engine{Store: &tickStore{fakeDemandStore: fakeDemandStore{err: want}}, Inventory: freshInventory, Config: tickConfig(), Bindings: []Binding{{ScaleSetID: 1, Profile: tickConfig().Profiles["small"]}}, ControllerID: "c", Mode: reconcile.Observe}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := tt.engine.Tick(context.Background()); err == nil {
				t.Fatal("Tick() unexpectedly succeeded")
			}
		})
	}
}

func TestEngineTickFailsClosedWhenCanonicalQueueSummaryIsUnavailable(t *testing.T) {
	now := time.Now().UTC()
	store := &tickStore{}
	queueStore := queueSummaryErrorStore{fakeDemandStore: &fakeDemandStore{}}
	engine := Engine{Store: store, Demand: DemandCoordinator{Store: queueStore}, Inventory: fakeInventory{
		instances: domain.Fresh([]domain.Instance(nil), now), host: domain.Fresh(domain.Host{Available: tickConfig().LinuxCapacity}, now),
	}, Config: tickConfig(), Bindings: []Binding{{ScaleSetID: 1, Profile: tickConfig().Profiles["small"]}}, ControllerID: "c", Mode: reconcile.Observe}
	if _, err := engine.Tick(context.Background()); err == nil {
		t.Fatal("tick accepted an unavailable canonical queue inventory")
	}
}

func TestEngineDefaultClockAndObserveMode(t *testing.T) {
	store := &tickStore{}
	engine := Engine{Store: store, Inventory: fakeInventory{
		instances: domain.Fresh([]domain.Instance(nil), time.Now()), host: domain.Fresh(domain.Host{Available: tickConfig().LinuxCapacity}, time.Now()),
	}, Config: tickConfig(), ControllerID: "c", Mode: reconcile.Observe}
	result, err := engine.Tick(context.Background())
	if err != nil || result.Applied || result.At.IsZero() {
		t.Fatalf("Tick() = %#v, %v", result, err)
	}
}

func TestEngineTickDegradesBindingWhenStatisticsAreStale(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	store := &tickStore{fakeDemandStore: fakeDemandStore{
		statistics: operations.DemandStatistics{MessageID: 1, Available: 1, ObservedAt: now.Add(-3 * time.Minute)},
		records: []operations.DemandRecord{{
			Status: operations.DemandJobAvailable, RunnerRequestID: 10, Owner: "owner", Repository: "repo", WorkflowRunID: 8, QueueTime: now.Add(-time.Minute),
		}}}}
	binding := Binding{ScaleSetID: 1, Profile: tickConfig().Profiles["small"]}
	host := domain.Host{Available: tickConfig().LinuxCapacity, Pressure: domain.HostPressure{FreeDiskGB: 200, AdmissionAllowed: true}}
	engine := Engine{Store: store, Demand: DemandCoordinator{Store: store}, Inventory: fakeInventory{
		instances: domain.Fresh([]domain.Instance(nil), now), host: domain.Fresh(host, now),
	}, Config: tickConfig(), Bindings: []Binding{binding}, ControllerID: "controller", Mode: reconcile.Authority, Now: func() time.Time { return now }}
	result, err := engine.Tick(context.Background())
	if err != nil || result.Plan.Status != scheduler.PlanReady {
		t.Fatalf("stale statistics failed the whole tick: %#v, %v", result, err)
	}
	if len(result.Demands) != 1 || len(result.Plan.Operations) != 1 {
		t.Fatalf("degraded binding must trickle exactly one demand: %#v", result)
	}
	if result.Queues["small"].Count != 1 || result.Queues["small"].Oldest != now.Add(-time.Minute) {
		t.Fatalf("degraded binding lost queue visibility: %#v", result.Queues)
	}
}

func TestEngineTickTricklesOldestDemandWhenStatisticsAreStale(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	store := &tickStore{fakeDemandStore: fakeDemandStore{
		statistics: operations.DemandStatistics{MessageID: 1, Available: 2, ObservedAt: now.Add(-3 * time.Minute)},
		records: []operations.DemandRecord{{
			Status: operations.DemandJobAvailable, RunnerRequestID: 11, Owner: "owner", Repository: "repo", WorkflowRunID: 8, QueueTime: now.Add(-2 * time.Minute),
		}, {
			Status: operations.DemandJobAvailable, RunnerRequestID: 12, Owner: "owner", Repository: "repo", WorkflowRunID: 8, QueueTime: now.Add(-time.Minute),
		}}}}
	binding := Binding{ScaleSetID: 1, Profile: tickConfig().Profiles["small"]}
	host := domain.Host{Available: tickConfig().LinuxCapacity, Pressure: domain.HostPressure{FreeDiskGB: 200, AdmissionAllowed: true}}
	engine := Engine{Store: store, Demand: DemandCoordinator{Store: store}, Inventory: fakeInventory{
		instances: domain.Fresh([]domain.Instance(nil), now), host: domain.Fresh(host, now),
	}, Config: tickConfig(), Bindings: []Binding{binding}, ControllerID: "controller", Mode: reconcile.Authority, Now: func() time.Time { return now }}
	result, err := engine.Tick(context.Background())
	if err != nil || len(result.Demands) != 1 || result.Demands[0].Key.JobID != 11 {
		t.Fatalf("expected only the oldest demand to trickle: %#v, %v", result.Demands, err)
	}
	if result.Queues["small"].Count != 2 {
		t.Fatalf("degraded binding lost queue visibility: %#v", result.Queues)
	}
}

// TestEngineTickTrickleSkipsDemandWithLiveIncarnation reproduces the incident:
// statistics are stale, so the trickle engages; the oldest demand already has
// a live (assigned) instance waiting on GitHub. Blindly trickling the oldest
// demand would respawn it and collide with the live incarnation, wedging every
// recycled tick. The trickle must instead admit the next demand — the oldest
// one without a live incarnation — and the tick must succeed.
func TestEngineTickTrickleSkipsDemandWithLiveIncarnation(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	store := &tickStore{fakeDemandStore: fakeDemandStore{
		statistics: operations.DemandStatistics{MessageID: 1, Available: 2, ObservedAt: now.Add(-3 * time.Minute)},
		records: []operations.DemandRecord{{
			Status: operations.DemandJobAvailable, RunnerRequestID: 11, Owner: "owner", Repository: "repo", WorkflowRunID: 8, QueueTime: now.Add(-2 * time.Minute),
		}, {
			Status: operations.DemandJobAvailable, RunnerRequestID: 12, Owner: "owner", Repository: "repo", WorkflowRunID: 8, QueueTime: now.Add(-time.Minute),
		}}}}
	cfg := scheduler.Config{LinuxCapacity: domain.Resources{CPU: 4, MemoryMB: 8192, Slots: 2}, FairnessAge: 5 * time.Minute,
		RepoCaps: map[string]int{"owner/repo": 2}, Profiles: map[domain.ProfileID]domain.Profile{
			"small": {ID: "small", Route: "tiered", Platform: domain.PlatformLinux, Resources: domain.Resources{CPU: 2, MemoryMB: 4096, Slots: 1}},
		}}
	live := domain.Instance{ID: "trf-small-live11", Repo: "owner/repo",
		Demand:   domain.DemandKey{Repo: "owner/repo", RunID: 8, Attempt: 1, JobID: 11},
		Platform: domain.PlatformLinux, Profile: "small", Route: "tiered",
		Resources: domain.Resources{CPU: 2, MemoryMB: 4096, Slots: 1},
		State:     domain.InstanceAssigned, Power: domain.InstancePowerRunning}
	binding := Binding{ScaleSetID: 1, Profile: cfg.Profiles["small"]}
	host := domain.Host{Available: cfg.LinuxCapacity, Pressure: domain.HostPressure{FreeDiskGB: 200, AdmissionAllowed: true}}
	engine := Engine{Store: store, Demand: DemandCoordinator{Store: store}, Inventory: fakeInventory{
		instances: domain.Fresh([]domain.Instance{live}, now), host: domain.Fresh(host, now),
	}, Config: cfg, Bindings: []Binding{binding}, ControllerID: "controller", Mode: reconcile.Authority, Now: func() time.Time { return now }}
	result, err := engine.Tick(context.Background())
	if err != nil {
		t.Fatalf("trickle must not wedge the tick when the oldest demand is live: %v", err)
	}
	if len(result.Demands) != 1 || result.Demands[0].Key.JobID != 12 {
		t.Fatalf("trickle must skip the live demand and admit the next: %#v", result.Demands)
	}
	spawns := 0
	for _, operation := range result.Plan.Operations {
		if operation.Kind != scheduler.OperationSpawn {
			continue
		}
		spawns++
		if operation.Demand.JobID == 11 {
			t.Fatalf("planned a duplicate spawn for a demand with a live incarnation: %#v", operation)
		}
		if operation.Demand.JobID != 12 {
			t.Fatalf("spawn targeted an unexpected demand: %#v", operation)
		}
	}
	if spawns != 1 {
		t.Fatalf("expected exactly one spawn for the skipped-forward demand: %#v", result.Plan.Operations)
	}
	if result.Queues["small"].Count != 2 {
		t.Fatalf("degraded binding lost queue visibility: %#v", result.Queues)
	}
}

// TestTrickleAdmitsTheOldestPlannableDemand pins the trickle selection helper
// directly. It now receives the plannable queue — plannableDemands has already
// dropped every demand a live instance incarnates — so its whole contract is
// "the oldest one, or nothing at all".
func TestTrickleAdmitsTheOldestPlannableDemand(t *testing.T) {
	keyA := domain.DemandKey{Repo: "owner/repo", RunID: 8, Attempt: 1, JobID: 11}
	keyB := domain.DemandKey{Repo: "owner/repo", RunID: 8, Attempt: 1, JobID: 12}
	a := domain.Demand{Key: keyA}
	b := domain.Demand{Key: keyB}
	if got := trickle(nil); got != nil {
		t.Fatalf("empty queue must trickle nothing: %#v", got)
	}
	if got := trickle([]domain.Demand{}); got != nil {
		t.Fatalf("a fully incarnated queue must trickle nothing: %#v", got)
	}
	if got := trickle([]domain.Demand{a, b}); len(got) != 1 || got[0].Key != keyA {
		t.Fatalf("trickle must admit the oldest plannable demand: %#v", got)
	}
	if got := trickle([]domain.Demand{b}); len(got) != 1 || got[0].Key != keyB {
		t.Fatalf("a skipped-forward queue must trickle its own head: %#v", got)
	}
}

// overdueTickStore lets the tick's overdue expiry act on the same records the
// tick is about to read, which is the ordering under test.
type overdueTickStore struct {
	*tickStore
	criteria operations.OverdueDemandCriteria
}

func (s *overdueTickStore) ExpireOverdueDemands(_ context.Context, _ int64, criteria operations.OverdueDemandCriteria) (int64, error) {
	s.criteria = criteria
	kept := s.records[:0]
	expired := int64(0)
	cutoff := criteria.Now.Add(-criteria.TTL)
	for _, record := range s.records {
		if record.Status == operations.DemandJobAvailable && !record.QueueTime.IsZero() && !record.QueueTime.After(cutoff) {
			expired++
			continue
		}
		kept = append(kept, record)
	}
	s.records = kept
	return expired, nil
}

// TestEngineTickRetiresOverdueDemandBeforeReadingIt is issue #315's wiring.
//
// The expiry has to run on the TICK, before demand is read: it is the one path
// that needs neither a session nor a REST observer, so a node returning from
// the outage that minted its dead rows sheds them on the first ticks instead of
// breaching its queue SLO until an operator arrives with raw SQL. A row GitHub
// has already failed must not spend one more tick as plannable work or as
// queue age.
func TestEngineTickRetiresOverdueDemandBeforeReadingIt(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	prior, _ := json.Marshal(scheduler.State{})
	store := &overdueTickStore{tickStore: &tickStore{state: operations.SchedulerState{Version: 2, Data: prior},
		fakeDemandStore: fakeDemandStore{
			statistics: operations.DemandStatistics{MessageID: 1, Available: 1, ObservedAt: now},
			records: []operations.DemandRecord{
				{Status: operations.DemandJobAvailable, RunnerRequestID: 10, Owner: "owner", Repository: "repo",
					WorkflowRunID: 8, QueueTime: now.Add(-49 * time.Hour)},
				{Status: operations.DemandJobAvailable, RunnerRequestID: 11, Owner: "owner", Repository: "repo",
					WorkflowRunID: 9, QueueTime: now.Add(-time.Minute)},
			}}}}
	binding := Binding{ScaleSetID: 1, Profile: tickConfig().Profiles["small"]}
	host := domain.Host{Available: tickConfig().LinuxCapacity, Pressure: domain.HostPressure{FreeDiskGB: 200, AdmissionAllowed: true}}
	engine := Engine{Store: store, Demand: DemandCoordinator{Store: store}, Inventory: fakeInventory{
		instances: domain.Fresh([]domain.Instance(nil), now), host: domain.Fresh(host, now),
	}, Config: tickConfig(), Bindings: []Binding{binding}, ControllerID: "controller", Mode: reconcile.Authority,
		Now: func() time.Time { return now }}

	result, err := engine.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.ExpiredOverdue != 1 {
		t.Fatalf("ExpiredOverdue = %d, want the one dead row", result.ExpiredOverdue)
	}
	if got := result.Queues["small"].Count; got != 1 {
		t.Fatalf("queue still counts the retired row: %d", got)
	}
	if oldest := result.Queues["small"].Oldest; oldest != now.Add(-time.Minute) {
		t.Fatalf("queue age is still the dead row's: %s", oldest)
	}
	if store.criteria.TTL != 48*time.Hour {
		t.Fatalf("TTL = %s, want double GitHub's 24-hour job-start bound", store.criteria.TTL)
	}
}

type overdueFailingTickStore struct{ *tickStore }

func (s *overdueFailingTickStore) ExpireOverdueDemands(context.Context, int64, operations.OverdueDemandCriteria) (int64, error) {
	return 0, errors.New("expiry write refused")
}

// A tick whose expiry write fails does not proceed to publish queue ages it
// knows may include dead rows; the failure classifies like any other unreadable
// demand.
func TestEngineTickFailsClosedWhenOverdueExpiryFails(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	prior, _ := json.Marshal(scheduler.State{})
	store := &overdueFailingTickStore{tickStore: &tickStore{state: operations.SchedulerState{Version: 2, Data: prior}}}
	host := domain.Host{Available: tickConfig().LinuxCapacity, Pressure: domain.HostPressure{FreeDiskGB: 200, AdmissionAllowed: true}}
	engine := Engine{Store: store, Demand: DemandCoordinator{Store: store}, Inventory: fakeInventory{
		instances: domain.Fresh([]domain.Instance(nil), now), host: domain.Fresh(host, now),
	}, Config: tickConfig(), Bindings: []Binding{{ScaleSetID: 1, Profile: tickConfig().Profiles["small"]}},
		ControllerID: "controller", Mode: reconcile.Authority, Now: func() time.Time { return now }}

	if _, err := engine.Tick(context.Background()); err == nil {
		t.Fatal("a failed expiry write must fail the tick closed")
	}
}
