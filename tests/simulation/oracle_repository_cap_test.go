package simulation_test

import (
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

// This file is issue #226 asked at the ORACLE level, which is the only level
// that can answer it. A scheduler test cannot distinguish an oracle that is
// right from one that agrees with the code it checks, and that is precisely the
// failure mode under investigation: PR #220 taught `feasibleDemands` to model
// what a reservation withholds on ONE axis, and the harness stopped asking about
// the other.
//
// TestOracleWithholdsNothingForAHeadItsRepositoryCapHolds is RED on `main`, and
// is meant to be. It states the invariant; it does not implement it.
//
// # The two axes
//
// A reserved head is admissible only when BOTH hold: its vector fits the free
// envelope, and its repository is under its cap. `scheduler.feasible` folds
// exactly those two terms into one boolean, and `planLinux` holds the
// reservation when that boolean is false — for EITHER reason.
//
// `reservedResidual` asks only the first. It subtracts the head's whole vector
// from the envelope every other demand is judged against whenever
// `free.Sub(profile.Resources)` succeeds, and never reads `RepoCaps`. So for a
// head that fits the vector and is held out by its own repository's cap, the
// oracle withholds six vCPU on behalf of a head it has, four lines further down
// in the very same function, already ruled inadmissible on the cap term.
//
// # Why that direction of imprecision is the dangerous one
//
// PR #220 considered the repository cap and left it out, with this reasoning:
//
//	What is deliberately NOT modelled is ADR 0030's repository slot: a head also
//	holds one slot of its own repository's cap... Charging it here would narrow
//	the oracle further, and narrowing is the direction that BLINDS a property.
//
// That is right about ADR 0030's slot, which is a charge against OTHER demands.
// It is backwards for the head's own feasibility predicate, which is the term
// this file is about. Making the head's predicate stricter — vector AND cap —
// makes `withheld` FALSE more often, which WIDENS the residual and produces MORE
// reports. It errs in exactly the direction #220 says a harness is allowed to err
// in. Leaving the cap out of it errs the other way: it suppresses reports.
//
// # What the ADRs actually decided
//
// ADR 0017's decision text is stated on the vector ("when the reserved vector
// does not fit the free envelope"), because when it was written the vector was
// the only axis in that predicate. Its RATIONALE is stated as a general
// criterion, and it transfers verbatim:
//
//	Holding the residual idle in that state protects nothing: a head that does
//	not fit cannot start until live instances release the resources it needs,
//	whatever backfill does. It is not waiting on backfill to stop.
//
// A cap-held head cannot start until one of its own repository's instances
// exits, whatever backfill does. It is not waiting on backfill to stop either.
//
// ADR 0029 and ADR 0030 then recognised the repository cap as an independent way
// to delay a head ("Resources are not the only way to delay a head") — but only
// in the protective direction, as a bound on which repository may bid. Neither
// record ever decided that a head the cap holds keeps its vector. That behaviour
// is an unexamined consequence of a predicate that predates the axis, not a
// decision, and no ADR argues for it.
//
// Issue #226 is the state where it costs: `c/repo` at its cap of two, an aged
// `c/repo` `xl` head the host could hand six vCPU to right now, and `a/repo`'s
// `large` — from a repository with no live instance at all — refused for as long
// as the cap holder runs. That is ADR 0029's own cost formula: "an idle vector
// the size of the starved profile, for the entire duration of the blocking job."

// capHeldWorld is the container-node arm, where `c/repo`'s cap is two.
func capHeldWorld() worldConfig { return containerNodeWorld() }

// TestOracleWithholdsNothingForAHeadItsRepositoryCapHolds is issue #226 stated
// as a property. IT IS RED ON `main` AND IS NOT A REGRESSION: it is the red-first
// half of a fix that has not been written.
//
// Tick 53 of seed 67 of the container-node arm, rebuilt from the issue's own
// state block:
//
//	next reservation=&{c/repo/1009/1/500009 xl {6 12288 1} ...}
//	  demand   a/repo/1010/1/500010 profile=large age=9m30s
//	  demand   c/repo/1009/1/500009 profile=xl    age=13m0s
//	  instance trf-large-…  repo=c/repo profile=large  state=assigned
//	  instance trf-medium-… repo=c/repo profile=medium state=running
//	host available={CPU:6 MemoryMB:18432 Slots:4}
//
// The oracle's own arithmetic: ceiling {12, 30720, 4} less live {6, 12288, 2}
// leaves headroom {6, 18432, 2}, and the host reports six cores, so
// free = {6 CPU, 18432 MB, 2 slots}. `c/repo` has two live instances against a
// cap of two.
//
// On `main` the oracle answers: withheld = true, residual = {0, 6144, 1}, and
// `a/repo`'s `large` — which needs {4, 8192, 1} and would fit `free` twice over —
// is not "definitely admissible". Property (a) reports nothing, for as many
// consecutive ticks as the cap holder runs.
//
// The head is resource-feasible and cap-infeasible. No amount of freed CPU can
// admit it. Withholding CPU for it therefore cannot be justified by "the pass
// must not delay the head", which is the entire warrant ADR 0029 condition 1
// rests on.
func TestOracleWithholdsNothingForAHeadItsRepositoryCapHolds(t *testing.T) {
	t.Parallel()
	cfg := capHeldWorld()
	live := []domain.Instance{
		oracleInstance(cfg, "trf-large-33754a03d97c06c1", "c/repo", "large"),
		oracleInstance(cfg, "trf-medium-befa30d57c9b846b", "c/repo", "medium"),
	}
	head := oracleDemand(cfg, "c/repo", 9, 13*time.Minute, "xl")
	waiting := oracleDemand(cfg, "a/repo", 10, 9*time.Minute+30*time.Second, "large")
	observation := oracleObservation(53, []domain.Demand{waiting, head}, live,
		domain.Resources{CPU: 6, MemoryMB: 18_432, Slots: 4}, reservationOf(cfg, head))

	// The head itself is not admissible, and the oracle knows it: `feasibleDemands`
	// drops it on the repository-cap term. The claim under test is that a head the
	// oracle has ruled out cannot also be a head the oracle reserves a vector for.
	keys := feasibleKeys(cfg, observation)
	if containsKey(keys, head.Key) {
		t.Fatalf("premise broken: the cap-held head must not be admissible to the oracle: %v", keys)
	}
	if !containsKey(keys, waiting.Key) {
		t.Fatalf("RED (issue #226): a %v `large` from a repository with no live instance, "+
			"against a free envelope of {6 CPU, 18432 MB, 2 slots}, is definitely admissible. "+
			"The oracle withholds the whole six-vCPU vector of a head its own repository cap "+
			"holds out, so it reports the queue as correctly served. feasible=%v",
			cfg.Scheduler.Profiles["large"].Resources, keys)
	}
	findings := runLiveness(cfg, observation)
	if len(findings) != 1 || findings[0].Kind != findingWedge {
		t.Fatalf("RED (issue #226): property (a) must report a residual refused behind a "+
			"CAP-held head exactly as it reports one refused behind a VECTOR-infeasible head "+
			"(TestFeasibilityKeepsTheResidualBehindAnInfeasibleHead); got %v", findings)
	}
}

// TestIssue216AndIssue226PinTheSameState is the bridge, and it is GREEN: it
// asserts arithmetic only, no policy.
//
// PR #220 fixed issue #216 by teaching the oracle to stop reporting the seed-92
// tick of the container-node arm. That tick's head is an `ops/fleet` `xl` held
// out by `ops/fleet`'s cap of two, which two live `ops/fleet` `small` instances
// have filled — the PR body says so itself. Issue #226's tick is a `c/repo` `xl`
// held out by `c/repo`'s cap of two, which two live `c/repo` instances have
// filled.
//
// The two states differ in repository name, in which profiles fill the cap, and
// in the size of the demand waiting behind. They do not differ in shape. So
// `TestFeasibilityWithholdsAFittingReservedHeadsVector` and the test above are
// pinned to opposite answers about one state, and the reason no seed in 1-120
// still meets `sigReservedHeadHeldByARepositoryCap` is not that the wedge left:
// it is that #220 declared this shape correct.
//
// Whatever the resolution, it cannot be "#226 is a duplicate of #216 and #216 is
// fixed". Those are one question with two answers on the record.
func TestIssue216AndIssue226PinTheSameState(t *testing.T) {
	t.Parallel()
	cfg := capHeldWorld()

	// Issue #216, seed 92 tick 188 (PR #220's own dump).
	live216 := []domain.Instance{
		oracleInstance(cfg, "trf-medium-1", "b/repo", "medium"),
		oracleInstance(cfg, "trf-small-1", simControlPlaneRepo, "small"),
		oracleInstance(cfg, "trf-small-2", simControlPlaneRepo, "small"),
	}
	head216 := oracleDemand(cfg, simControlPlaneRepo, 8, 24*time.Minute+30*time.Second, "xl")
	free216 := domain.Resources{CPU: 7, MemoryMB: 22_528, Slots: 1}

	// Issue #226, seed 67 tick 53.
	live226 := []domain.Instance{
		oracleInstance(cfg, "trf-large-1", "c/repo", "large"),
		oracleInstance(cfg, "trf-medium-1", "c/repo", "medium"),
	}
	head226 := oracleDemand(cfg, "c/repo", 9, 13*time.Minute, "xl")
	free226 := domain.Resources{CPU: 6, MemoryMB: 18_432, Slots: 2}

	for _, test := range []struct {
		issue string
		live  []domain.Instance
		head  domain.Demand
		free  domain.Resources
	}{
		{"#216 (seed 92, tick 188)", live216, head216, free216},
		{"#226 (seed 67, tick 53)", live226, head226, free226},
	} {
		occupied := 0
		for _, instance := range test.live {
			if instance.Repo == test.head.Key.Repo {
				occupied++
			}
		}
		vector := cfg.Scheduler.Profiles[test.head.Profile].Resources
		if !test.free.CanFit(vector) {
			t.Fatalf("%s: the head's vector %v must FIT the free envelope %v — that is what makes "+
				"it the cap axis rather than ADR 0017's vector axis", test.issue, vector, test.free)
		}
		if occupied < repoCap(cfg, test.head.Key.Repo) {
			t.Fatalf("%s: %s must be AT its cap of %d, got %d live", test.issue,
				test.head.Key.Repo, repoCap(cfg, test.head.Key.Repo), occupied)
		}
	}
}

// TestScheduler226ReducesToOneTick retires the caveat issue #226 filed against
// itself, and it is GREEN: a characterization of today's scheduler.
//
// The issue says, under "What is NOT claimed":
//
//	A direct scheduler.PlanTick over the same instances, demands, and prior
//	reservation ADMITS the waiting demand, so this is not a property of one
//	tick's inputs — it is the cross-tick composition that arm reaches.
//
// It is a property of one tick's inputs. `PlanTick` over the issue's own state
// block admits nothing, and it admits nothing WITH THE PRIOR RESERVATION REMOVED
// too: the aged head is the oldest demand, `feasible` refuses it on the cap term,
// `planLinux` mints a fresh reservation for it, and `safeBackfill` subtracts the
// head's whole six-vCPU vector from a six-vCPU envelope, leaving {0, 6144, 1} for
// a `large` that needs {4, 8192, 1}.
//
// So the finding does not need a trace, a signature, or an arm to reproduce, and
// the mechanism the original signature declined to name is `safeBackfill`'s
// remainder subtraction.
func TestScheduler226ReducesToOneTick(t *testing.T) {
	t.Parallel()
	cfg := capHeldWorld()
	live := []domain.Instance{
		oracleInstance(cfg, "trf-large-33754a03d97c06c1", "c/repo", "large"),
		oracleInstance(cfg, "trf-medium-befa30d57c9b846b", "c/repo", "medium"),
	}
	head := oracleDemand(cfg, "c/repo", 9, 13*time.Minute, "xl")
	waiting := oracleDemand(cfg, "a/repo", 10, 9*time.Minute+30*time.Second, "large")
	host := domain.Fresh(domain.Host{Available: domain.Resources{CPU: 6, MemoryMB: 18_432, Slots: 4},
		Capacity: domain.Resources{CPU: 12, MemoryMB: 30_720, Slots: 4}}, oracleNow)

	for _, test := range []struct {
		name  string
		prior scheduler.State
	}{
		{"the reservation carried in", scheduler.State{Reservation: reservationOf(cfg, head)}},
		{"no prior reservation at all", scheduler.State{}},
	} {
		plan := scheduler.PlanTick(scheduler.Input{Now: oracleNow, Config: cfg.Scheduler,
			Demands:   domain.Fresh([]domain.Demand{waiting, head}, oracleNow),
			Instances: domain.Fresh(live, oracleNow), Host: host, Prior: test.prior})
		if plan.Status != scheduler.PlanReady {
			t.Fatalf("%s: the plan must be ready, got %v", test.name, plan.Status)
		}
		if spawned := spawnedKeys(plan); len(spawned) != 0 {
			t.Fatalf("%s: today's scheduler admits nothing on this tick; if that has changed, "+
				"issue #226 is fixed and this characterization is its regression: %v", test.name, spawned)
		}
		if plan.Next.Reservation == nil || plan.Next.Reservation.Demand != head.Key {
			t.Fatalf("%s: the cap-held head must hold the reservation, got %v", test.name, plan.Next.Reservation)
		}
	}
}
