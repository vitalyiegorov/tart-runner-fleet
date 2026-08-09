package replay_test

import (
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

// Replay of the 2026-08-09 remainder stall on the production Mac mini
// (Mac16,10: 10 cores, 24 GiB), captured live at ~18:10Z on
// v0.1.394+main.d9c8ed23f842 — the build that shipped ADR 0024's
// "the remainder pass chooses a target, not a cohort" amendment.
//
// Timeline. A Linux `xl` (6 CPU / 12288 MB) from `rnw-community/rnw-community`
// was busy. The aged global-FIFO head was a second `xl` from that same
// repository, infeasible beside it (6 + 6 > 10), so `planLinux` held its
// reservation and ADR 0029's vector condition correctly released the four-core
// residual. The repository cap is 4 with one instance live, so ADR 0030's slack
// (`4 - 1 - 1 = 2`) let that repository's own macOS work bid.
//
// Every documented condition was therefore satisfied, and three `maestro`
// demands (4 CPU / 7168 MB, queued 29 minutes) that fit the four free cores
// EXACTLY were still refused on every tick.
//
// The refusal was `remainderMacProfile`. With mixed cohorts it names the pass's
// single target as the highest-priority queued profile with `maxActive` room —
// and asks nothing about that profile's vector. A `builder` (6 CPU / 12288 MB)
// queued 38 minutes was older, had `maxActive` room (none live), and cannot fit
// a four-core residual. It won the target, `appendMacSpawns` refused it on the
// envelope, and `fillMacRemainder` returned having offered the maestros nothing.
//
// ADR 0024 rule 3 says the veto is the envelope, not identity, and its
// amendment says a profile that cannot be admitted ends its OWN turn rather
// than the pass. A vector the residual cannot hold is exactly such a profile:
// ADR 0017's rule says it waits on the live instances holding what it needs,
// never on this pass.
//
// This replay pins the temporal property across the window: the free vector is
// put to work exactly once, the reservation survives every tick with no drain
// planned, the oversized builder is never admitted into a residual too small
// for it, and the reserved head still takes the released vector FIRST.

const (
	oversizedTargetTicks    = 12
	oversizedTargetInterval = 5 * time.Minute
	oversizedTargetRepo     = "rnw-community/rnw-community"
	oversizedTargetMacRepo  = "budgie-at/budgie"
)

// oversizedTargetConfig is the live topology with the live flags: mixed
// platform admission and mixed profile cohorts both on, as production runs
// them, and the repository cap of 4 the head's repository actually carries.
func oversizedTargetConfig() scheduler.Config {
	config := reservedRemainderConfig(true)
	config.MixedProfileCohorts = true
	config.RepoCaps = map[string]int{oversizedTargetRepo: 4, oversizedTargetMacRepo: 4}
	return config
}

func oversizedTargetDemand(at time.Time, repo string, jobID int64, age time.Duration, profile domain.ProfileID) domain.Demand {
	configured := reservedRemainderProfiles()[profile]
	return domain.Demand{Key: domain.DemandKey{Repo: repo, RunID: 31325708527, Attempt: 1, JobID: jobID},
		CreatedAt: at.Add(-age), Profile: profile, Route: configured.Route,
		Platform: configured.Platform, Event: domain.EventPullRequest}
}

func TestOversizedRemainderTargetIncidentAdmitsTheProfileTheResidualHolds(t *testing.T) {
	// 18:10Z, the moment captured live; the blocking xl started at ~17:53Z.
	start := time.Date(2026, 8, 9, 18, 10, 0, 0, time.UTC)
	busyXL := reservedRemainderInstance("trf-xl-05bbe1c83f21fcd6", "xl", oversizedTargetRepo)

	head := oversizedTargetDemand(start, oversizedTargetRepo, 11, 59*time.Minute, "xl")
	builder := oversizedTargetDemand(start, oversizedTargetMacRepo, 12, 38*time.Minute, "builder")
	maestros := make([]domain.Demand, 0, 3)
	for i := 0; i < 3; i++ {
		maestros = append(maestros, oversizedTargetDemand(start, oversizedTargetRepo, int64(20+i),
			30*time.Minute-time.Duration(i)*time.Minute, "maestro"))
	}
	queue := append([]domain.Demand{head, builder}, maestros...)
	instances := []domain.Instance{busyXL}
	config := oversizedTargetConfig()

	// The control arm: no macOS work queued but the oversized builder. The
	// residual cannot hold it on any tick, so the correct plan admits nothing —
	// this is the shape the stall LOOKED like, and it is legitimate here.
	control := scheduler.State{}
	for tick := 0; tick < oversizedTargetTicks; tick++ {
		at := start.Add(time.Duration(tick) * oversizedTargetInterval)
		plan := scheduler.PlanTick(scheduler.Input{Now: at, Config: config,
			Demands:   domain.Fresh([]domain.Demand{head, builder}, at),
			Instances: domain.Fresh(instances, at),
			Host:      reservedRemainderHost(at), Prior: control})
		if plan.Status != scheduler.PlanReady {
			t.Fatalf("control tick %d status = %s, want ready", tick, plan.Status)
		}
		if got := spawnKeys(plan); len(got) != 0 {
			t.Fatalf("control tick %d spawns = %#v, want none: nothing queued fits four cores", tick, got)
		}
		control = plan.Next
	}

	// The live arm: the maestros are queued behind the older builder. Tick 1
	// must put the four free cores to work with the oldest demand the residual
	// can actually hold.
	first := scheduler.PlanTick(scheduler.Input{Now: start, Config: config,
		Demands: domain.Fresh(queue, start), Instances: domain.Fresh(instances, start),
		Host: reservedRemainderHost(start), Prior: scheduler.State{}})
	if got := spawnKeys(first); len(got) != 1 || got[0] != maestros[0].Key {
		t.Fatalf("tick 1 spawns = %#v, want exactly the oldest maestro", got)
	}
	if first.Next.Reservation == nil || first.Next.Reservation.Demand != head.Key {
		t.Fatalf("tick 1 reservation = %#v, want the aged xl head still reserved", first.Next.Reservation)
	}

	// Ticks 2..12: the admitted maestro is live and no longer advertised. The
	// host is full (6 + 4 = 10 cores), so nothing further may be admitted, the
	// oversized builder still does not fit, and the reservation must survive
	// with no drain planned.
	live := []domain.Instance{busyXL,
		reservedRemainderInstance("trf-maestro-6004d55706822bb8", "maestro", oversizedTargetRepo)}
	remaining := append([]domain.Demand{head, builder}, maestros[1:]...)
	state := first.Next
	for tick := 1; tick < oversizedTargetTicks; tick++ {
		at := start.Add(time.Duration(tick) * oversizedTargetInterval)
		plan := scheduler.PlanTick(scheduler.Input{Now: at, Config: config,
			Demands: domain.Fresh(remaining, at), Instances: domain.Fresh(live, at),
			Host: reservedRemainderHost(at), Prior: state})
		if plan.Status != scheduler.PlanReady {
			t.Fatalf("tick %d status = %s, want ready", tick+1, plan.Status)
		}
		if got := spawnKeys(plan); len(got) != 0 {
			t.Fatalf("tick %d spawns = %#v, want none: the host is full", tick+1, got)
		}
		for _, operation := range plan.Operations {
			if operation.Kind == scheduler.OperationDrain {
				t.Fatalf("tick %d planned a drain %#v; no pass here may drain to make room", tick+1, operation)
			}
		}
		if plan.Next.Reservation == nil || plan.Next.Reservation.Demand != head.Key {
			t.Fatalf("tick %d reservation = %#v, want the head still reserved", tick+1, plan.Next.Reservation)
		}
		state = plan.Next
	}

	// The blocking xl finishes. The reserved head takes the released vector
	// FIRST, ahead of the builder and the maestros still queued — the proof that
	// looking past an unfittable target took neither the head's vector nor its
	// turn.
	at := start.Add(time.Duration(oversizedTargetTicks) * oversizedTargetInterval)
	released := scheduler.PlanTick(scheduler.Input{Now: at, Config: config,
		Demands: domain.Fresh(remaining, at), Instances: domain.Fresh([]domain.Instance{live[1]}, at),
		Host: reservedRemainderHost(at), Prior: state})
	got := spawnKeys(released)
	if len(got) == 0 || got[0] != head.Key {
		t.Fatalf("released spawns = %#v, want the reserved head first", got)
	}
}
