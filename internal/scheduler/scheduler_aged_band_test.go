package scheduler

import (
	"reflect"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// FINDING 3 of ADR 0031, found by the simulation harness (#128).
//
// ADR 0004 states the whole scheduling order in three lines:
//
//  1. aged work in global FIFO order,
//  2. young control-plane work,
//  3. young standard work,
//
// and says the order "is shared by platform arbitration, Linux exact admission
// and safe backfill, macOS profile arbitration, and macOS admission".
//
// `priorityOrder` honours it. `exactSelect` then RE-RANKED the very same
// candidates: `schedulingBand` mapped control-plane to band 0 and standard to
// band 1, aging was not a band at all, and `betterAdmission` compares band
// coverage before anything else. So a young control-plane demand beat an aged
// standard one — rule 2 ahead of rule 1 — and the aged demand's global FIFO
// position, the one guarantee that bounds starvation, was silently spent.
//
// The repair is to give aging its own band ahead of both young lanes, so the
// band vector IS ADR 0004's list rather than a second, contradictory policy.

// agedBandConfig is the default topology plus an `xl` tier that cannot fit the
// four free cores (which is what makes an aged head reserve and open the
// residual to backfill) and a control-plane repository.
func agedBandConfig() Config {
	cfg := testConfig()
	cfg.LinuxCapacity = domain.Resources{CPU: 4, MemoryMB: 8_192, Slots: 4}
	cfg.RepoCaps = map[string]int{"a/repo": 4, "b/repo": 4, "cp/repo": 4}
	cfg.RepoSchedulingClasses = map[string]domain.SchedulingClass{"cp/repo": domain.SchedulingControlPlane}
	cfg.Profiles["xl"] = domain.Profile{ID: "xl", Platform: domain.PlatformLinux, Route: "tiered",
		Resources: domain.Resources{CPU: 6, MemoryMB: 12_288, Slots: 1}}
	return cfg
}

func agedBandInput(cfg Config, demands []domain.Demand, instances []domain.Instance) Input {
	return Input{Now: testNow, Config: cfg, Demands: domain.Fresh(demands, testNow),
		Instances: domain.Fresh(instances, testNow),
		Host:      domain.Fresh(domain.Host{Available: cfg.LinuxCapacity}, testNow)}
}

// TestExactSelectionRanksAgedWorkAboveEveryYoungLane is the unit-level statement
// of ADR 0004's order over `exactSelect`'s band vector. Every case offers more
// feasible work than the envelope holds, so the selection is a pure priority
// decision, and every candidate list is given in the order `priorityOrder`
// produces it.
func TestExactSelectionRanksAgedWorkAboveEveryYoungLane(t *testing.T) {
	cfg := agedBandConfig()
	// Exactly one `large` (4 CPU / 8192 MB) fits, so each case admits one demand.
	free := domain.Resources{CPU: 4, MemoryMB: 8_192, Slots: 4}
	for _, test := range []struct {
		name       string
		rule       string
		candidates []domain.Demand
		want       []int64
	}{
		{
			name: "aged standard work outranks young control-plane work",
			rule: "ADR 0004 rule 1 before rule 2",
			candidates: []domain.Demand{
				profileDemand(cfg, "b/repo", 1, 30*time.Minute, "large"),
				profileDemand(cfg, "cp/repo", 2, time.Minute, "large"),
			},
			want: []int64{1},
		},
		{
			name: "young control-plane work outranks young standard work",
			rule: "ADR 0004 rule 2 before rule 3",
			candidates: []domain.Demand{
				profileDemand(cfg, "b/repo", 3, time.Minute, "large"),
				profileDemand(cfg, "cp/repo", 4, time.Minute, "large"),
			},
			want: []int64{4},
		},
		{
			name: "aged work is one band in global FIFO order",
			rule: "ADR 0004 rule 1 is a class-blind FIFO",
			candidates: []domain.Demand{
				profileDemand(cfg, "b/repo", 5, 60*time.Minute, "large"),
				profileDemand(cfg, "cp/repo", 6, 30*time.Minute, "large"),
			},
			want: []int64{5},
		},
		{
			name: "aged control-plane work outranks young standard work",
			rule: "ADR 0004 rule 1 before rule 3",
			candidates: []domain.Demand{
				profileDemand(cfg, "b/repo", 7, time.Minute, "large"),
				profileDemand(cfg, "cp/repo", 8, 30*time.Minute, "large"),
			},
			want: []int64{8},
		},
		{
			// Seed 88 of the ADR 0031 corpus. Both selections admit ONE aged
			// demand, so band counts tie and the young control-plane band used to
			// break the tie — which decides WHICH aged demand is served, and that
			// is rule 1's business, not rule 2's.
			name: "a young lane may not choose which aged demand is served",
			rule: "ADR 0004 rule 1 is settled before rule 2 is read",
			candidates: []domain.Demand{
				profileDemand(cfg, "b/repo", 9, 37*time.Minute, "large"),
				profileDemand(cfg, "b/repo", 10, 6*time.Minute, "small"),
				profileDemand(cfg, "cp/repo", 11, time.Minute, "medium"),
			},
			want: []int64{9},
		},
		{
			// The same tie-break must stay work-conserving: once the oldest aged
			// demand has its vector, whatever still fits is still admitted.
			name: "the young lanes still take what the aged band leaves",
			rule: "ADR 0004 orders admission, it does not idle capacity",
			candidates: []domain.Demand{
				profileDemand(cfg, "b/repo", 12, 37*time.Minute, "medium"),
				profileDemand(cfg, "cp/repo", 13, time.Minute, "medium"),
			},
			want: []int64{12, 13},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			selected := exactSelect(testNow, test.candidates, free, nil, cfg)
			got := make([]int64, 0, len(selected))
			for _, demand := range selected {
				got = append(got, demand.Key.JobID)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("%s: selected jobs %v, want %v", test.rule, got, test.want)
			}
		})
	}
}

