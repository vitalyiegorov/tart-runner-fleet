package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

type emptyExitStoreFake struct {
	*tickStore
	exits map[domain.DemandKey]domain.EmptyExits
	err   error
}

func (s emptyExitStoreFake) EmptyExits(context.Context, time.Time) (map[domain.DemandKey]domain.EmptyExits, error) {
	return s.exits, s.err
}

func withheldEngine(now time.Time, exits map[domain.DemandKey]domain.EmptyExits, err error) Engine {
	engine := liveIncarnationEngine(now, nil)
	store := emptyExitStoreFake{tickStore: engine.Store.(*tickStore), exits: exits, err: err}
	engine.Store, engine.Demand.Store = store, store
	return engine
}

func TestAnEmptyExitHoldWithholdsTheDemandAndKeepsItNamed(t *testing.T) {
	now := time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)
	exits := map[domain.DemandKey]domain.EmptyExits{liveIncarnationKey: {Count: 3, Last: now.Add(-time.Minute)}}
	result, err := withheldEngine(now, exits, nil).Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Plan.Operations) != 0 || len(result.Demands) != 0 {
		t.Fatalf("a held demand was planned: %+v", result.Plan.Operations)
	}
	if len(result.EmptyExitBackoffs) != 1 || result.EmptyExitBackoffs[0].Exits != 3 {
		t.Fatalf("the hold must be named: %+v", result.EmptyExitBackoffs)
	}

	exits[liveIncarnationKey] = domain.EmptyExits{Count: 3, Last: now.Add(-5 * time.Minute)}
	result, err = withheldEngine(now, exits, nil).Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Plan.Operations) != 1 || len(result.EmptyExitBackoffs) != 1 {
		t.Fatalf("after the hold it is retried and still named: %+v %+v", result.Plan.Operations, result.EmptyExitBackoffs)
	}
}

func TestAnUnreadableEmptyExitCountFailsTheTick(t *testing.T) {
	now := time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)
	if _, err := withheldEngine(now, nil, errors.New("boom")).Tick(context.Background()); err == nil {
		t.Fatal("a count that could not be read must not read as no exits")
	}
}

func TestARefusedImageWithholdsItsWorkFromTheTick(t *testing.T) {
	now := time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)
	engine := liveIncarnationEngine(now, nil)
	engine.Refusals = &RunnerRefusals{Now: func() time.Time { return now }}
	engine.Refusals.Record(domain.PlatformLinux, "2.336.0", "trf-a")
	result, err := engine.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Plan.Operations) != 0 || result.RunnerRefusals[domain.PlatformLinux].Version != "2.336.0" {
		t.Fatalf("got %+v %+v", result.Plan.Operations, result.RunnerRefusals)
	}
	engine.Now = func() time.Time { return now.Add(domain.RunnerRefusalProbeInterval) }
	result, err = engine.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Plan.Operations) != 1 {
		t.Fatalf("the probe must launch: %+v", result.Plan.Operations)
	}
}

func TestARefusalOnTheWallClockIsDated(t *testing.T) {
	refusals := &RunnerRefusals{}
	refusals.Record(domain.PlatformLinux, "2.336.0", "trf-a")
	if refusals.Observe(nil)[domain.PlatformLinux].At.IsZero() {
		t.Fatal("a nil clock is the wall clock")
	}
}

// refusedEngine is a node with a standing verdict whose probe interval has passed.
func refusedEngine(now time.Time, instances []domain.Instance) Engine {
	engine := liveIncarnationEngine(now, instances)
	at := now.Add(-domain.RunnerRefusalProbeInterval)
	engine.Refusals = &RunnerRefusals{Now: func() time.Time { return at }}
	engine.Refusals.Record(domain.PlatformLinux, "2.336.0", "trf-a")
	engine.Refusals.Record(domain.PlatformLinux, "2.336.0", "trf-b")
	return engine
}

// A guest being torn down can never answer the question, and a stuck cleanup or a
// parked dead letter can stay that way indefinitely, so it may not stand in for a
// probe.
func TestAGuestInTeardownIsNotAProbe(t *testing.T) {
	now := time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)
	other := domain.DemandKey{Repo: "owner/repo", RunID: 99, Attempt: 1, JobID: 99}
	for _, state := range []domain.InstanceState{domain.InstanceDraining, domain.InstanceDeregistering, domain.InstanceStopping} {
		t.Run(string(state), func(t *testing.T) {
			stuck := incarnation(state)
			stuck.Demand = other
			result, err := refusedEngine(now, []domain.Instance{stuck}).Tick(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Plan.Operations) != 1 {
				t.Fatalf("a stuck cleanup blocked the probe: %+v", result.Plan.Operations)
			}
		})
	}
	waiting := incarnation(domain.InstanceAssigned)
	waiting.Demand = other
	result, err := refusedEngine(now, []domain.Instance{waiting}).Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Plan.Operations) != 0 {
		t.Fatalf("a guest already answering needs no second probe: %+v", result.Plan.Operations)
	}
}

