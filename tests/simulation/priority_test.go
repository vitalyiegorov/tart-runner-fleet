package simulation_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

// simEscalation is the escalation threshold of the tiered arm: ten ticks. It is
// deliberately short relative to a run so escalation actually resolves inside a
// simulated history instead of being a promise the harness never reaches.
const simEscalation = 5 * time.Minute

// simReleaseRepos are the repositories the tiered world declares as release
// work. Two of the four repositories carry the high tier, which is what makes
// the arm adversarial: the stream of tier work never stops, so the default tier
// is served only because escalation makes it outrank the stream.
var simReleaseRepos = []string{"a/repo", "b/repo"}

// tieredWorld is the priority-tier arm of issue #224. It is the world the
// 2026-08-09 incident describes, generalized: a declared tier that outranks
// ordinary CI, and a relentless stream of that tier's work.
//
// It runs as its own arm rather than as a case inside TestSimFuzz for the same
// reason the budgeted host does -- the seed stream is a function of the world,
// so one binary exploring two worlds must explore each of them fully. Adding it
// leaves every existing arm's history untouched, which is what keeps the corpus
// digests of a fleet that declares no tier where they were.
func tieredWorld() worldConfig {
	cfg := defaultWorld()
	cfg.Name = "tiered-release-priority"
	cfg.Priority = domain.PriorityPolicy{Tiers: []domain.PriorityTier{
		{Name: "release", Match: []domain.PriorityMatch{
			{Scope: simReleaseRepos[0]}, {Scope: simReleaseRepos[1]},
		}},
	}}
	cfg.Scheduler.PriorityEscalation = simEscalation
	// A demand of the default tier is overtaken by release work only while its
	// effective tier is lower, and escalation lifts it one rank per threshold.
	// One declared tier therefore costs at most one threshold of extra wait, and
	// the budget below is that arithmetic plus the ordinary liveness slack.
	cfg.TierStarvationT = len(cfg.Priority.Tiers)*int(simEscalation/simTick) + cfg.LivenessK
	return cfg
}

// effectiveTierOf is the harness's own reading of a demand's priority tier. It
// recomputes the rule from the demand and the clock rather than calling the
// scheduler, because an oracle that asks the implementation what it did proves
// nothing about what it should have done.
func effectiveTierOf(cfg worldConfig, now time.Time, demand domain.Demand) int {
	tier := demand.Priority.Rank
	if cfg.Scheduler.PriorityEscalation <= 0 || demand.CreatedAt.IsZero() {
		return tier
	}
	if waited := now.Sub(demand.CreatedAt); waited > 0 {
		tier += int(waited / cfg.Scheduler.PriorityEscalation)
	}
	return tier
}

// tiered reports whether this world declares any priority tier. Every oracle
// below is inert without one, which is how a world that predates issue #224
// keeps producing the history it always produced.
func (c worldConfig) tiered() bool { return c.Priority.Declared() }

// ---------------------------------------------------------------------------
// (l) Tier order is respected when the vectors are equal.
// ---------------------------------------------------------------------------

