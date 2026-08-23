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
// # The two axes
//
// A reserved head is admissible only when BOTH hold: its vector fits the free
// envelope, and its repository is under its cap. `scheduler.feasible` folds
// exactly those two terms into one boolean, and `planLinux` holds the
// reservation when that boolean is false — for EITHER reason.
//
// `reservedResidual` asked only the first. It subtracted the head's whole vector
// from the envelope every other demand is judged against whenever
// `free.Sub(profile.Resources)` succeeded, and never read `RepoCaps`. So for a
// head that fits the vector and is held out by its own repository's cap, the
// oracle withheld six vCPU on behalf of a head it had, four lines further down
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
// # What a cap-held head may lend, and what it may not
//
// Not everything. ADR 0017's Consequences promise that "anything admitted in
// this path is by construction too small to fit the reserved vector, so no
// equal-or-larger job can jump the queue". That construction is automatic while
// the only capacity ever lent belongs to a head that does NOT fit the envelope.
// It is not automatic for a cap-held head, whose vector `free` holds in full: an
// equal-or-larger peer admitted there takes exactly the vector the oldest aged
// demand is entitled to, and inverts the aged FIFO the reservation exists to
// protect.
//
// So the oracle models what ADR 0038 decides — a cap-held head lends its vector
// to work it still outranks, and withholds it from work that could take the
// vector whole — and both halves are pinned here, in both directions.

// capHeldWorld is the container-node arm, where `c/repo`'s cap is two.
func capHeldWorld() worldConfig { return containerNodeWorld() }

// issue226Live is the live occupancy of issue #226's own state block: `c/repo`
// at its cap of two, holding 6 CPU / 12288 MB / 2 slots.
func issue226Live(cfg worldConfig) []domain.Instance {
	return []domain.Instance{
		oracleInstance(cfg, "trf-large-33754a03d97c06c1", "c/repo", "large"),
		oracleInstance(cfg, "trf-medium-befa30d57c9b846b", "c/repo", "medium"),
	}
}

// issue226Available is what the host reports on that tick. Ceiling
// {12, 30720, 4} less live {6, 12288, 2} leaves headroom {6, 18432, 2}, and the
// host reports six cores, so the oracle's free envelope is {6, 18432, 2} — a
// vector the `xl` head fits exactly.
func issue226Available() domain.Resources {
	return domain.Resources{CPU: 6, MemoryMB: 18_432, Slots: 4}
}

// TestOracleLendsACapHeldHeadsVectorToWorkItOutranks is issue #226 stated as a
// property, at the level that settles it.
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
// Before ADR 0038 the oracle answered: withheld = true, residual = {0, 6144, 1},
// and `a/repo`'s `large` — which needs {4, 8192, 1} and fits `free` outright —
// was not "definitely admissible". Property (a) reported nothing, for as many
// consecutive ticks as the cap holder ran.
//
// The head is resource-feasible and cap-infeasible. No amount of freed CPU can
// admit it, so withholding CPU for it cannot be justified by "the pass must not
// delay the head", which is the entire warrant ADR 0029 condition 1 rests on.
func TestOracleLendsACapHeldHeadsVectorToWorkItOutranks(t *testing.T) {
	t.Parallel()
	cfg := capHeldWorld()
	head := oracleDemand(cfg, "c/repo", 9, 13*time.Minute, "xl")
	waiting := oracleDemand(cfg, "a/repo", 10, 9*time.Minute+30*time.Second, "large")
	observation := oracleObservation(53, []domain.Demand{waiting, head}, issue226Live(cfg),
		issue226Available(), reservationOf(cfg, head))

	// The head itself is not admissible, and the oracle knows it: `feasibleDemands`
	// drops it on the repository-cap term. The claim under test is that a head the
	// oracle has ruled out cannot also be a head the oracle reserves a vector for.
	keys := feasibleKeys(cfg, observation)
	if containsKey(keys, head.Key) {
		t.Fatalf("premise broken: the cap-held head must not be admissible to the oracle: %v", keys)
	}
	if !containsKey(keys, waiting.Key) {
		t.Fatalf("issue #226: a %v `large` from a repository with no live instance, against a free "+
			"envelope of {6 CPU, 18432 MB, 2 slots}, is definitely admissible. The oracle withheld the "+
			"whole six-vCPU vector of a head its own repository cap holds out, so it reported the queue "+
			"as correctly served. feasible=%v", cfg.Scheduler.Profiles["large"].Resources, keys)
	}
	findings := runLiveness(cfg, observation)
	if len(findings) != 1 || findings[0].Kind != findingWedge {
		t.Fatalf("issue #226: property (a) must report a residual refused behind a CAP-held head "+
			"exactly as it reports one refused behind a VECTOR-infeasible head "+
			"(TestFeasibilityKeepsTheResidualBehindAnInfeasibleHead); got %v", findings)
	}
}

