package replay_test

import (
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

// Replay of the 2026-08-02 starved-remainder incident on the production Mac
// mini (Mac16,10: 10 cores, 24 GiB).
//
// Timeline. A Linux `xl` (6 CPU / 12288 MB) ran a single job from 12:47Z to
// 14:05Z. Three macOS `maestro` jobs (4 CPU / 7168 MB) sat queued the whole
// time, aged 1h18m by 13:50Z. The aged global-FIFO head was a second `xl`,
// which cannot fit beside the live one (6 + 6 > 10), so `planLinux` correctly
// held its reservation. The host admission gate was green throughout and four
// cores were free the entire 78 minutes — a maestro fits them exactly.
//
// `fillMacRemainder` returned early on `plan.Next.Reservation != nil`, so not
// one of those ticks admitted anything. The reserved `xl` could not have used
// the four free cores; it was waiting for the live `xl` to exit.
//
// The mechanism note that matters for reading this replay: `State.Reservation`
// is singular and Linux-authored — `planLinux` is its only writer — so the
// reserved head here is the queued `xl`, and the macOS work is what starves
// behind it. That is also what PR #112's forensics recorded.
//
// This replay pins the temporal property the unit tests cannot: across the
// whole 78-minute window the free vector is put to work exactly once (there is
// only room for one maestro), the reservation survives every tick, and the
// reserved head still starts FIRST on the tick the blocking `xl` exits — so the
// backfill provably consumed nothing the head needed.

const (
	reservedRemainderTicks    = 16
	reservedRemainderInterval = 5 * time.Minute
)

func reservedRemainderProfiles() map[domain.ProfileID]domain.Profile {
	return map[domain.ProfileID]domain.Profile{
		"small":   {ID: "small", Platform: domain.PlatformLinux, Route: "tiered", Resources: domain.Resources{CPU: 1, MemoryMB: 2_048, Slots: 1}},
		"xl":      {ID: "xl", Platform: domain.PlatformLinux, Route: "tiered", Resources: domain.Resources{CPU: 6, MemoryMB: 12_288, Slots: 1}},
		"builder": {ID: "builder", Platform: domain.PlatformMacOS, Route: "macos-builder", Resources: domain.Resources{CPU: 6, MemoryMB: 12_288, Slots: 1}, MaxActive: 1},
		"maestro": {ID: "maestro", Platform: domain.PlatformMacOS, Route: "macos-maestro", Resources: domain.Resources{CPU: 4, MemoryMB: 7_168, Slots: 1}, MaxActive: 2},
	}
}

// reservedRemainderConfig is the live topology: the ten physical cores and
// 24 GiB of the Mac mini as the shared envelope, with mixed-platform admission
// on as it is in production.
func reservedRemainderConfig(mixed bool) scheduler.Config {
	return scheduler.Config{
		LinuxCapacity:          domain.Resources{CPU: 10, MemoryMB: 24_576, Slots: 4},
		FairnessAge:            5 * time.Minute,
		AssignedTimeout:        15 * time.Minute,
		RepoCaps:               map[string]int{"a/repo": 4, "b/repo": 4, "mac-a": 3},
		Profiles:               reservedRemainderProfiles(),
		MixedPlatformAdmission: mixed,
	}
}

func reservedRemainderInstance(id string, profile domain.ProfileID, repo string) domain.Instance {
	p := reservedRemainderProfiles()[profile]
	return domain.Instance{ID: id, Repo: repo, Platform: p.Platform, Profile: profile, Route: p.Route,
		Resources: p.Resources, State: domain.InstanceRunning, Power: domain.InstancePowerRunning}
}

// reservedRemainderHost is the measured observation: the gate green, memory and
// disk ample, the machine far from saturated.
func reservedRemainderHost(at time.Time) domain.Observation[domain.Host] {
	return domain.Fresh(domain.Host{
		Available: domain.Resources{CPU: 10, MemoryMB: 24_576, Slots: 4},
		Pressure: domain.HostPressure{AvailableMemoryMB: 15_800, FreeDiskGB: 148, SwapUsedMB: 612,
			CPUIdlePercent: 58.2, LoadAverage: 4.11, AdmissionAllowed: true, AdmissionReason: "capacity available"},
	}, at)
}

func TestStarvedRemainderIncidentAdmitsTheFreeVectorWithoutDelayingTheHead(t *testing.T) {
	// 13:50Z, the moment captured live; the blocking xl started at 12:47Z.
	start := time.Date(2026, 8, 2, 13, 50, 0, 0, time.UTC)
	busyXL := reservedRemainderInstance("trf-linux-xl-9f21c0", "xl", "a/repo")

	head := domain.Demand{Key: domain.DemandKey{Repo: "b/repo", RunID: 4021, Attempt: 1, JobID: 11},
		CreatedAt: start.Add(-80 * time.Minute), Profile: "xl", Route: "tiered",
		Platform: domain.PlatformLinux, Event: domain.EventPullRequest}
	maestros := make([]domain.Demand, 0, 3)
	for i := 0; i < 3; i++ {
		maestros = append(maestros, domain.Demand{
			Key:       domain.DemandKey{Repo: "mac-a", RunID: 4022, Attempt: 1, JobID: int64(20 + i)},
			CreatedAt: start.Add(-78 * time.Minute), Profile: "maestro", Route: "macos-maestro",
			Platform: domain.PlatformMacOS, Event: domain.EventPullRequest})
	}
	queue := append([]domain.Demand{head}, maestros...)

	// The control arm: no complementary admission pass at all. This is what the
	// early return produced for these inputs, tick after tick — the 78-minute
	// hole, with four cores idle and the gate green the whole time.
	control := scheduler.State{}
	for tick := 0; tick < reservedRemainderTicks; tick++ {
		at := start.Add(time.Duration(tick) * reservedRemainderInterval)
		plan := scheduler.PlanTick(scheduler.Input{Now: at, Config: reservedRemainderConfig(false),
			Demands: domain.Fresh(queue, at), Instances: domain.Fresh([]domain.Instance{busyXL}, at),
			Host: reservedRemainderHost(at), Prior: control})
		if plan.Status != scheduler.PlanReady {
			t.Fatalf("control tick %d status = %s, want ready", tick, plan.Status)
		}
		if got := spawnKeys(plan); len(got) != 0 {
			t.Fatalf("control tick %d spawns = %#v, want the documented hole", tick, got)
		}
		control = plan.Next
	}

	// The fixed arm, same inputs. Tick 1 must put the free vector to work.
	config := reservedRemainderConfig(true)
	first := scheduler.PlanTick(scheduler.Input{Now: start, Config: config,
		Demands: domain.Fresh(queue, start), Instances: domain.Fresh([]domain.Instance{busyXL}, start),
		Host: reservedRemainderHost(start), Prior: scheduler.State{}})
	if got := spawnKeys(first); len(got) != 1 || got[0] != maestros[0].Key {
		t.Fatalf("tick 1 spawns = %#v, want exactly one maestro in the four free cores", got)
	}
	if first.Next.Reservation == nil || first.Next.Reservation.Demand != head.Key {
		t.Fatalf("tick 1 reservation = %#v, want the aged xl head still reserved", first.Next.Reservation)
	}

	// Ticks 2..16: the admitted maestro is live and GitHub no longer advertises
	// it as queued. The host is now genuinely full (6 + 4 = 10 cores), so
	// nothing further may be admitted and the reservation must survive intact.
	live := []domain.Instance{busyXL, reservedRemainderInstance("trf-maestro-4a71e2", "maestro", "mac-a")}
	remaining := append([]domain.Demand{head}, maestros[1:]...)
	state := first.Next
	for tick := 1; tick < reservedRemainderTicks; tick++ {
		at := start.Add(time.Duration(tick) * reservedRemainderInterval)
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

	// 14:05Z: the hour-long xl job finishes. The reserved head must take the
	// released vector FIRST — ahead of the two maestros still queued — which is
	// the proof that the backfill consumed nothing the head needed.
	released := scheduler.PlanTick(scheduler.Input{Now: start.Add(78 * time.Minute), Config: config,
		Demands:   domain.Fresh(remaining, start.Add(78*time.Minute)),
		Instances: domain.Fresh([]domain.Instance{live[1]}, start.Add(78*time.Minute)),
		Host:      reservedRemainderHost(start.Add(78 * time.Minute)), Prior: state})
	got := spawnKeys(released)
	if len(got) != 1 || got[0] != head.Key {
		t.Fatalf("release tick spawns = %#v, want only the reserved head admitted first", got)
	}
	if released.Next.Reservation != nil {
		t.Fatalf("release tick reservation = %#v, want it cleared once the head spawned", released.Next.Reservation)
	}
}