// tierInversionChecker fails when a tick admits a demand while leaving behind a
// feasible demand of the SAME platform, the SAME resource vector, and the SAME
// age band that outranks it on tier. Equal vectors are the case with no other
// explanation: the two demands compete for one identical slot, no packing
// argument distinguishes them, and no residual-arbitration or count-maximization
// defect can be blamed. Whatever decided that tick decided it on order alone,
// and order inside a band is what a declared tier is.
//
// The comparison is confined to one of ADR 0004's lanes, because a tier ranks
// INSIDE a lane and never across one. An aged demand ahead of a fresh one of a
// higher tier is aging working, and a young control-plane demand ahead of a
// young release is the fleet keeping its own repair path open -- neither is a
// tier being ignored. The reserved head is exempt for the reason property (b)
// exempts it: it is protected by ordering, and admission behind it is the
// decision rather than a violation of it. So is the single repository slot that
// head holds (ADR 0030; issue #255) -- a demand that exclusion removes from the
// candidate list was never competing for the slot the tick handed out -- and
// capAllowsSwap charges that slot beside ADR 0012's cap.
func tierInversionChecker(cfg worldConfig) checker {
	return func(w *world, observation tickObservation) []finding {
		if !cfg.tiered() || w.tearingDown(observation) {
			return nil
		}
		admitted := admittedDemands(observation)
		var findings []finding
		for _, waiting := range feasibleDemands(cfg, observation) {
			if containsDemand(admitted, waiting) || holdsReservation(observation, waiting) {
				continue
			}
			for _, chosen := range admitted {
				if !sameSlot(cfg, waiting, chosen) || schedulingLane(cfg, observation.Now, waiting) != schedulingLane(cfg, observation.Now, chosen) {
					continue
				}
				if effectiveTierOf(cfg, observation.Now, waiting) <= effectiveTierOf(cfg, observation.Now, chosen) ||
					!capAllowsSwap(cfg, observation, waiting, chosen) {
					continue
				}
				findings = append(findings, finding{Kind: findingTierInversion, Tick: observation.Tick,
					Detail: fmt.Sprintf("tier %d %s (%s old) left waiting while tier %d %s (%s old) took an identical slot\n%s",
						effectiveTierOf(cfg, observation.Now, waiting), waiting.Key, observation.Now.Sub(waiting.CreatedAt),
						effectiveTierOf(cfg, observation.Now, chosen), chosen.Key, observation.Now.Sub(chosen.CreatedAt),
						w.dumpPlan(observation))})
			}
		}
		return findings
	}
}

// capAllowsSwap asks whether the waiting demand could actually have taken the
// slot the chosen one took. A repository cap is charged against live instances
// AND against everything this same plan already admits, so a queue whose head
// tier belongs to a repository this tick has already filled was not passed over
// on tier grounds -- it was refused by ADR 0012's cap, which outranks every
// ordering question in this package. The swap frees the chosen demand's own
// charge, because that is the admission being questioned.
//
// A held reservation charges one more slot of its OWN repository, and that term
// is issue #255. ADR 0030 sets the head's future slot aside before anything else
// may bid -- `slack = cap - occupied - 1` -- and its `slack <= 0` clause is a
// wholesale exclusion in as many words: "the head is waiting for the last free
// slot ... nothing from its repository may be admitted". A demand that exclusion
// removes was never a candidate for the slot the tick handed out, so calling the
// admission an inversion states something false about the tick.
//
// This is the counterfactual question, not the feasibility question, and the two
// are allowed to differ. `reservedResidual` deliberately does NOT charge this
// slot, because there the answer decides whether a demand is admissible at all
// and a narrower answer BLINDS properties (a) and (b) -- they keep reporting an
// ADR 0030 hold that never ends, with no tolerance spent on the ticks it is
// legitimate. Here the answer decides whether one demand could have taken
// ANOTHER'S slot, and an un-charged written rule is simply a wrong answer: the
// property has no repeat tolerance, so it fails the build on the first tick the
// rule binds.
//
// The exemption cannot hide an inverted tier, because of what the head is: a
// reservation is only ever held for the demand `priorityOrder` ranked first in
// the aged band, by effective tier and then by age (ADR 0037 §4), and it is
// re-derived the moment anything outranks it. So the demand this term leaves
// waiting is waiting for one that outranks it, and property (n) still bounds how
// long any tier-based pass-over may last.
func capAllowsSwap(cfg worldConfig, observation tickObservation, waiting, chosen domain.Demand) bool {
	occupied := reservedRepoSlot(observation, waiting.Key.Repo)
	for _, instance := range observation.Instances {
		if instance.ConsumesHostResources() && !instance.State.TearingDown() &&
			instance.State != domain.InstanceOnlineIdle && instance.Repo == waiting.Key.Repo {
			occupied++
		}
	}
	for _, admitted := range admittedDemands(observation) {
		if admitted.Key.Repo == waiting.Key.Repo && admitted.Key != chosen.Key {
			occupied++
		}
	}
	return occupied < repoCap(cfg, waiting.Key.Repo)
}

