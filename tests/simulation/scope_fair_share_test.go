package simulation_test

import (
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// ---------------------------------------------------------------------------
// Incident 2026-09-21/22 -- ADR 0057, one scope holding every slot
// ---------------------------------------------------------------------------

// scopeShareWorld is the arrangement the incident happened in, and the one the
// harness could not previously express: ONE macOS profile, two slots, and two
// GitHub scopes competing for them.
//
// Every earlier world draws each arrival's repository independently, so a
// batch -- one workflow run's matrix, every job of it in one scope, arriving
// together -- was never generated. That is the blind spot the incident hid in:
// a batch is older than anything another scope queues after it, and aged FIFO
// then hands the whole node back to the batch every time a slot frees. The
// trace below generates exactly that.
func scopeShareWorld() worldConfig {
	cfg := defaultWorld()
	cfg.Name = "two-scope-maestro-mac"
	profiles := map[domain.ProfileID]domain.Profile{"maestro": simProfiles()["maestro"]}
	cfg.Scheduler.Profiles = profiles
	cfg.Bindings = simBindings(profiles)
	cfg.Profiles = sortedProfileIDs(profiles)
	// Two scopes, neither of them the control plane: the question is fairness
	// between equals, and a declared class would answer it for other reasons.
	cfg.Repos = []string{"a/repo", "b/repo"}
	cfg.Scheduler.RepoSchedulingClasses = nil
	cfg.Scheduler.RepoCaps = map[string]int{"a/repo": 4, "b/repo": 4}
	return cfg
}

// scopeShareBatchTrace is the incident's shape: scope `a` arrives in batches of
// four long maestro jobs and refills them, scope `b` queues three jobs shortly
// after the first batch and then nothing.
func scopeShareBatchTrace(cfg worldConfig) simTrace {
	trace := simTrace{Seed: 20_260_922, Ticks: 200, Config: cfg.Name}
	for _, tick := range []int{1, 40, 80, 120} {
		for range 6 {
			trace.Events = append(trace.Events, simEvent{Tick: tick, Kind: eventArrive, Repo: "a/repo",
				Profile: "maestro", Event: "push"})
		}
		// The jobs are the incident's: half-hour Maestro runs, not the harness's
		// default couple of ticks. A short job hides the defect by accident.
		trace.Events = append(trace.Events, simEvent{Tick: tick, Kind: eventLongJob, Count: 6})
	}
	for range 3 {
		trace.Events = append(trace.Events, simEvent{Tick: 4, Kind: eventArrive, Repo: "b/repo",
			Profile: "maestro", Event: "push"})
	}
	trace.Events = append(trace.Events, simEvent{Tick: 150, Kind: eventStopArrivals})
	return trace
}

// TestAScopeHoldingEverySlotYieldsTheNextOne drives the 2026-09-21/22 incident
// through the simulator. Before ADR 0057 the waiting scope was passed over on
// every freed slot for as long as the batch kept refilling; property (s) is the
// bound, and the admission tick below is the measurement.
func TestAScopeHoldingEverySlotYieldsTheNextOne(t *testing.T) {
	t.Parallel()
	cfg := scopeShareWorld()
	w := newWorld(t, cfg, scopeShareBatchTrace(cfg))
	defer w.close()
	if findings := w.run(); len(findings) > 0 {
		t.Fatalf("the scope-starvation incident must no longer violate a property: %s", findings[0])
	}

	waiting, holding := firstAdmissionPerScope(w)
	if waiting == 0 {
		t.Fatal("the harness never admitted the waiting scope at all: the incident is not being generated")
	}
	if holding == 0 {
		t.Fatal("the harness never admitted the streaming scope: the competition is not being generated")
	}
	// Its three jobs arrived at tick 4, one tick after the batch took both slots.
	// One long job plus the teardown walk is the honest wait; the incident's was
	// the whole batch, and then the next batch.
	if waiting > 30 {
		t.Fatalf("the waiting scope was first admitted at tick %d, behind the streaming scope's batch "+
			"(first admitted at tick %d); ADR 0057 bounds that wait by one job, not by one backlog", waiting, holding)
	}
}

// firstAdmissionPerScope reports the first tick each of the two scopes had a
// demand admitted.
func firstAdmissionPerScope(w *world) (waiting, holding int) {
	for _, observation := range w.observations {
		for _, key := range observation.spawns() {
			switch simScopeOf(key.Repo) {
			case "b":
				if waiting == 0 {
					waiting = observation.Tick
				}
			case "a":
				if holding == 0 {
					holding = observation.Tick
				}
			}
		}
	}
	return waiting, holding
}

// ---------------------------------------------------------------------------
// Incident 2026-09-22 -- issue #350, the teardown a cap has released and a core
// has not
// ---------------------------------------------------------------------------

// wedgedTeardownScopeShareTrace is the shape no earlier trace could make: the
// streaming scope holds BOTH slots, both jobs end together, and one of its two
// guests is held in `deregistering` by a guest that will not power down (issue
// #233) while the other completes and hands its slot back.
//
// On that tick the streaming scope is unambiguously on one of this node's two
// cores -- `ConsumesHostResources` says so, and the operator sees the VM -- and
// its repository cap slot is already released, because ADR 0043 releases the cap
// early on purpose. Reading occupancy from the cap therefore reports the scope
// as holding NOTHING, and the freed slot goes back to it over a scope holding
// nothing for real. That is issue #350.
//
// The harness could not reach the tick before this trace: every scope-fairness
// world admitted a scope's guests together, so its guests walked the teardown in
// lockstep and were never in two different states at once, and no trace ever
// wedged a teardown in a world where fairness between scopes was the question.
func wedgedTeardownScopeShareTrace(cfg worldConfig) simTrace {
	trace := simTrace{Seed: 20_260_923, Ticks: 200, Config: cfg.Name}
	for _, tick := range []int{1, 40, 80, 120} {
		// Four at once: two fill the node's two slots and two stay queued, so the
		// scope still has work of its own -- OLDER than anything the other scope
		// queues afterwards, which is the whole difficulty -- when those two tear
		// down. The tick under test is a contested one, or it measures nothing.
		for range 4 {
			trace.Events = append(trace.Events, simEvent{Tick: tick, Kind: eventArrive, Repo: "a/repo",
				Profile: "maestro", Event: "push"})
		}
		// Half-hour Maestro jobs, as the incident's were. A short job hides this by
		// accident: the teardown window is then a smaller share of the run.
		trace.Events = append(trace.Events, simEvent{Tick: tick, Kind: eventLongJob, Count: 2})
		// Both guests start together and therefore end together; this holds one of
		// them in `deregistering` while the other releases its slot.
		trace.Events = append(trace.Events, simEvent{Tick: tick + 13, Kind: eventUnstoppableGuest, Count: 1})
	}
	for range 3 {
		trace.Events = append(trace.Events, simEvent{Tick: 4, Kind: eventArrive, Repo: "b/repo",
			Profile: "maestro", Event: "push"})
	}
	trace.Events = append(trace.Events, simEvent{Tick: 150, Kind: eventStopArrivals})
	return trace
}

// TestAScopeStillOnACoreDoesNotReadAsHoldingNothing drives issue #350 through
// the simulator and measures the decision directly, tick by tick.
//
// On every tick that admits work while exactly one scope is on a core and the
// other is on none, the slot must go to the scope on none -- whatever lifecycle
// state the occupying scope's guest is in, because the question ADR 0057 asks is
// how much of this node a scope holds and a guest in `deregistering` is holding
// a whole core.
func TestAScopeStillOnACoreDoesNotReadAsHoldingNothing(t *testing.T) {
	t.Parallel()
	cfg := scopeShareWorld()
	w := newWorld(t, cfg, wedgedTeardownScopeShareTrace(cfg))
	defer w.close()
	if findings := w.run(); len(findings) > 0 {
		t.Fatalf("a scope visibly holding a core was read as holding nothing: %s", findings[0])
	}

	contested := 0
	for _, observation := range w.observations {
		spawns := observation.spawns()
		if len(spawns) == 0 {
			continue
		}
		onACore := map[string]int{}
		for _, instance := range observation.Instances {
			if instance.ConsumesHostResources() {
				onACore[simScopeOf(instance.Repo)]++
			}
		}
		queued := map[string]int{}
		for _, demand := range observation.Demands {
			if aged(cfg, observation.Now, demand) {
				queued[simScopeOf(demand.Key.Repo)]++
			}
		}
		// Exactly the arrangement the rule is about: one scope on this node, one
		// scope on none, and both asking for the same profile's next slot.
		if len(onACore) != 1 || queued["a"] == 0 || queued["b"] == 0 {
			continue
		}
		holder := ""
		for scope := range onACore {
			holder = scope
		}
		contested++
		for _, key := range spawns {
			if simScopeOf(key.Repo) == holder {
				t.Fatalf("tick %d: scope %q was on a core and took the freed slot anyway, over a scope on none "+
					"with %d aged demands queued\n%s", observation.Tick, holder, queued[map[string]string{"a": "b", "b": "a"}[holder]],
					w.dumpPlan(observation))
			}
		}
	}
	if contested == 0 {
		t.Fatal("no contested admission was generated: the trace measures nothing")
	}
	t.Logf("contested admissions judged: %d", contested)
}