// One guest's word cannot stop a node: a verdict takes two distinct guests, and the
// same guest twice, or two readings an hour and more apart, is still one witness.
func TestOneGuestsRefusalIsNotAVerdict(t *testing.T) {
	now := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	clock := now
	refusals := &RunnerRefusals{Now: func() time.Time { return clock }}
	refusals.Record(domain.PlatformLinux, "2.336.0", "trf-a")
	if standing := refusals.Observe(nil); len(standing) != 0 {
		t.Fatalf("one guest stopped the node: %+v", standing)
	}
	refusals.Record(domain.PlatformLinux, "2.336.0", "trf-a")
	if standing := refusals.Observe(nil); len(standing) != 0 {
		t.Fatalf("one guest counted twice: %+v", standing)
	}
	refusals.Record(domain.PlatformLinux, "2.336.0", "trf-b")
	if standing := refusals.Observe(nil); len(standing) != 1 {
		t.Fatalf("two guests agreeing is a verdict: %+v", standing)
	}
}

// probeEngine is a refused node with a `big` profile wider than its Linux cap will
// ever allow, whose binding comes first, and a `small` one that fits.
func probeEngine(now time.Time, smalls int) Engine {
	engine := refusedEngine(now, nil)
	small := engine.Config.Profiles["small"]
	big := domain.Profile{ID: "big", Route: "big", Platform: domain.PlatformLinux,
		Resources: domain.Resources{CPU: 64, MemoryMB: 4096, Slots: 1}}
	engine.Config.Profiles = map[domain.ProfileID]domain.Profile{"small": small, "big": big}
	engine.Config.RepoCaps = map[string]int{"owner/big": 2, "owner/small": 4}
	engine.Bindings = []Binding{
		{ScaleSetID: 1, Profile: big, Targets: []string{"owner/big"}},
		{ScaleSetID: 2, Profile: small, Targets: []string{"owner/small"}},
	}
	store := engine.Store.(*tickStore)
	store.statistics.Available = 8
	store.records = []operations.DemandRecord{{Status: operations.DemandJobAvailable, RunnerRequestID: 1,
		Owner: "owner", Repository: "big", WorkflowRunID: 1, QueueTime: now.Add(-time.Hour)}}
	for index := range smalls {
		store.records = append(store.records, operations.DemandRecord{Status: operations.DemandJobAvailable,
			RunnerRequestID: int64(10 + index), Owner: "owner", Repository: "small", WorkflowRunID: 2,
			QueueTime: now.Add(-time.Minute)})
	}
	return engine
}

func spawnKeys(plan scheduler.Plan) []domain.DemandKey {
	var keys []domain.DemandKey
	for _, operation := range plan.Operations {
		if operation.Kind == scheduler.OperationSpawn {
			keys = append(keys, operation.Demand)
		}
	}
	return keys
}

// The probe belongs to work the scheduler can start. An unschedulable demand met
// first must not hold the allowance, and however many fit, only one launches.
func TestAProbeGoesToWorkThatCanStartAndOnlyOneLaunches(t *testing.T) {
	now := time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)
	result, err := probeEngine(now, 3).Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	spawned := spawnKeys(result.Plan)
	if len(spawned) != 1 || spawned[0].Repo != "owner/small" {
		t.Fatalf("one probe, of work that fits: %+v", spawned)
	}
	if len(result.Demands) != 1 || result.Demands[0].Key != spawned[0] {
		t.Fatalf("the plan's queue must be what it planned: %+v", result.Demands)
	}
}

// Nothing is spent until a spawn is selected: a tick that could start nothing leaves
// the allowance for the next one.
func TestAProbeThatCannotStartIsNotSpent(t *testing.T) {
	now := time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)
	engine := probeEngine(now, 1)
	engine.Inventory = fakeInventory{instances: domain.Fresh[[]domain.Instance](nil, now),
		host: domain.Fresh(domain.Host{Pressure: domain.HostPressure{FreeDiskGB: 200}}, now)}
	result, err := engine.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if spawned := spawnKeys(result.Plan); len(spawned) != 0 {
		t.Fatalf("a node refusing admission launched: %+v", spawned)
	}
	engine.Inventory = probeEngine(now, 1).Inventory
	result, err = engine.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if spawned := spawnKeys(result.Plan); len(spawned) != 1 {
		t.Fatalf("the allowance was spent on a tick that started nothing: %+v", spawned)
	}
}
