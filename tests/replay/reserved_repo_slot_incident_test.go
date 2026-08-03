package replay_test

import (
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

// Replay of the 2026-08-03 same-repository starvation on the production Mac
// mini (Mac16,10: 10 cores, 24 GiB), observed at ~06:50Z on v0.1.332 — the
// build that shipped ADR 0029.
//
// Timeline. A Linux `xl` (6 CPU / 12288 MB) from `rnw-community/rnw-community`
// was busy. The aged global-FIFO head was a second `xl` from the SAME
// repository, which cannot fit beside it (6 + 6 > 10), so `planLinux` held its
// reservation and ADR 0029's vector condition correctly released the four-core
// residual: a head that does not fit is waiting on the live `xl`, not on
// backfill. Three macOS `maestro` jobs (4 CPU / 7168 MB) from that same
// repository had been queued 4h39m. The repository cap is 4 and exactly one
// instance was live.
//
// ADR 0029's repository condition excluded them anyway — it dropped the head's
// repository wholesale — so the residual stayed shut to the oldest work on the
// host. Three cap slots stood free and the head needs one; admitting a maestro
// could not have cost the head its slot.
//
// This replay pins the temporal property: across the window the free vector is
// put to work exactly once (there is room for one maestro), the reservation
// survives every tick, and the reserved head still takes the released vector
// FIRST — so the same-repository backfill provably consumed neither the head's
// vector nor its repository slot.

const (
	reservedRepoSlotTicks    = 12
	reservedRepoSlotInterval = 5 * time.Minute
	reservedRepoSlotRepo     = "rnw-community/rnw-community"
)

// reservedRepoSlotConfig is the live topology with the live cap: four
// concurrent instances for the repository that owns both the head and the
// starved macOS work.
func reservedRepoSlotConfig(cap int) scheduler.Config {
	config := reservedRemainderConfig(true)
	config.RepoCaps = map[string]int{reservedRepoSlotRepo: cap, "a/repo": 4}
	return config
}

func reservedRepoSlotDemand(at time.Time, jobID int64, age time.Duration, profile domain.ProfileID) domain.Demand {
	configured := reservedRemainderProfiles()[profile]
	return domain.Demand{Key: domain.DemandKey{Repo: reservedRepoSlotRepo, RunID: 5107, Attempt: 1, JobID: jobID},
		CreatedAt: at.Add(-age), Profile: profile, Route: configured.Route,
		Platform: configured.Platform, Event: domain.EventPullRequest}
}

func TestReservedRepositorySlotIncidentAdmitsAgedSameRepositoryWork(t *testing.T) {
	start := time.Date(2026, 8, 3, 6, 50, 0, 0, time.UTC)
	busyXL := reservedRemainderInstance("trf-linux-xl-3c88af", "xl", reservedRepoSlotRepo)

	head := reservedRepoSlotDemand(start, 11, 5*time.Hour, "xl")
	maestros := make([]domain.Demand, 0, 3)
	for i := 0; i < 3; i++ {
		maestros = append(maestros, reservedRepoSlotDemand(start, int64(20+i), 4*time.Hour+39*time.Minute, "maestro"))
	}
	queue := append([]domain.Demand{head}, maestros...)
	instances := []domain.Instance{busyXL}

	// The control arm: the head's repository is at its last free slot (cap 2 with
	// one live instance), so the head genuinely needs the slot the maestros would
	// take. Nothing may be admitted, on any tick.
	control := scheduler.State{}
	for tick := 0; tick < reservedRepoSlotTicks; tick++ {
		at := start.Add(time.Duration(tick) * reservedRepoSlotInterval)
		plan := scheduler.PlanTick(scheduler.Input{Now: at, Config: reservedRepoSlotConfig(2),
			Demands: domain.Fresh(queue, at), Instances: domain.Fresh(instances, at),
			Host: reservedRemainderHost(at), Prior: control})
		if plan.Status != scheduler.PlanReady {
			t.Fatalf("control tick %d status = %s, want ready", tick, plan.Status)
		}
		if got := spawnKeys(plan); len(got) != 0 {
			t.Fatalf("control tick %d spawns = %#v, want none: the head needs that slot", tick, got)
		}
		if plan.Next.Reservation == nil || plan.Next.Reservation.Demand != head.Key {
			t.Fatalf("control tick %d reservation = %#v, want the aged xl head reserved", tick, plan.Next.Reservation)
		}
		control = plan.Next
	}

	// The live arm: cap 4 with one live instance, so three slots stand free and
	// the head needs one. Tick 1 must put the four free cores to work with the
	// OLDEST candidate, which is a maestro from the head's own repository.
	config := reservedRepoSlotConfig(4)
	first := scheduler.PlanTick(scheduler.Input{Now: start, Config: config,
		Demands: domain.Fresh(queue, start), Instances: domain.Fresh(instances, start),
		Host: reservedRemainderHost(start), Prior: scheduler.State{}})
	if got := spawnKeys(first); len(got) != 1 || got[0] != maestros[0].Key {
		t.Fatalf("tick 1 spawns = %#v, want exactly one aged same-repository maestro", got)
	}
	if first.Next.Reservation == nil || first.Next.Reservation.Demand != head.Key {
		t.Fatalf("tick 1 reservation = %#v, want the aged xl head still reserved", first.Next.Reservation)
	}

	// Ticks 2..12: the admitted maestro is live and no longer advertised as
	// queued. The host is full (6 + 4 = 10 cores), so nothing further may be
	// admitted and the reservation must survive intact with no drain planned.
	live := []domain.Instance{busyXL,
		reservedRemainderInstance("trf-maestro-77b104", "maestro", reservedRepoSlotRepo)}
	remaining := append([]domain.Demand{head}, maestros[1:]...)
	state := first.Next
	for tick := 1; tick < reservedRepoSlotTicks; tick++ {
		at := start.Add(time.Duration(tick) * reservedRepoSlotInterval)
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
	// FIRST, ahead of the two maestros still queued — the proof that lending a
	// spare repository slot took neither the head's vector nor its turn.
	at := start.Add(time.Duration(reservedRepoSlotTicks) * reservedRepoSlotInterval)
	released := scheduler.PlanTick(scheduler.Input{Now: at, Config: config,
		Demands: domain.Fresh(remaining, at), Instances: domain.Fresh([]domain.Instance{live[1]}, at),
		Host: reservedRemainderHost(at), Prior: state})
	got := spawnKeys(released)
	if len(got) != 1 || got[0] != head.Key {
		t.Fatalf("release tick spawns = %#v, want only the reserved head admitted first", got)
	}
	if released.Next.Reservation != nil {
		t.Fatalf("release tick reservation = %#v, want it cleared once the head spawned", released.Next.Reservation)
	}
}
