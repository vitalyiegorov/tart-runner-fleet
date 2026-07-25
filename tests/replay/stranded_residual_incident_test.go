package replay_test

import (
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

// Replay of the 2026-07-25 stranded-residual incident on the production Mac
// mini (Mac16,10: 10 cores, 24 GiB).
//
// Observed: `fleetctl status` reported 12 queued jobs across builder, maestro,
// large, and medium with the oldest waiting 37 minutes, exactly one live VM (a
// 7-vCPU macOS builder), `queue_slo_breached`, and a host that fleetd itself
// measured as 71.44% CPU idle with 11550 MiB available and
// `fleet_host_admission_allowed 1`.
//
// Mechanism: the 8-vCPU/16-GiB shared envelope minus the live builder leaves a
// residual of 1 vCPU / 4096 MiB. The aged Linux head is a `large` that cannot
// fit that residual, so it reserves. Backfill then computed its remainder as
// free-minus-reservation, which underflows, so every tick admitted nothing --
// even for queued work that fits the residual exactly -- until the builder
// finished. The reserved large was blocked by the macOS builder, so Linux
// backfill could never have delayed it.
//
// This replay pins the temporal property the unit tests cannot: across ticks
// the residual is put to work AND the reservation is not starved.
const strandedIncidentEnvelope = 4

func strandedProfiles() map[domain.ProfileID]domain.Profile {
	return map[domain.ProfileID]domain.Profile{
		"small":   {ID: "small", Platform: domain.PlatformLinux, Route: "tiered", Resources: domain.Resources{CPU: 1, MemoryMB: 2_048, Slots: 1}},
		"medium":  {ID: "medium", Platform: domain.PlatformLinux, Route: "tiered", Resources: domain.Resources{CPU: 2, MemoryMB: 4_096, Slots: 1}},
		"large":   {ID: "large", Platform: domain.PlatformLinux, Route: "legacy", Resources: domain.Resources{CPU: 4, MemoryMB: 8_192, Slots: 1}},
		"builder": {ID: "builder", Platform: domain.PlatformMacOS, Route: "macos-builder", Resources: domain.Resources{CPU: 7, MemoryMB: 12_288, Slots: 1}, MaxActive: 1},
	}
}

func strandedConfig() scheduler.Config {
	return scheduler.Config{
		LinuxCapacity:          domain.Resources{CPU: 8, MemoryMB: 16_384, Slots: strandedIncidentEnvelope},
		FairnessAge:            5 * time.Minute,
		AssignedTimeout:        15 * time.Minute,
		RepoCaps:               map[string]int{"a/repo": 4, "b/repo": 4, "mac-a": 2},
		Profiles:               strandedProfiles(),
		MixedPlatformAdmission: true,
	}
}

func strandedInstance(id string, profile domain.ProfileID, repo string) domain.Instance {
	p := strandedProfiles()[profile]
	return domain.Instance{ID: id, Repo: repo, Platform: p.Platform, Profile: profile, Route: p.Route,
		Resources: p.Resources, State: domain.InstanceRunning, Power: domain.InstancePowerRunning}
}

func TestStrandedResidualIncidentPutsHostToWorkWithoutStarvingReservation(t *testing.T) {
	now := time.Date(2026, 7, 25, 5, 36, 17, 0, time.UTC)
	// The measured host observation from the incident.
	host := domain.Fresh(domain.Host{
		Available: domain.Resources{CPU: 8, MemoryMB: 11_550, Slots: strandedIncidentEnvelope},
		Pressure: domain.HostPressure{AvailableMemoryMB: 11_550, FreeDiskGB: 152, SwapUsedMB: 574,
			CPUIdlePercent: 71.44, LoadAverage: 3.37, AdmissionAllowed: true, AdmissionReason: "capacity available"},
	}, now)

	agedLarge := domain.Demand{Key: domain.DemandKey{Repo: "a/repo", RunID: 100, Attempt: 1, JobID: 3},
		CreatedAt: now.Add(-27 * time.Minute), Profile: "large", Route: "legacy", Platform: domain.PlatformLinux,
		Event: domain.EventPullRequest}
	fittingSmall := domain.Demand{Key: domain.DemandKey{Repo: "b/repo", RunID: 100, Attempt: 1, JobID: 7},
		CreatedAt: now.Add(-3 * time.Minute), Profile: "small", Route: "tiered", Platform: domain.PlatformLinux,
		Event: domain.EventPullRequest}
	builderHead := domain.Demand{Key: domain.DemandKey{Repo: "mac-a", RunID: 100, Attempt: 1, JobID: 1},
		CreatedAt: now.Add(-37 * time.Minute), Profile: "builder", Route: "macos-builder",
		Platform: domain.PlatformMacOS, Event: domain.EventPullRequest}

	demands := []domain.Demand{builderHead, agedLarge, fittingSmall}
	builder := strandedInstance("trf-builder-35917ac43a789b33", "builder", "mac-a")

	// Tick 1: the builder holds 7 vCPU. The aged large reserves; the small must
	// be admitted into the otherwise stranded residual.
	first := scheduler.PlanTick(scheduler.Input{Now: now, Config: strandedConfig(),
		Demands: domain.Fresh(demands, now), Instances: domain.Fresh([]domain.Instance{builder}, now),
		Host: host, Prior: scheduler.State{}})
	if first.Status != scheduler.PlanReady {
		t.Fatalf("tick 1 status = %s, want ready", first.Status)
	}
	spawned := spawnKeys(first)
	if len(spawned) != 1 || spawned[0] != fittingSmall.Key {
		t.Fatalf("tick 1 spawns = %#v, want only the fitting small; the residual must not be stranded", spawned)
	}
	if first.Next.Reservation == nil || first.Next.Reservation.Profile != "large" {
		t.Fatalf("tick 1 reservation = %#v, want the aged large reserved", first.Next.Reservation)
	}

	// Tick 2: the backfilled small is live beside the builder, so GitHub no
	// longer advertises it as queued. The residual is now genuinely exhausted
	// (1-1 vCPU), so nothing further may be admitted.
	small := strandedInstance("trf-linux-small-1", "small", "b/repo")
	assigned := []domain.Demand{builderHead, agedLarge}
	second := scheduler.PlanTick(scheduler.Input{Now: now.Add(30 * time.Second), Config: strandedConfig(),
		Demands: domain.Fresh(assigned, now), Instances: domain.Fresh([]domain.Instance{builder, small}, now),
		Host: host, Prior: first.Next})
	if got := spawnKeys(second); len(got) != 0 {
		t.Fatalf("tick 2 spawns = %#v, want none: the residual is exhausted", got)
	}
	if second.Next.Reservation == nil || second.Next.Reservation.Demand != agedLarge.Key {
		t.Fatalf("tick 2 reservation = %#v, want the large still reserved", second.Next.Reservation)
	}

	// Tick 3: the builder's job finished. Its 7 vCPU / 12 GiB return to the
	// envelope and the reserved large must be admitted immediately -- the
	// backfill delayed it by nothing.
	third := scheduler.PlanTick(scheduler.Input{Now: now.Add(2 * time.Minute), Config: strandedConfig(),
		Demands: domain.Fresh(assigned, now), Instances: domain.Fresh([]domain.Instance{small}, now),
		Host: host, Prior: second.Next})
	got := spawnKeys(third)
	if len(got) == 0 || got[0] != agedLarge.Key {
		t.Fatalf("tick 3 spawns = %#v, want the reserved large admitted first once the builder exited", got)
	}
	if third.Next.Reservation != nil {
		t.Fatalf("tick 3 reservation = %#v, want it cleared after the large spawned", third.Next.Reservation)
	}
}

func spawnKeys(plan scheduler.Plan) []domain.DemandKey {
	var keys []domain.DemandKey
	for _, operation := range plan.Operations {
		if operation.Kind == scheduler.OperationSpawn {
			keys = append(keys, operation.Demand)
		}
	}
	return keys
}
