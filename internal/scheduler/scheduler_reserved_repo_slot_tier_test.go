package scheduler

import (
	"reflect"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// The tick issue #255 reduces to, transcribed from the tiered arm's seed 82 at
// tick 140. Every value below is the one the simulated fleet held, and the plan
// this input produces is `plan-3a13529ee7cf2705e5b79331` -- the identical plan
// the sweep reported -- so this is the whole decision, not a re-staging of it.
//
// Three demands, one free maestro slot:
//
//	ops/fleet/1008  xl       59m0s   effective tier 11  <- the reserved aged head
//	ops/fleet/1013  maestro  43m30s  effective tier 8
//	b/repo/1021     maestro  7m30s   effective tier 2   (declared `release`)
//
// `ops/fleet` has a repository cap of 2 and one live instance, so the head's own
// future slot is the only one left. Under ADR 0030 that is the `slack <= 0`
// case -- "the head is waiting for the last free slot ... nothing from its
// repository may be admitted" -- so the tier-8 demand is not a candidate at all,
// and the residual goes to the only demand that may bid for it.
func reservedRepoSlotConfig() Config {
	return Config{
		LinuxCapacity:   domain.Resources{CPU: 10, MemoryMB: 24_576, Slots: 4},
		FairnessAge:     5 * time.Minute,
		AssignedTimeout: 15 * time.Minute,
		RepoCaps:        map[string]int{"b/repo": 4, "ops/fleet": 2},
		RepoSchedulingClasses: map[string]domain.SchedulingClass{
			"ops/fleet": domain.SchedulingControlPlane,
		},
		Profiles: map[domain.ProfileID]domain.Profile{
			"small":   {ID: "small", Platform: domain.PlatformLinux, Route: "linux-small", Resources: domain.Resources{CPU: 1, MemoryMB: 2_048, Slots: 1}},
			"xl":      {ID: "xl", Platform: domain.PlatformLinux, Route: "linux-xl", Resources: domain.Resources{CPU: 6, MemoryMB: 12_288, Slots: 1}},
			"maestro": {ID: "maestro", Platform: domain.PlatformMacOS, Route: "macos-maestro", Resources: domain.Resources{CPU: 4, MemoryMB: 7_168, Slots: 1}, MaxActive: 2},
		},
		MixedPlatformAdmission: true,
		MixedProfileCohorts:    true,
		ElasticHostEnvelope:    true,
		PriorityEscalation:     5 * time.Minute,
	}
}

var reservedRepoSlotNow = time.Date(2026, 8, 3, 10, 10, 0, 0, time.UTC)

func reservedRepoSlotInput(config Config) Input {
	now := reservedRepoSlotNow
	head := domain.Demand{Key: domain.DemandKey{Repo: "ops/fleet", RunID: 1008, Attempt: 1, JobID: 500008},
		CreatedAt: now.Add(-59 * time.Minute), Profile: "xl", Route: "linux-xl",
		Platform: domain.PlatformLinux, Event: domain.EventPush, RunStatus: domain.RunQueued}
	waiting := domain.Demand{Key: domain.DemandKey{Repo: "ops/fleet", RunID: 1013, Attempt: 1, JobID: 500013},
		CreatedAt: now.Add(-43*time.Minute - 30*time.Second), Profile: "maestro", Route: "macos-maestro",
		Platform: domain.PlatformMacOS, Event: domain.EventPush, RunStatus: domain.RunQueued}
	release := domain.Demand{Key: domain.DemandKey{Repo: "b/repo", RunID: 1021, Attempt: 1, JobID: 500021},
		CreatedAt: now.Add(-7*time.Minute - 30*time.Second), Profile: "maestro", Route: "macos-maestro",
		Platform: domain.PlatformMacOS, Event: domain.EventPush, RunStatus: domain.RunQueued,
		Priority: domain.Priority{Tier: "release", Rank: 1}}

	return Input{Now: now, Config: config,
		Demands:   domain.Fresh([]domain.Demand{waiting, release, head}, now),
		Instances: domain.Fresh(reservedRepoSlotInstances(now), now),
		Host: domain.Fresh(domain.Host{
			Available: domain.Resources{CPU: 5, MemoryMB: 13_312, Slots: 4},
			Capacity:  domain.Resources{CPU: 10, MemoryMB: 22_528, Slots: 4},
			Pressure: domain.HostPressure{AvailableMemoryMB: 15_360, FreeDiskGB: 400, CPUIdlePercent: 50,
				LoadAverage: 5, SwapOutRateObserved: true, AdmissionAllowed: true, AdmissionReason: "capacity available"},
		}, now),
		Prior: State{
			Reservation: &domain.Reservation{Demand: head.Key, Profile: "xl",
				Resources: domain.Resources{CPU: 6, MemoryMB: 12_288, Slots: 1}, Since: now.Add(-9 * time.Minute)},
			DRRCursor:    "ops/fleet",
			LinuxHandoff: &LinuxHandoff{Demand: head.Key, Since: now.Add(-3 * time.Minute)},
		}}
}

func reservedRepoSlotInstances(now time.Time) []domain.Instance {
	return []domain.Instance{
		{ID: "trf-maestro-174e2ba7a7bc7da5", Repo: "b/repo",
			Demand:   domain.DemandKey{Repo: "b/repo", RunID: 1012, Attempt: 1, JobID: 500012},
			Platform: domain.PlatformMacOS, Profile: "maestro", Route: "macos-maestro",
			Resources: domain.Resources{CPU: 4, MemoryMB: 7_168, Slots: 1},
			State:     domain.InstanceRunning, Power: domain.InstancePowerRunning, RunningSince: now.Add(-90 * time.Second)},
		{ID: "trf-small-723a8bb25ac863b5", Repo: "ops/fleet",
			Demand:   domain.DemandKey{Repo: "ops/fleet", RunID: 1023, Attempt: 1, JobID: 500023},
			Platform: domain.PlatformLinux, Profile: "small", Route: "linux-small",
			Resources: domain.Resources{CPU: 1, MemoryMB: 2_048, Slots: 1},
			State:     domain.InstanceRunning, Power: domain.InstancePowerRunning, RunningSince: now.Add(-150 * time.Second)},
	}
}

// TestTheReservedHeadsLastRepositorySlotOutranksATierBelowIt is issue #255's
// finding, stated as the decision it actually is.
//
// The tier-8 `ops/fleet` maestro is left waiting and a tier-2 `b/repo` maestro
// takes the identical slot -- and that is ADR 0030 working, not ADR 0037 being
// ignored. The demand left behind shares the reserved head's repository, and
// that repository's last free slot is the head's own. The head outranks it on
// BOTH keys the aged band is ordered by (effective tier 11 against 8, and 59
// minutes against 43m30s), so the pass-over serves the tier order rather than
// inverting it: what waits is the tier-8 demand, and what it waits for is a
// tier-11 one.
//
// Refusing the tier-2 demand as well would not help the tier-8 demand by a
// single tick -- it is not eligible for the slot on any tier -- and it would
// hold the residual idle, which ADR 0004 forbids in as many words: "admission
// stays work-conserving: the order decides who is served first, never that
// capacity is held idle".
func TestTheReservedHeadsLastRepositorySlotOutranksATierBelowIt(t *testing.T) {
	in := reservedRepoSlotInput(reservedRepoSlotConfig())
	release := domain.DemandKey{Repo: "b/repo", RunID: 1021, Attempt: 1, JobID: 500021}

	plan := PlanTick(in)
	if got := spawnedKeys(plan); !reflect.DeepEqual(got, []domain.DemandKey{release}) {
		t.Fatalf("residual went to %#v, want the only demand ADR 0030 lets bid for it, %#v", got, release)
	}
	if plan.Next.Reservation == nil || plan.Next.Reservation.Demand.RunID != 1008 {
		t.Fatalf("the reserved head this slot is held for was dropped: %#v", plan.Next.Reservation)
	}
}

// TestATierTakesTheResidualAsSoonAsTheReservedHeadCanSpareTheSlot is the other
// half, and it is what makes the test above a statement about ADR 0030 rather
// than a licence to ignore a tier.
//
// One extra slot of repository cap is the ONLY difference: the head still holds
// its own, the vectors are identical, the ages are identical, and the tier-8
// demand takes the residual the moment it is allowed to bid for it. So the tier
// is decisive exactly when the reservation is not, which is ADR 0030's "slack >
// 0" case verbatim.
func TestATierTakesTheResidualAsSoonAsTheReservedHeadCanSpareTheSlot(t *testing.T) {
	config := reservedRepoSlotConfig()
	config.RepoCaps["ops/fleet"] = 3
	waiting := domain.DemandKey{Repo: "ops/fleet", RunID: 1013, Attempt: 1, JobID: 500013}

	if got := spawnedKeys(PlanTick(reservedRepoSlotInput(config))); !reflect.DeepEqual(got, []domain.DemandKey{waiting}) {
		t.Fatalf("residual went to %#v, want the higher tier %#v once the head can spare a slot", got, waiting)
	}
}
