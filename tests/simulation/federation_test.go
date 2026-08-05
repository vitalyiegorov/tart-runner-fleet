package simulation_test

import (
	"fmt"
	"testing"
	"time"
)

// This file is issue #153's simulated statement, and it is the only place in the
// harness where one scope has TWO scale sets.
//
// ADR 0034 as amended lets two nodes advertise one label in one scope, because
// the #144 spike proved GitHub places the work itself and fills each set to the
// capacity it last advertised. What GitHub does not do is tell either node about
// the other. The REST inventory lane polls the SCOPE, so both sets see the whole
// queue and both match every job in it by label -- and before the bound of
// issue #153, both would have recorded all of it. Two nodes would then report
// one queue twice, and a demand GitHub gave one of them would be corroborated
// forever on the other, which is the ADR 0026 ghost that spawns a guest for work
// that will never arrive.
//
// Two invariants are checked on every tick of every seed:
//
//	(1) the scope's committed observation is shared, not multiplied -- the queued
//	    jobs the two sets attribute to themselves never exceed the number of jobs
//	    the observation actually contained;
//	(2) one workflow job is incarnated by at most one live guest, whichever set
//	    GitHub gave it to.
//
// Before the fix this world was not merely wrong, it was unreachable: a job
// matching two bindings failed the whole scope observation with ErrConflict, so
// every REST poll of a federated scope recorded a store-error finding and the
// inventory lane -- a precondition of the topology -- never committed anything.

// federationSeeds is how many histories the federated scope is swept over. The
// arrival process is bursty and the fault mix is the same one the other worlds
// run, so a few dozen seeds reach saturation, statistics gaps, REST lag, and
// silent cancellation inside a shared scope.
const federationSeeds = 24

// TestFederatedScopeSharesOneQueueInsteadOfCountingItTwice sweeps the shared
// scope, one parallel subtest per seed.
//
// The seeds are independent by construction -- ADR 0031 makes a run a pure
// function of (trace, config), and every world owns its own `:memory:` store --
// so running them concurrently is a scheduling decision and nothing else. It is
// worth stating why it is made: swept serially this was 4800 ticks in one
// goroutine, which under the race detector measured 389s of the package's 392s
// on 2026-08-05 and was the single reason `Nightly resilience` reached the go
// test timeout and stopped reporting the four suites that run beside it
// (issue #208). The three fuzz arms were already parallel for the same reason.
func TestFederatedScopeSharesOneQueueInsteadOfCountingItTwice(t *testing.T) {
	t.Parallel()
	cfg := federatedWorld()
	if len(cfg.Bindings) != 2 || cfg.Bindings[0].Scope != cfg.Bindings[1].Scope {
		t.Fatalf("the federated world must be two scale sets in one scope: %#v", cfg.Bindings)
	}
	// One slot per seed, written only by that seed's own subtest, so the sweep
	// stays a sum over independent runs rather than shared mutable state.
	contested := make([]int, federationSeeds)
	t.Run("seeds", func(t *testing.T) {
		for offset := range federationSeeds {
			seed := int64(1 + offset)
			t.Run(fmt.Sprintf("seed-%03d", seed), func(t *testing.T) {
				t.Parallel()
				world := newWorld(t, cfg, generateTrace(seed, 200, cfg))
				defer world.close()
				contested[offset] = world.runFederated(t, seed)
			})
		}
	})
	total := 0
	for _, count := range contested {
		total += count
	}
	// The scenario has to have HAPPENED. A scope whose observations never held
	// more than one job could not double-count anything, and would pass both
	// invariants without exercising the bound at all.
	if total == 0 {
		t.Fatal("no scope observation ever carried work two sets could both claim")
	}
	t.Logf("federated scope: %d observations carried contested work across %d seeds", total, federationSeeds)
}

// runFederated executes one history, asserting both invariants after every tick,
// and reports how many committed observations carried work both sets could have
// claimed.
func (w *world) runFederated(t *testing.T, seed int64) int {
	t.Helper()
	contested := 0
	for w.tick = 1; w.tick <= w.trace.Ticks; w.tick++ {
		w.now = simEpoch.Add(time.Duration(w.tick) * simTick)
		w.applyTraceEvents()
		w.advancePhysics()
		w.produceMessages()
		w.deliverMessages()
		w.deliverSnapshots()
		observation := w.reconcile()
		w.executeOperations()
		w.observations = append(w.observations, observation)
		w.recordOutstandingWork()
		// An observation whose jobs, claimed by both sets, would have exceeded the
		// work the scope actually owed. This is the double count itself, and it
		// has to be reached or the invariants below prove nothing.
		if w.restCommitted != nil && 2*len(w.restCommitted.jobs) > w.restCommitted.outstanding &&
			len(w.restCommitted.jobs) > 0 {
			contested++
		}
		w.assertQueueIsSharedNotMultiplied(t, seed)
		w.assertOneGuestPerWorkflowJob(t, seed, observation)
		if w.check(observation) {
			t.Fatalf("seed %d: property violation in a federated scope: %v\n%s", seed, w.findings, w.trace)
		}
	}
	return contested
}