// TestAgedWorkKeepsItsPlaceInEveryExactAdmissionSeam is the same rule stated
// through `PlanTick`, once per pass that reaches `exactSelect` with aged and
// young candidates in one slice. `planLinux`'s own young path cannot mix them —
// it returns inside the aged branch — so the mixing seams are safe backfill
// behind a reserved head and the bounded drain backfill of a macOS handoff.
func TestAgedWorkKeepsItsPlaceInEveryExactAdmissionSeam(t *testing.T) {
	cfg := agedBandConfig()
	for _, test := range []struct {
		name      string
		seam      string
		demands   []domain.Demand
		instances []domain.Instance
		want      []int64
	}{
		{
			name: "safe backfill behind an infeasible reserved head",
			seam: "safeBackfill",
			demands: []domain.Demand{
				profileDemand(cfg, "a/repo", 1, 60*time.Minute, "xl"),
				profileDemand(cfg, "b/repo", 2, 30*time.Minute, "large"),
				profileDemand(cfg, "cp/repo", 3, time.Minute, "large"),
			},
			want: []int64{2},
		},
		{
			name: "the bounded drain backfill of a macOS handoff",
			seam: "boundedDrainBackfill",
			demands: []domain.Demand{
				profileDemand(cfg, "mac-a", 4, 60*time.Minute, "builder"),
				profileDemand(cfg, "b/repo", 5, 30*time.Minute, "small"),
				profileDemand(cfg, "cp/repo", 6, time.Minute, "small"),
			},
			instances: []domain.Instance{liveInstance(cfg, "linux-busy", "a/repo", "medium")},
			want:      []int64{5},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan := PlanTick(agedBandInput(cfg, test.demands, test.instances))
			if got := spawnedJobIDs(plan); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("%s admitted jobs %v, want %v\n%#v", test.seam, got, test.want, plan.Operations)
			}
		})
	}
}

// TestReservedHeadSurvivesAgedBackfill guards the other half of the contract:
// giving aged work its band back must not cost the reserved head its
// reservation, which is the ordering that ADR 0017 protects it by.
func TestReservedHeadSurvivesAgedBackfill(t *testing.T) {
	cfg := agedBandConfig()
	head := profileDemand(cfg, "a/repo", 1, 60*time.Minute, "xl")
	plan := PlanTick(agedBandInput(cfg, []domain.Demand{head,
		profileDemand(cfg, "b/repo", 2, 30*time.Minute, "large"),
		profileDemand(cfg, "cp/repo", 3, time.Minute, "large")}, nil))
	if plan.Next.Reservation == nil || plan.Next.Reservation.Demand != head.Key {
		t.Fatalf("the aged head lost its reservation: %#v", plan.Next.Reservation)
	}
}