// reservedRepoSlot is the `- 1` of ADR 0030's slack arithmetic: one slot of the
// reserved head's own repository, set aside before anything else may bid. The
// head itself never pays it, because tierInversionChecker has already excused
// every demand holdsReservation names before it asks this question.
//
// It reads the reservation from the plan's own next state -- a decision the plan
// publishes, the same fact `holdsReservation` and `reservedResidual` already
// read -- and never from any envelope the scheduler computed, so ADR 0031's
// independence rule holds here exactly as it does there.
func reservedRepoSlot(observation tickObservation, repo string) int {
	reservation := observation.Plan.Next.Reservation
	if reservation == nil || reservation.Demand.Repo != repo {
		return 0
	}
	return 1
}

// sameSlot reports whether two demands compete for one identical vector on one
// platform. Anything else is a packing question, and packing is what properties
// (b) and (g) already arbitrate.
func sameSlot(cfg worldConfig, a, b domain.Demand) bool {
	return a.Platform == b.Platform &&
		cfg.Scheduler.Profiles[a.Profile].Resources == cfg.Scheduler.Profiles[b.Profile].Resources
}

// schedulingLane is ADR 0004's lane for one demand, recomputed by the harness:
// aged global FIFO, then young control-plane, then young standard. A priority
// tier orders demands inside one of these and never reorders the lanes.
func schedulingLane(cfg worldConfig, now time.Time, demand domain.Demand) int {
	if aged(cfg, now, demand) {
		return 0
	}
	if cfg.Scheduler.RepoSchedulingClasses[demand.Key.Repo] == domain.SchedulingControlPlane {
		return 1
	}
	return 2
}

