package simulation_test

import (
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

// ---------------------------------------------------------------------------
// Issue #404 -- two oracles judged a tick by an order the fleet does not use
// ---------------------------------------------------------------------------
//
// The nightly sweep went red on 2026-10-16 and stayed red. Each night's only
// reported finding was in the oracle, not the fleet, and the tests below are
// the two ticks as the sweep found them. Like the rest of the oracle tests in
// this package they build the observation directly: a scheduler test cannot
// tell an oracle that is right from one that agrees with the code it checks.

// admitting is a ready observation whose plan spawned exactly these demands.
func admitting(observation tickObservation, admitted ...domain.Demand) tickObservation {
	for _, demand := range admitted {
		observation.Plan.Operations = append(observation.Plan.Operations,
			scheduler.Operation{Kind: scheduler.OperationSpawn, Demand: demand.Key, Profile: demand.Profile})
	}
	return observation
}

// TestIssue404CrossPlatformHeadBreaksAnAgeTieByKey is seed 296, tick 149 of the
// mini arm (nightly 2026-10-26 and 2026-10-27).
//
// Two Linux demands arrived on the same tick: `c/repo`'s `xl` and
// `ops/fleet`'s `medium`, both 55m30s old. Behind them was an aged macOS
// `maestro`, 53m30s old. The plan admitted an older `small` and the `maestro`.
// The scheduler orders the aged band by tier, then age, then key (`demandLess`,
// ADR 0057's "lexicographic key as the final tiebreak"), so its Linux head was
// the `xl`. Four cores cannot hold six, so the `maestro` took nothing that head
// was waiting for (ADR 0045, ADR 0049). The oracle broke the tie on the order
// the observation happened to list them in, judged the `medium` as the head,
// and reported an inversion against a demand that was second in the queue.
//
// Property (r) judges only the head; ADR 0049 records why that restriction is
// load-bearing. So which demand IS the head has to be the one `priorityOrder`
// puts first, and the second case keeps the property firing when the key
// really does put the `medium` first.
func TestIssue404CrossPlatformHeadBreaksAnAgeTieByKey(t *testing.T) {
	t.Parallel()
	cfg := defaultWorld()
	live := []domain.Instance{
		oracleInstance(cfg, "trf-large-1", "b/repo", "large"),
		oracleInstance(cfg, "trf-small-1", simControlPlaneRepo, "small"),
	}
	oldest := oracleDemand(cfg, "c/repo", 20, 57*time.Minute, "small")
	tied := 55*time.Minute + 30*time.Second
	maestro := oracleDemand(cfg, simControlPlaneRepo, 25, 53*time.Minute+30*time.Second, "maestro")

	for _, test := range []struct {
		name     string
		xlRepo   string
		medRepo  string
		reported bool
	}{
		// The tick as it happened: the `xl`'s key sorts first, so it is the head.
		{"the key puts the xl first, which the maestro cannot take", "c/repo", simControlPlaneRepo, false},
		// The same tick with the key reversed: the `medium` is the head, and the
		// `maestro` took its vector when it could not have fitted beside it.
		{"the key puts the medium first, whose vector the maestro took", "c/repo", "a/repo", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			xl := oracleDemand(cfg, test.xlRepo, 21, tied, "xl")
			medium := oracleDemand(cfg, test.medRepo, 22, tied, "medium")
			// The observation lists the `medium` before the `xl`, as the sweep's did:
			// the oracle used to keep whichever came first.
			observation := admitting(oracleObservation(149, []domain.Demand{maestro, medium, oldest, xl}, live,
				domain.Resources{CPU: 2, MemoryMB: 9_216, Slots: 4}, nil), oldest, maestro)
			findings := crossPlatformInversionChecker(cfg)(&world{}, observation)
			if got := len(findings) > 0; got != test.reported {
				t.Fatalf("reported = %v, want %v: %v", got, test.reported, findings)
			}
		})
	}
}

