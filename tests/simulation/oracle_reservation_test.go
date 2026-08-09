package simulation_test

import (
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

// The tests in this file pin the FEASIBILITY ORACLE properties (a) and (b) rest
// on, in the one place a reservation makes "definitely admissible" a question
// about a written rule rather than about arithmetic.
//
// They are oracle tests, not scheduler tests: each one builds a tick
// observation directly and asks the oracle what it concludes from it. That is
// deliberate. A scheduler test cannot distinguish an oracle that is right from
// one that is merely agreeing with the code it is checking, and both directions
// of this refinement have to be pinned -- the tick the oracle must stop
// reporting (issue #216) and the ticks it must go on reporting (issue #125, the
// 2026-07-25 incident of ADR 0017).

// oracleNow is the virtual instant every observation in this file is taken at.
var oracleNow = time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)

// oracleDemand builds one queued demand of the given age, in the profile
// vocabulary of the world under test.
func oracleDemand(cfg worldConfig, repo string, id int64, age time.Duration, profile domain.ProfileID) domain.Demand {
	definition := cfg.Scheduler.Profiles[profile]
	return domain.Demand{Key: domain.DemandKey{Repo: repo, RunID: 1000 + id, Attempt: 1, JobID: 500_000 + id},
		CreatedAt: oracleNow.Add(-age), Profile: profile, Route: definition.Route,
		Platform: definition.Platform, Event: domain.EventPullRequest}
}

// oracleInstance builds one live instance that consumes the host and holds a
// repository slot, which is what makes it visible to every term of the oracle.
func oracleInstance(cfg worldConfig, id, repo string, profile domain.ProfileID) domain.Instance {
	definition := cfg.Scheduler.Profiles[profile]
	return domain.Instance{ID: id, Repo: repo,
		Demand:   domain.DemandKey{Repo: repo, RunID: 1, Attempt: 1, JobID: 1},
		Platform: definition.Platform, Profile: profile, Route: definition.Route,
		Resources: definition.Resources, State: domain.InstanceAssigned, Power: domain.InstancePowerRunning}
}

// oracleObservation is a ready, usable tick that planned nothing: the shape
// every liveness question is asked about.
func oracleObservation(tick int, demands []domain.Demand, instances []domain.Instance,
	available domain.Resources, reservation *domain.Reservation) tickObservation {
	return tickObservation{Tick: tick, Now: oracleNow,
		Plan:            scheduler.Plan{ID: "oracle-plan", Status: scheduler.PlanReady, Next: scheduler.State{Reservation: reservation}},
		Applied:         true,
		Demands:         demands,
		Instances:       instances,
		InstancesUsable: true,
		Host:            domain.Host{Available: available, Capacity: domain.Resources{CPU: 12, MemoryMB: 30_720, Slots: 4}},
		HostUsable:      true,
	}
}

// reservationOf names the vector a plan withholds for its aged head, built from
// the CONFIGURED profile so a test states the reservation the same way the
// oracle reads it.
func reservationOf(cfg worldConfig, head domain.Demand) *domain.Reservation {
	return &domain.Reservation{Demand: head.Key, Profile: head.Profile,
		Resources: cfg.Scheduler.Profiles[head.Profile].Resources, Since: oracleNow.Add(-time.Hour)}
}

// feasibleKeys reduces the oracle's answer to the keys it called admissible.
func feasibleKeys(cfg worldConfig, observation tickObservation) []domain.DemandKey {
	var keys []domain.DemandKey
	for _, demand := range feasibleDemands(cfg, observation) {
		keys = append(keys, demand.Key)
	}
	return keys
}

// runLiveness feeds one observation to property (a) for K+1 consecutive ticks
// and returns the findings, which is what the checker's own counter measures.
func runLiveness(cfg worldConfig, observation tickObservation) []finding {
	check := livenessChecker(cfg)
	w := &world{}
	var findings []finding
	for tick := range cfg.LivenessK + 1 {
		observation.Tick = tick
		findings = append(findings, check(w, observation)...)
	}
	return findings
}