func containsDemand(demands []domain.Demand, demand domain.Demand) bool {
	for _, candidate := range demands {
		if candidate.Key == demand.Key {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// (m) Escalation is monotonic.
// ---------------------------------------------------------------------------

// escalationChecker fails when a waiting demand's effective priority tier goes
// DOWN. Escalation is the whole safety argument for tiers: it converts "a tier
// may overtake" into "a tier may overtake for a bounded time", and it can only
// do that if a demand never loses ground it has already gained. A tier that
// could fall would also make the order non-deterministic in the one direction
// nobody would look, because the demand that lost rank is by definition the one
// nobody is watching.
func escalationChecker(cfg worldConfig) checker {
	highest := map[string]int{}
	return func(_ *world, observation tickObservation) []finding {
		if !cfg.tiered() {
			return nil
		}
		var findings []finding
		for _, demand := range observation.Demands {
			key := demand.Key.String()
			tier := effectiveTierOf(cfg, observation.Now, demand)
			previous, known := highest[key]
			if known && tier < previous {
				findings = append(findings, finding{Kind: findingEscalationRegression, Tick: observation.Tick,
					Detail: fmt.Sprintf("%s fell from tier %d to tier %d after waiting %s",
						demand.Key, previous, tier, observation.Now.Sub(demand.CreatedAt))})
			}
			if !known || tier > previous {
				highest[key] = tier
			}
		}
		return findings
	}
}

// ---------------------------------------------------------------------------
// (n) A declared tier cannot starve the default tier.
// ---------------------------------------------------------------------------

// tierStarvationChecker bounds the exemption property (b) grants a declared
// tier. Property (b) stops counting a pass-over the declared policy explains --
// an overtaker of a strictly higher effective tier -- because that IS the
// written invariant once a tier is declared. This oracle is what keeps that
// exemption honest: escalation must end it. A feasible demand passed over on
// tier grounds for more than T ticks means escalation is not delivering, and a
// tier that never ends is the starvation the whole design promised to avoid.
func tierStarvationChecker(cfg worldConfig) checker {
	passedOver := map[string]int{}
	return func(w *world, observation tickObservation) []finding {
		if !cfg.tiered() || w.tearingDown(observation) {
			return nil
		}
		admitted := admittedDemands(observation)
		if len(admitted) == 0 {
			return nil
		}
		var findings []finding
		for _, waiting := range feasibleDemands(cfg, observation) {
			overtaker, overtaken := youngestOvertaker(admitted, waiting)
			if !overtaken || !outranksOnTier(cfg, observation.Now, overtaker, waiting) {
				continue
			}
			key := waiting.Key.String()
			passedOver[key]++
			if passedOver[key] != cfg.TierStarvationT+1 {
				continue
			}
			findings = append(findings, finding{Kind: findingTierStarvation, Tick: observation.Tick,
				Detail: fmt.Sprintf("%s (%s old, tier %d) passed over on tier grounds %d times, this tick by %s (tier %d)\n%s",
					waiting.Key, observation.Now.Sub(waiting.CreatedAt), effectiveTierOf(cfg, observation.Now, waiting),
					passedOver[key], overtaker.Key, effectiveTierOf(cfg, observation.Now, overtaker),
					w.dumpPlan(observation))})
		}
		return findings
	}
}

// outranksOnTier reports whether the declared policy, with escalation applied,
// explains one demand going ahead of another. The overtaker must be in the aged
// band too: a tier ranks inside a band and never across one, so a young demand
// ahead of an aged one is never explained by a tier.
func outranksOnTier(cfg worldConfig, now time.Time, overtaker, waiting domain.Demand) bool {
	return cfg.tiered() && aged(cfg, now, overtaker) &&
		effectiveTierOf(cfg, now, overtaker) > effectiveTierOf(cfg, now, waiting)
}

// ---------------------------------------------------------------------------
// The adversarial stream, stated deterministically.
// ---------------------------------------------------------------------------

// TestAnAdversarialReleaseStreamCannotStarveTheDefaultTier is the bound of the
// whole feature, proved rather than sampled.
//
// The adversary is a ROLLING BACKLOG, not a burst of fresh work: on every tick a
// release-tier demand appears that is already past the fairness age and is still
// younger than the waiter. Nothing but the tier separates it from the waiter --
// same band, same vector, younger -- so under tier order alone it takes the one
// builder slot every tick, forever, and the waiter never runs.
//
// Escalation is what ends it. The waiter climbs one rank per threshold, and one
// declared tier is one rank, so it must take the slot within one threshold of
// waiting. It does.
func TestAnAdversarialReleaseStreamCannotStarveTheDefaultTier(t *testing.T) {
	const escalation = 30 * time.Minute
	config := tierSchedulerConfig(escalation)
	start := simEpoch
	// The waiter is already past the fairness age when the run begins, so every
	// adversary below shares its band and is younger than it.
	waiter := priorityDemand("waiter/repo", 1, start.Add(-2*config.FairnessAge), domain.Priority{})

	admitted := -1
	for tick := 1; tick <= 400; tick++ {
		now := start.Add(time.Duration(tick) * simTick)
		// Aged by one second more than the fairness age, so it shares the waiter's
		// band, and always younger than the waiter, so age can never explain it
		// winning. Only the declared tier can.
		stream := priorityDemand("release/repo", int64(1_000+tick),
			now.Add(-config.FairnessAge-time.Second), domain.Priority{Tier: "release", Rank: 1})
		if !stream.CreatedAt.After(waiter.CreatedAt) {
			t.Fatalf("tick %d: the adversary is older than the waiter, which is FIFO rather than tier", tick)
		}
		plan := scheduler.PlanTick(scheduler.Input{Now: now, Config: config,
			Demands:   domain.Fresh([]domain.Demand{waiter, stream}, now),
			Instances: domain.Fresh([]domain.Instance{}, now),
			Host:      domain.Fresh(domain.Host{Available: domain.Resources{CPU: 6, MemoryMB: 12_288, Slots: 1}}, now)})
		if planAdmits(plan, waiter.Key) {
			admitted = tick
			break
		}
		if !planAdmits(plan, stream.Key) {
			t.Fatalf("tick %d admitted nothing at all: %s", tick, dumpKeys(plan))
		}
	}

	if admitted < 0 {
		t.Fatal("an adversarial release backlog starved the default tier forever")
	}
	// One declared tier is one rank, so one threshold of waiting is the whole
	// cost. Anything longer means escalation is not the bound this design claims.
	waited := start.Add(time.Duration(admitted) * simTick).Sub(waiter.CreatedAt)
	if waited > escalation+simTick {
		t.Fatalf("the default tier waited %s behind one declared tier, want at most %s", waited, escalation+simTick)
	}
}

// TestATierNeverOvertakesAnEqualVectorItAlreadyOutranks states the first new
// property without the simulator: at equal vectors the tier decides, in both
// directions, and the answer does not depend on the order the demands arrived
// in the observation.
func TestATierNeverOvertakesAnEqualVectorItAlreadyOutranks(t *testing.T) {
	config := tierSchedulerConfig(time.Hour)
	now := simEpoch.Add(time.Hour)
	release := priorityDemand("release/repo", 1, now.Add(-10*time.Minute), domain.Priority{Tier: "release", Rank: 1})
	standard := priorityDemand("standard/repo", 2, now.Add(-20*time.Minute), domain.Priority{})

	for _, observed := range [][]domain.Demand{{release, standard}, {standard, release}} {
		plan := scheduler.PlanTick(scheduler.Input{Now: now, Config: config,
			Demands:   domain.Fresh(observed, now),
			Instances: domain.Fresh([]domain.Instance{}, now),
			Host:      domain.Fresh(domain.Host{Available: domain.Resources{CPU: 6, MemoryMB: 12_288, Slots: 1}}, now)})
		if !planAdmits(plan, release.Key) || planAdmits(plan, standard.Key) {
			t.Fatalf("equal vectors ignored the tier order: %s", dumpKeys(plan))
		}
	}
}

// TestEscalationOnlyEverRaisesATier is the monotonicity property stated over a
// long wait, at the granularity the fleet actually ticks at.
func TestEscalationOnlyEverRaisesATier(t *testing.T) {
	cfg := tieredWorld()
	demand := priorityDemand("standard/repo", 1, simEpoch, domain.Priority{})
	previous := -1
	for tick := range 2_000 {
		tier := effectiveTierOf(cfg, simEpoch.Add(time.Duration(tick)*simTick), demand)
		if tier < previous {
			t.Fatalf("tier fell from %d to %d at tick %d", previous, tier, tick)
		}
		previous = tier
	}
	if previous < len(cfg.Priority.Tiers) {
		t.Fatalf("escalation never reached the top declared tier: %d", previous)
	}
}

func tierSchedulerConfig(escalation time.Duration) scheduler.Config {
	cfg := defaultWorld().Scheduler
	cfg.PriorityEscalation = escalation
	cfg.RepoCaps = map[string]int{"release/repo": 4, "waiter/repo": 4, "standard/repo": 4}
	return cfg
}

func priorityDemand(repo string, jobID int64, createdAt time.Time, priority domain.Priority) domain.Demand {
	return domain.Demand{Key: domain.DemandKey{Repo: repo, RunID: 900, Attempt: 1, JobID: jobID},
		CreatedAt: createdAt, Profile: "builder", Route: "macos-builder", Platform: domain.PlatformMacOS,
		Event: domain.EventPush, RunStatus: domain.RunQueued, Priority: priority}
}

func planAdmits(plan scheduler.Plan, key domain.DemandKey) bool {
	for _, operation := range plan.Operations {
		if operation.Kind == scheduler.OperationSpawn && operation.Demand == key {
			return true
		}
	}
	return false
}

func dumpKeys(plan scheduler.Plan) string {
	keys := make([]string, 0, len(plan.Operations))
	for _, operation := range plan.Operations {
		keys = append(keys, string(operation.Kind)+" "+operation.Demand.String())
	}
	return fmt.Sprint(keys)
}

// TestSimFuzzTieredPriority is the seed sweep on the tiered arm: a declared
// release tier, a relentless stream of its work, and every property of ADR 0031
// still holding -- including bounded starvation, which survives only because
// escalation ends every tier-based pass-over.
func TestSimFuzzTieredPriority(t *testing.T) {
	t.Parallel()
	sweep(t, "TestSimFuzzTieredPriority", tieredWorld())
}