// TestIssue404ScopeShareCountsAQueuedAheadAdmissionOnce is seed 296, tick 76 of
// the container arm (nightly 2026-10-18 to 2026-10-25, and seed 157 on the two
// nights before). The trace still reproduces at HEAD. Issue #429's new
// generator event moved seed 296 off it, which is what turned the arm green.
//
// `a/repo` holds two slots. Scope `c` queued a `large` and two `medium`s,
// scope `ops` an `xl`, a `large` and a `medium`, and the plan admitted both of
// `c`'s `medium`s. ADR 0057's key is the slot a demand's scope is asking for:
// what the scope runs plus what it has queued AHEAD. `c`'s second `medium` and
// `ops`'s `medium` each ask for their scope's third slot, so the rank is equal,
// age decides, and `c`'s is older. The fleet ordered them exactly so.
//
// The oracle counted `c`'s first `medium` twice: once in the band rank, where
// it is queued ahead, and once more as an admission that "spends" the next
// slot. So it read the second `medium` as asking for a fourth slot and called
// the tick a pass-over. An admission the rank does not already hold still
// spends a slot, and the remaining cases keep that half of the rule firing.
func TestIssue404ScopeShareCountsAQueuedAheadAdmissionOnce(t *testing.T) {
	t.Parallel()
	cfg := containerNodeWorld()
	live := []domain.Instance{
		oracleInstance(cfg, "trf-medium-1", "a/repo", "medium"),
		oracleInstance(cfg, "trf-xl-1", "a/repo", "xl"),
	}
	cLarge := oracleDemand(cfg, "c/repo", 20, 20*time.Minute+30*time.Second, "large")
	cFirst := oracleDemand(cfg, "c/repo", 21, 19*time.Minute, "medium")
	cSecond := oracleDemand(cfg, "c/repo", 24, 17*time.Minute, "medium")
	opsXL := oracleDemand(cfg, simControlPlaneRepo, 22, 19*time.Minute, "xl")
	opsLarge := oracleDemand(cfg, simControlPlaneRepo, 25, 17*time.Minute, "large")
	opsMedium := oracleDemand(cfg, simControlPlaneRepo, 30, 10*time.Minute+30*time.Second, "medium")
	cYoung := oracleDemand(cfg, "c/repo", 39, 3*time.Minute+30*time.Second, "medium")
	cYounger := oracleDemand(cfg, "c/repo", 40, 2*time.Minute, "medium")

	for _, test := range []struct {
		name     string
		queued   []domain.Demand
		admitted []domain.Demand
		reported bool
	}{
		{"equal asks are decided by age", []domain.Demand{cLarge, cFirst, cSecond, opsXL, opsLarge, opsMedium},
			[]domain.Demand{cFirst, cSecond}, false},
		// `ops` asks for its FIRST slot; `c`'s second `medium` asks for its third.
		{"a scope asking for its first slot is passed over", []domain.Demand{cLarge, cFirst, cSecond, opsMedium},
			[]domain.Demand{cFirst, cSecond}, true},
		// Young admissions carry no band rank, so the slots they spend are the
		// only thing that tells the second from the first.
		{"young admissions still spend their scope's slots", []domain.Demand{opsMedium, cYoung, cYounger},
			[]domain.Demand{cYoung, cYounger}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			observation := admitting(oracleObservation(76, test.queued, live,
				domain.Resources{CPU: 6, MemoryMB: 18_432, Slots: 4}, nil), test.admitted...)
			check := scopeShareChecker(cfg)
			var findings []finding
			for tick := range cfg.ScopeShareN + 1 {
				observation.Tick = tick
				findings = append(findings, check(&world{}, observation)...)
			}
			if got := len(findings) > 0; got != test.reported {
				t.Fatalf("reported = %v, want %v: %v", got, test.reported, findings)
			}
		})
	}
}

// TestIssue404AReservedHeadTakingItsVectorIsNotAPassOver is seed 498, tick
// 148 of the container arm, the one seed a 500-seed sweep of every arm still
// found once the two oracle defects above were fixed.
//
// `b`'s `xl` (1h6m30s) reserved the vector at tick 136, when neither `b` nor
// `a` held a slot, so ADR 0057's key agreed with age. At tick 145 backfill
// behind the reservation admitted a `small` of `b`'s in the residual. At tick
// 148 the vector freed and the reserved head took it, over `a`'s `xl` (1h3m)
// while `b` held that one `small` and `a` held nothing. Property (s) called
// that its third pass-over of `a`.
//
// It is the reservation contract working. ADR 0017: the reserved head wins the
// first vector large enough for it, and "backfill may use stranded capacity; it
// may never take the head's turn". Re-ranking the head by the slot its own
// backfill took would be exactly that. ADR 0057 leaves the reserved head it
// mints as it was and amends the no-jump clause only so a CAP-held head lends
// its vector (ADR 0038). This is a property/oracle defect. The second case keeps
// the property firing on the same admission when no reservation was carried.
func TestIssue404AReservedHeadTakingItsVectorIsNotAPassOver(t *testing.T) {
	t.Parallel()
	cfg := containerNodeWorld()
	// One pass-over is enough to report, so the tick under test is the only one.
	cfg.ScopeShareN = 0
	live := []domain.Instance{oracleInstance(cfg, "trf-small-1", "b/repo", "small")}
	reserved := oracleDemand(cfg, "b/repo", 20, 66*time.Minute+30*time.Second, "xl")
	waiting := oracleDemand(cfg, "a/repo", 27, 63*time.Minute, "xl")
	queued := []domain.Demand{reserved, waiting}
	available := domain.Resources{CPU: 7, MemoryMB: 23_552, Slots: 4}

	for _, test := range []struct {
		name     string
		carried  *domain.Reservation
		reported bool
	}{
		{"the reserved head takes the vector it reserved", reservationOf(cfg, reserved), false},
		{"an unreserved demand of a scope holding a slot passes over one holding none", nil, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			check := scopeShareChecker(cfg)
			// The tick before: the vector is still held and nothing is admitted.
			findings := check(&world{}, oracleObservation(147, queued, live, domain.Resources{CPU: 5, MemoryMB: 11_264, Slots: 4}, test.carried))
			findings = append(findings, check(&world{}, admitting(oracleObservation(148, queued, live, available, nil), reserved))...)
			if got := len(findings) > 0; got != test.reported {
				t.Fatalf("reported = %v, want %v: %v", got, test.reported, findings)
			}
		})
	}
}

// TestIssue404SeedFourNineEightIsClean pins the generated trace that reached the
// tick above.
func TestIssue404SeedFourNineEightIsClean(t *testing.T) {
	t.Parallel()
	cfg := containerNodeWorld()
	if findings := runTrace(t, cfg, generateTrace(498, 320, cfg)); len(findings) > 0 {
		t.Fatalf("seed 498 of the container arm is red again: %s", findings[0])
	}
}