// TestFeasibilityWithholdsAFittingReservedHeadsVector is issue #216: the seed
// 92 tick of the container-node arm, judged by the oracle alone.
//
//	go test ./tests/simulation -run TestSimFuzzContainerNode -seeds=1 -seed-base=92 -sim-ticks=320
//	tick 188: liveness_wedge: no admission for 12 ticks while b/repo/1011/1/500011 was feasible
//
// Three live instances hold 4 CPU / 8192 MB / 3 slots of a 12 CPU / 30720 MB /
// 4 slot ceiling, and the host reports 7 CPU available, so the oracle's own free
// envelope is {7 CPU, 22528 MB, 1 slot}. The aged head is an `ops/fleet` xl that
// FITS that envelope; it is held out of admission by its repository cap, which
// `ops/fleet`'s two live smalls have already filled -- ADR 0029's own guard case.
//
// ADR 0029 condition 1 therefore withholds the head's whole vector, and
// `safeBackfill` plans inside {1 CPU, 10240 MB, 0 slots}. A `medium` needs two
// cores and a slot. Calling it "definitely admissible" reports the aged-FIFO
// guarantee as a defect: the plan is correct and the oracle was wrong.
func TestFeasibilityWithholdsAFittingReservedHeadsVector(t *testing.T) {
	t.Parallel()
	cfg := containerNodeWorld()
	live := []domain.Instance{
		oracleInstance(cfg, "trf-medium-1", "b/repo", "medium"),
		oracleInstance(cfg, "trf-small-1", simControlPlaneRepo, "small"),
		oracleInstance(cfg, "trf-small-2", simControlPlaneRepo, "small"),
	}
	head := oracleDemand(cfg, simControlPlaneRepo, 8, 24*time.Minute+30*time.Second, "xl")
	waiting := oracleDemand(cfg, "b/repo", 11, 15*time.Minute+30*time.Second, "medium")
	observation := oracleObservation(188, []domain.Demand{head, waiting}, live,
		domain.Resources{CPU: 7, MemoryMB: 22_528, Slots: 4}, reservationOf(cfg, head))

	if keys := feasibleKeys(cfg, observation); len(keys) != 0 {
		t.Fatalf("a demand that fits only INSIDE the reserved head's vector is not definitely admissible: %v", keys)
	}
	if findings := runLiveness(cfg, observation); len(findings) > 0 {
		t.Fatalf("property (a) must not report a tick that is correctly holding the head's vector: %s", findings[0])
	}
}

// TestFeasibilityKeepsTheResidualBehindAnInfeasibleHead is the direction that
// must NOT move: the 2026-07-25 incident of ADR 0017, which is issue #125's
// production bug and the reason property (a) exists.
//
// The head does not fit the free envelope at all. Nothing is charged for it --
// it is blocked by live instances rather than by this tick's admission, so it
// cannot start until they release, whatever backfill does -- and admission
// proceeds in the FULL residual. Work that fits and is refused here is a wedge,
// and the oracle must go on saying so.
func TestFeasibilityKeepsTheResidualBehindAnInfeasibleHead(t *testing.T) {
	t.Parallel()
	cfg := defaultWorld()
	// A macOS builder wedged in draining holds 6 CPU / 12288 MB of a 10 CPU /
	// 22528 MB ceiling, so the free envelope is {4 CPU, 10240 MB}.
	live := []domain.Instance{oracleInstance(cfg, "trf-builder-1", "a/repo", "builder")}
	// The aged head is an xl: six cores against four free. It reserves and cannot
	// fit, which is ADR 0017's case exactly.
	head := oracleDemand(cfg, "a/repo", 21, time.Hour, "xl")
	waiting := oracleDemand(cfg, "b/repo", 22, 30*time.Minute, "medium")
	observation := oracleObservation(97, []domain.Demand{head, waiting}, live,
		domain.Resources{CPU: 4, MemoryMB: 10_240, Slots: 4}, reservationOf(cfg, head))

	keys := feasibleKeys(cfg, observation)
	if len(keys) != 1 || keys[0] != waiting.Key {
		t.Fatalf("an infeasible reservation charges nothing: the residual is still definitely admissible, got %v", keys)
	}
	findings := runLiveness(cfg, observation)
	if len(findings) != 1 || findings[0].Kind != findingWedge {
		t.Fatalf("property (a) must still report a residual refused behind an infeasible head: %v", findings)
	}
}