// TestOracleWithholdsACapHeldHeadsVectorFromAPeerThatCouldTakeIt is the other
// half of ADR 0038, and the half that keeps the correction from becoming a
// blanket release.
//
// The same tick, with the demand waiting behind the head swapped for an `xl` of
// its own: a peer whose vector is EQUAL to the head's. `free` fits it, because
// `free` fits the head. Admitting it would hand the oldest aged demand's exact
// vector to a younger one and invert the aged FIFO the reservation exists to
// protect, which is what ADR 0017's Consequences forbid:
//
//	Because anything admitted in this path is by construction too small to fit
//	the reserved vector, no equal-or-larger job can jump the queue.
//
// So the oracle must go on judging such a peer against `free - reservation`, and
// must NOT call the refusal a wedge. This direction was green before ADR 0038
// only because the oracle refused everything; it is load-bearing afterwards, and
// under ADR 0045 it is the ONLY thing a reservation still withholds.
func TestOracleWithholdsACapHeldHeadsVectorFromAPeerThatCouldTakeIt(t *testing.T) {
	t.Parallel()
	cfg := capHeldWorld()
	head := oracleDemand(cfg, "c/repo", 9, 13*time.Minute, "xl")
	peer := oracleDemand(cfg, "a/repo", 10, 9*time.Minute+30*time.Second, "xl")
	observation := oracleObservation(53, []domain.Demand{peer, head}, issue226Live(cfg),
		issue226Available(), reservationOf(cfg, head))

	if keys := feasibleKeys(cfg, observation); len(keys) != 0 {
		t.Fatalf("a peer that could take the cap-held head's whole vector is not definitely "+
			"admissible — ADR 0017 lets nothing equal-or-larger jump the queue: %v", keys)
	}
	if findings := runLiveness(cfg, observation); len(findings) > 0 {
		t.Fatalf("property (a) must not report a tick that is correctly keeping an equal-or-larger "+
			"peer out of the reserved vector: %s", findings[0])
	}
}

// TestIssue216AndIssue226PinTheSameState is the bridge, and it asserts
// arithmetic only, no policy.
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
// `TestFeasibilityWithholdsAFittingReservedHeadsVector` and the test above were
// pinned to opposite answers about one state, and the reason no seed in 1-120
// still met `sigReservedHeadHeldByARepositoryCap` was not that the wedge left:
// it is that #220 declared this shape correct.
//
// The resolution could therefore never be "#226 is a duplicate of #216 and #216
// is fixed". Those were one question with two answers on the record, and ADR
// 0038 answers it once: #216's tick is a real wedge on the cap axis, and #220's
// pin is re-anchored to a head held by the VECTOR axis, which is the axis #220
// was right about.
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
	head226 := oracleDemand(cfg, "c/repo", 9, 13*time.Minute, "xl")
	free226 := domain.Resources{CPU: 6, MemoryMB: 18_432, Slots: 2}

	for _, test := range []struct {
		issue string
		live  []domain.Instance
		head  domain.Demand
		free  domain.Resources
	}{
		{"#216 (seed 92, tick 188)", live216, head216, free216},
		{"#226 (seed 67, tick 53)", issue226Live(cfg), head226, free226},
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

// TestIssue226ReducesToOneTick retires the caveat issue #226 filed against
// itself, and is the finding-7 characterization in its corrected form.
//
// The issue said, under "What is NOT claimed":
//
//	A direct scheduler.PlanTick over the same instances, demands, and prior
//	reservation ADMITS the waiting demand, so this is not a property of one
//	tick's inputs — it is the cross-tick composition that arm reaches.
//
// It is a property of one tick's inputs, and that matters for how it is pinned:
// the trace pin this finding used to carry only holds while `overrun_job` is
// armed, because the wedge lasts exactly as long as the cap holder runs. The
// tick holds unconditionally.
//
// Before ADR 0038 `PlanTick` admitted NOTHING here, and admitted nothing with
// the prior reservation removed too: the aged head is the oldest demand,
// `feasible` refuses it on the cap term, `planLinux` mints a fresh reservation
// for it, and `safeBackfill` subtracted the head's whole six-vCPU vector from a
// six-vCPU envelope, leaving {0, 6144, 1} for a `large` that needs {4, 8192, 1}.
// The mechanism the original signature declined to name is `safeBackfill`'s
// remainder subtraction, and this test names it.
func TestIssue226ReducesToOneTick(t *testing.T) {
	t.Parallel()
	cfg := capHeldWorld()
	head := oracleDemand(cfg, "c/repo", 9, 13*time.Minute, "xl")
	waiting := oracleDemand(cfg, "a/repo", 10, 9*time.Minute+30*time.Second, "large")
	host := domain.Fresh(domain.Host{Available: issue226Available(),
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
			Instances: domain.Fresh(issue226Live(cfg), oracleNow), Host: host, Prior: test.prior})
		if plan.Status != scheduler.PlanReady {
			t.Fatalf("%s: the plan must be ready, got %v", test.name, plan.Status)
		}
		spawned := spawnedKeys(plan)
		if len(spawned) != 1 || spawned[0] != waiting.Key {
			t.Fatalf("%s: FINDING 7 (issue #226): the cap-held head must lend its vector to the "+
				"`large` it outranks; `safeBackfill`'s remainder subtraction sterilized it. "+
				"spawned=%v", test.name, spawned)
		}
		if plan.Next.Reservation == nil || plan.Next.Reservation.Demand != head.Key {
			t.Fatalf("%s: lending the vector must not drop the reservation — the head is still "+
				"first in line for the cap slot it waits on, got %v", test.name, plan.Next.Reservation)
		}
	}
}