// TestReservationYieldsToOlderWorkThatReentersTheQueue is issue #208's
// bounded-starvation violation, seed 55 of the simulator's container-node arm,
// reduced to one tick.
//
// A reservation is held for the oldest aged demand that does not fit, and ADR
// 0017 and ADR 0029 both promise it is re-checked FIRST on every later tick so it
// wins the first vector large enough for it. That promise is made against work
// that is YOUNGER than the head. The plannable queue, however, is not frozen
// while the reservation is held: ADR 0027 keeps a demand out of it for as long as
// a live instance incarnates it, and a recovery drain puts that demand back --
// with the GitHub queue time every aging rule here measures from.
//
// The tick below is that moment. The oldest demand's own runner has just been
// reaped for a stalled assignment, so it re-enters the queue ahead of the reserved
// head, and only one `xl` fits in what the reaped VM released. `planLinux`
// returned straight out of its reservation branch without consulting the queue it
// had been handed, so the YOUNGER demand took the vector -- ADR 0004's rule 1
// broken by the mechanism that exists to keep it. In the simulator that repeated
// on every recovery deadline: four pass-overs of the same demand, fifty ticks
// apart, for as long as the run lasted.
func TestReservationYieldsToOlderWorkThatReentersTheQueue(t *testing.T) {
	cfg := agedBandConfig()
	cfg.LinuxCapacity = domain.Resources{CPU: 12, MemoryMB: 30_720, Slots: 4}
	older := profileDemand(cfg, "a/repo", 7, 103*time.Minute+30*time.Second, "xl")
	younger := profileDemand(cfg, "a/repo", 18, 70*time.Minute+30*time.Second, "xl")
	in := agedBandInput(cfg, []domain.Demand{older, younger},
		[]domain.Instance{liveInstance(cfg, "trf-small-live", "a/repo", "small")})
	in.Host = domain.Fresh(domain.Host{Available: domain.Resources{CPU: 11, MemoryMB: 28_672, Slots: 4}}, testNow)
	in.Prior = State{Reservation: &domain.Reservation{Demand: younger.Key, Profile: "xl",
		Resources: cfg.Profiles["xl"].Resources, Since: testNow.Add(-18 * time.Minute)}}

	plan := PlanTick(in)
	if want := []domain.DemandKey{older.Key}; !reflect.DeepEqual(spawnedKeys(plan), want) {
		t.Fatalf("spawns = %v, want the older aged demand %v", spawnedKeys(plan), want)
	}
	// The demand that lost the reservation is first in line behind the work that
	// displaced it, which is what makes this a re-derivation rather than a loss.
	if plan.Next.Reservation == nil || plan.Next.Reservation.Demand != younger.Key {
		t.Fatalf("the displaced head must hold the next reservation: %#v", plan.Next.Reservation)
	}
}

// TestReservationSurvivesYoungerWorkEnteringTheQueue is the guard on the same
// seam: the reservation is re-derived only against work that outranks it. A
// demand that enters the queue YOUNGER than the reserved head -- however feasible
// -- must not displace it, or the reservation would protect nothing at all.
func TestReservationSurvivesYoungerWorkEnteringTheQueue(t *testing.T) {
	cfg := agedBandConfig()
	cfg.LinuxCapacity = domain.Resources{CPU: 12, MemoryMB: 30_720, Slots: 4}
	head := profileDemand(cfg, "a/repo", 7, 103*time.Minute+30*time.Second, "xl")
	newcomer := profileDemand(cfg, "b/repo", 18, 110*time.Minute, "small")
	newcomer.CreatedAt = testNow.Add(-90 * time.Minute)
	in := agedBandInput(cfg, []domain.Demand{head, newcomer},
		[]domain.Instance{liveInstance(cfg, "trf-xl-live", "b/repo", "xl")})
	// The measured residual leaves five cores, so the reserved `xl` head does not
	// fit and the newcomer does.
	in.Host = domain.Fresh(domain.Host{Available: domain.Resources{CPU: 5, MemoryMB: 18_432, Slots: 4}}, testNow)
	in.Prior = State{Reservation: &domain.Reservation{Demand: head.Key, Profile: "xl",
		Resources: cfg.Profiles["xl"].Resources, Since: testNow.Add(-18 * time.Minute)}}

	plan := PlanTick(in)
	if plan.Next.Reservation == nil || plan.Next.Reservation.Demand != head.Key {
		t.Fatalf("younger work displaced a held reservation: %#v", plan.Next.Reservation)
	}
	if len(spawnedKeys(plan)) != 1 || spawnedKeys(plan)[0] != newcomer.Key {
		t.Fatalf("the head does not fit; backfill must serve the newcomer alone: %#v", plan.Operations)
	}
}