// TestFeasibilityStillReportsWorkRefusedBesideAFittingHead is the other half of
// the same guard, and the one that keeps the refinement from becoming a blanket
// exemption for "a reservation is held".
//
// The head fits, so its vector is withheld -- and the envelope is wide enough
// that a `medium` fits what is LEFT. ADR 0029 condition 1 permits that
// admission, so refusing it for K ticks is a wedge with a reservation held, and
// property (a) must report it.
func TestFeasibilityStillReportsWorkRefusedBesideAFittingHead(t *testing.T) {
	t.Parallel()
	cfg := containerNodeWorld()
	live := []domain.Instance{oracleInstance(cfg, "trf-large-1", "b/repo", "large")}
	// Ceiling {12, 30720, 4} less the live large leaves {8, 22528, 3}; the host
	// reports eight cores, so free is {8 CPU, 22528 MB, 3 slots}. The aged xl head
	// fits it and is withheld, leaving {2 CPU, 10240 MB, 2 slots} -- room for the
	// medium.
	head := oracleDemand(cfg, "a/repo", 31, time.Hour, "xl")
	waiting := oracleDemand(cfg, "b/repo", 32, 30*time.Minute, "medium")
	observation := oracleObservation(50, []domain.Demand{head, waiting}, live,
		domain.Resources{CPU: 8, MemoryMB: 22_528, Slots: 4}, reservationOf(cfg, head))

	keys := feasibleKeys(cfg, observation)
	if len(keys) != 2 {
		t.Fatalf("a head that fits withholds its own vector and lends the rest: %v", keys)
	}
	findings := runLiveness(cfg, observation)
	if len(findings) != 1 || findings[0].Kind != findingWedge {
		t.Fatalf("property (a) must still report work refused in the remainder a fitting head leaves: %v", findings)
	}
}

// TestFeasibilityJudgesTheReservedHeadAgainstTheWholeEnvelope pins the one
// demand the subtraction may never be applied to.
//
// The head is entitled to the vector, so it is judged against the envelope that
// vector comes out of. Judging it against `free - itself` would make every
// reserved head permanently infeasible to the oracle, and a fleet that reserved
// for a head it could admit and then admitted nothing would become invisible --
// which is the wedge shape of issue #125 wearing a reservation.
func TestFeasibilityJudgesTheReservedHeadAgainstTheWholeEnvelope(t *testing.T) {
	t.Parallel()
	cfg := containerNodeWorld()
	head := oracleDemand(cfg, "a/repo", 41, time.Hour, "xl")
	observation := oracleObservation(60, []domain.Demand{head}, nil,
		domain.Resources{CPU: 12, MemoryMB: 30_720, Slots: 4}, reservationOf(cfg, head))

	keys := feasibleKeys(cfg, observation)
	if len(keys) != 1 || keys[0] != head.Key {
		t.Fatalf("the reserved head is judged against the envelope its own vector comes out of: %v", keys)
	}
	findings := runLiveness(cfg, observation)
	if len(findings) != 1 || findings[0].Kind != findingWedge {
		t.Fatalf("a reservation held for a head the oracle can admit, admitting nothing, is still a wedge: %v", findings)
	}
}
