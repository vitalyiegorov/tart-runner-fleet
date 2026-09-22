package scheduler

import (
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// The 2026-09-21/22 incident on both Macs: one scope streams 30-60 minute
// Maestro jobs in batches, the other asks for a single 20 minute job, and the
// aged band -- pure FIFO by creation time -- hands every freed slot back to the
// scope that already holds them, because a batch that arrived first is older
// than everything the other scope will ever queue.
//
// ADR 0057 states the missing key: inside the aged band, a scope that already
// holds instances on this node yields the next slot to a scope that holds none.

func fairShareConfig() Config {
	config := testConfig()
	config.RepoCaps = map[string]int{"budgie-org/budgie": 4, "pony-org/pony": 4}
	return config
}

func maestroDemand(repo string, jobID int64, age time.Duration) domain.Demand {
	return domain.Demand{
		Key:       domain.DemandKey{Repo: repo, RunID: 100, Attempt: 1, JobID: jobID},
		CreatedAt: testNow.Add(-age),
		Profile:   "maestro",
		Route:     "macos-maestro",
		Platform:  domain.PlatformMacOS,
		Event:     domain.EventPullRequest,
	}
}

func maestroInstance(id, repo string, state domain.InstanceState) domain.Instance {
	return domain.Instance{
		ID: id, Repo: repo, Platform: domain.PlatformMacOS, Profile: "maestro", Route: "macos-maestro",
		Resources: domain.Resources{CPU: 4, MemoryMB: 7_168, Slots: 1},
		State:     state, Power: domain.InstancePowerRunning, RunningSince: testNow.Add(-time.Hour),
	}
}

func fairShareInput(demands []domain.Demand, instances []domain.Instance) Input {
	return Input{
		Now:       testNow,
		Config:    fairShareConfig(),
		Demands:   domain.Fresh(demands, testNow),
		Instances: domain.Fresh(instances, testNow),
		Host:      domain.Fresh(domain.Host{Available: domain.Resources{CPU: 8, MemoryMB: 16_384, Slots: 4}}, testNow),
	}
}

// TestFreedSlotGoesToTheScopeHoldingNone is the incident in one tick: the
// occupying scope's queued batch is older than the waiting scope's job, and one
// slot has just been freed.
func TestFreedSlotGoesToTheScopeHoldingNone(t *testing.T) {
	demands := []domain.Demand{
		maestroDemand("budgie-org/budgie", 1, 90*time.Minute),
		maestroDemand("pony-org/pony", 2, 40*time.Minute),
	}
	instances := []domain.Instance{maestroInstance("budgie-1", "budgie-org/budgie", domain.InstanceRunning)}
	plan := PlanTick(fairShareInput(demands, instances))
	keys := spawnedKeys(plan)
	if len(keys) != 1 || keys[0].Repo != "pony-org/pony" {
		t.Fatalf("freed slot went to %#v, want the scope holding no instances", keys)
	}
}

// TestFairShareDoesNotReorderWithinAScope keeps FIFO exactly where it still
// rules: two demands of one scope stay in age order.
func TestFairShareDoesNotReorderWithinAScope(t *testing.T) {
	demands := []domain.Demand{
		maestroDemand("budgie-org/budgie", 1, 90*time.Minute),
		maestroDemand("budgie-org/budgie", 2, 40*time.Minute),
	}
	plan := PlanTick(fairShareInput(demands, nil))
	keys := spawnedKeys(plan)
	if len(keys) != 2 || keys[0].JobID != 1 || keys[1].JobID != 2 {
		t.Fatalf("within-scope order = %#v, want age order", keys)
	}
}

// TestScopeFairShareBoundsTheWaitBehindAStreamingScope replays the shape of the
// trace over many ticks: scope A arrives in batches of six long jobs and keeps
// refilling, scope B queues three jobs shortly after the first batch. Under pure
// FIFO B waits for A's whole backlog; under ADR 0057 it waits at most one job
// length behind the slot that frees next.
func TestScopeFairShareBoundsTheWaitBehindAStreamingScope(t *testing.T) {
	world := newFairShareWorld(fairShareConfig(), 12)
	first := domain.DemandKey{}
	spawned := map[domain.DemandKey]int{}
	for tick := range 200 {
		if tick%40 == 0 {
			for range 6 {
				world.enqueue("budgie-org/budgie")
			}
		}
		if tick == 5 {
			for index := range 3 {
				key := world.enqueue("pony-org/pony")
				if index == 0 {
					first = key
				}
			}
		}
		for _, key := range world.step() {
			if _, seen := spawned[key]; !seen {
				spawned[key] = tick
			}
		}
	}
	admitted, ok := spawned[first]
	if !ok {
		t.Fatalf("the waiting scope was never admitted in 200 ticks")
	}
	// One job length (12 ticks) of wait is the honest bound: the incumbent job
	// holding the slot has to finish, and the teardown walk has to release it.
	if wait := admitted - 5; wait > 18 {
		t.Fatalf("the waiting scope waited %d ticks behind the streaming scope, want <= 18", wait)
	}
}

// fairShareWorld is the smallest faithful multi-tick world for this question: a
// two-slot Mac, one maestro profile, jobs of a fixed length, and the same
// lifecycle walk the package's simulation harness performs. It is deliberately
// deterministic -- no randomness -- because the property under test is an
// ordering rule, and an ordering rule that holds only on average is no rule.
type fairShareWorld struct {
	now       time.Time
	config    Config
	instances []domain.Instance
	pending   []domain.Demand
	state     State
	jobTicks  map[string]int
	jobLength int
	jobID     int64
}

func newFairShareWorld(config Config, jobLength int) *fairShareWorld {
	return &fairShareWorld{now: testNow, config: config, jobTicks: map[string]int{}, jobLength: jobLength}
}

func (w *fairShareWorld) enqueue(repo string) domain.DemandKey {
	w.jobID++
	demand := domain.Demand{
		Key:       domain.DemandKey{Repo: repo, RunID: 100, Attempt: 1, JobID: w.jobID},
		CreatedAt: w.now, Profile: "maestro", Route: "macos-maestro",
		Platform: domain.PlatformMacOS, Event: domain.EventPullRequest,
	}
	w.pending = append(w.pending, demand)
	return demand.Key
}

// step plans one tick, applies it, advances every instance one legal lifecycle
// step, and returns the demands this tick admitted.
func (w *fairShareWorld) step() []domain.DemandKey {
	plan := PlanTick(Input{
		Now: w.now, Config: w.config,
		Demands:   domain.Fresh(append([]domain.Demand(nil), w.pending...), w.now),
		Instances: domain.Fresh(append([]domain.Instance(nil), w.instances...), w.now),
		Host:      domain.Fresh(domain.Host{Available: domain.Resources{CPU: 8, MemoryMB: 16_384, Slots: 4}}, w.now),
		Prior:     w.state,
	})
	var admitted []domain.DemandKey
	drained := map[string]bool{}
	for _, operation := range plan.Operations {
		switch operation.Kind {
		case OperationSpawn:
			w.spawn(operation)
			admitted = append(admitted, operation.Demand)
		case OperationDrain:
			drained[operation.Instance] = true
		}
	}
	var next []domain.Instance
	for _, instance := range w.instances {
		if advanced, keep := w.advance(instance, drained[instance.ID]); keep {
			next = append(next, advanced)
		}
	}
	w.instances = next
	w.state = plan.Next
	w.now = w.now.Add(time.Minute)
	return admitted
}

func (w *fairShareWorld) spawn(operation Operation) {
	profile := w.config.Profiles[operation.Profile]
	w.instances = append(w.instances, domain.Instance{
		ID: "vm-" + operation.Demand.String(), Repo: operation.Demand.Repo, Demand: operation.Demand,
		Platform: profile.Platform, Profile: profile.ID, Route: profile.Route, Resources: profile.Resources,
		State: domain.InstancePlanned, Power: domain.InstancePowerRunning,
	})
	var keep []domain.Demand
	for _, demand := range w.pending {
		if demand.Key != operation.Demand {
			keep = append(keep, demand)
		}
	}
	w.pending = keep
}

func (w *fairShareWorld) advance(instance domain.Instance, drained bool) (domain.Instance, bool) {
	if drained && instance.State.CanTransitionTo(domain.InstanceDraining) {
		instance.State = domain.InstanceDraining
		return instance, true
	}
	switch instance.State {
	case domain.InstancePlanned:
		instance.State = domain.InstanceCloning
	case domain.InstanceCloning:
		instance.State = domain.InstanceBooting
	case domain.InstanceBooting:
		instance.State = domain.InstanceReachable
	case domain.InstanceReachable:
		instance.State = domain.InstanceRegistering
	case domain.InstanceRegistering:
		instance.State = domain.InstanceAssigned
		instance.AssignedSince = w.now
	case domain.InstanceAssigned:
		instance.State = domain.InstanceRunning
		instance.RunningSince = w.now
		w.jobTicks[instance.ID] = w.jobLength
	case domain.InstanceRunning:
		if w.jobTicks[instance.ID] > 0 {
			w.jobTicks[instance.ID]--
			return instance, true
		}
		instance.State = domain.InstanceDraining
	case domain.InstanceDraining:
		instance.State = domain.InstanceDeregistering
	case domain.InstanceDeregistering:
		instance.State = domain.InstanceStopping
	case domain.InstanceStopping:
		delete(w.jobTicks, instance.ID)
		return instance, false
	}
	return instance, true
}