// assertQueueIsSharedNotMultiplied is invariant (1). The durable attribution of
// each scale set is read back from the real store, so this measures what the
// fleet committed rather than what the coordinator returned.
func (w *world) assertQueueIsSharedNotMultiplied(t *testing.T, seed int64) {
	t.Helper()
	claimed := 0
	for _, binding := range w.cfg.Bindings {
		jobs, err := w.store.QueuedGitHubJobs(w.ctx, binding.StoreKey)
		if err != nil {
			t.Fatalf("seed %d tick %d: read attributed jobs: %v", seed, w.tick, err)
		}
		claimed += len(jobs)
	}
	if w.restCommitted == nil {
		return
	}
	// The two lanes read the scope at two different instants -- the observation
	// when it was captured, the statistics when the broker last produced them --
	// so the conservation bound is the most work the scope owed anywhere across
	// the window their evidence can describe, not at either endpoint alone.
	ceiling := w.peakOutstandingWork()
	if claimed > ceiling {
		t.Fatalf("seed %d tick %d: the scale sets of one scope claim %d queued jobs from an observation of %d, "+
			"when the scope has owed at most %d jobs in total across the evidence window\n%s",
			seed, w.tick, claimed, len(w.restCommitted.jobs), ceiling, w.trace)
	}
}

// simEvidenceWindow is the oldest instant either lane's evidence can describe:
// four ticks of statistics freshness (DemandCoordinator.StatisticsMaxAge over
// simTick), up to six ticks of broker delivery delay, and up to six ticks of
// REST lag. A conservation bound narrower than this would be measuring the
// harness's fault injector rather than the fleet.
const simEvidenceWindow = 16

// recordOutstandingWork appends what the scope owes at this tick: every job
// GitHub has not finished, whichever set it belongs to.
func (w *world) recordOutstandingWork() {
	owed := 0
	for _, job := range w.jobs {
		switch job.status {
		case jobQueued, jobAcquired, jobRunning:
			owed++
		case jobDone, jobCancelled:
		}
	}
	w.owedHistory = append(w.owedHistory, owed)
}

// peakOutstandingWork is the most work the scope has owed at any tick the
// fleet's current evidence could have been taken at.
func (w *world) peakOutstandingWork() int {
	history := w.owedHistory
	if len(history) > simEvidenceWindow {
		history = history[len(history)-simEvidenceWindow:]
	}
	peak := 0
	for _, owed := range history {
		peak = max(peak, owed)
	}
	return peak
}

// assertOneGuestPerWorkflowJob is invariant (2), and it is deliberately keyed on
// the WORKFLOW JOB rather than on the demand: two sets in one scope hold two
// separate durable demand tables, so a duplicate spawn across them would be
// invisible to any per-set uniqueness check.
func (w *world) assertOneGuestPerWorkflowJob(t *testing.T, seed int64, observation tickObservation) {
	t.Helper()
	incarnated := map[int64]string{}
	for _, instance := range observation.Instances {
		if !instance.IncarnatesDemand() {
			continue
		}
		job := instance.Demand.JobID
		if previous, duplicate := incarnated[job]; duplicate {
			t.Fatalf("seed %d tick %d: workflow job %d incarnated by guests %s and %s\n%s",
				seed, w.tick, job, previous, instance.ID, w.trace)
		}
		incarnated[job] = instance.ID
	}
}

// TestFederatedScopeAttributesEachSetOnlyItsOwnAssignedWork is the smallest
// complete statement of the bound, with no seeded trace at all: one scope, two
// identically-labelled sets, four queued jobs, and GitHub having assigned one to
// the first set and three to the second -- the exact split of run 3 of the #144
// spike, where a set advertising 1 received 1 and a set advertising 5 received 3.
func TestFederatedScopeAttributesEachSetOnlyItsOwnAssignedWork(t *testing.T) {
	t.Parallel()
	cfg := federatedWorld()
	world := newWorld(t, cfg, simTrace{Seed: 0, Ticks: 1, Config: cfg.Name})
	defer world.close()
	// Tick two, because the scope is polled every other tick.
	world.tick = simSnapshotEvery
	world.now = simEpoch.Add(time.Duration(world.tick) * simTick)

	// Four jobs arrive in the shared scope. Placement is GitHub's: the world
	// hands each one to the set with the most room.
	for range 4 {
		if job := world.arrive("a/repo", "maestro", "push"); job == nil {
			t.Fatal("the federated scope must accept arrivals")
		}
	}
	// GitHub's own placement, made explicit rather than inferred: one job to the
	// first set, three to the second.
	world.jobs[0].binding, world.jobs[1].binding = 0, 1
	world.jobs[2].binding, world.jobs[3].binding = 1, 1

	world.produceMessages()
	world.deliverMessages()
	world.deliverSnapshots()
	if world.restCommitted == nil || len(world.restCommitted.jobs) != 4 {
		t.Fatalf("the scope observation must carry all four queued jobs, got %#v", world.restCommitted)
	}
	if len(world.findings) != 0 {
		t.Fatalf("a shared-label scope must not fail its own observation: %v", world.findings)
	}

	for _, expected := range []struct {
		key   int64
		count int
	}{{key: 1, count: 1}, {key: 2, count: 3}} {
		jobs, err := world.store.QueuedGitHubJobs(world.ctx, expected.key)
		if err != nil {
			t.Fatalf("read attributed jobs for scale set %d: %v", expected.key, err)
		}
		if len(jobs) != expected.count {
			t.Fatalf("scale set %d attributed %d of the scope's four queued jobs, want %d",
				expected.key, len(jobs), expected.count)
		}
	}
}
