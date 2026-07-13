package replay_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

// TestIssue13BlockedMacDrainBackfillsSmallRequiredGate replays the production
// deadlock from issue #13: an old macOS request has selected handoff, one
// legitimate Linux job is still running, and a small required gate fits in the
// residual Linux envelope. The gate must make progress without spawning macOS
// or exceeding the host vector.
func TestIssue13BlockedMacDrainBackfillsSmallRequiredGate(t *testing.T) {
	now := time.Date(2026, 7, 13, 8, 21, 0, 0, time.UTC)
	profiles := map[domain.ProfileID]domain.Profile{
		"small": {
			ID: "small", Platform: domain.PlatformLinux, Route: "linux-small",
			Resources: domain.Resources{CPU: 1, MemoryMB: 2_048, Slots: 1},
		},
		"medium": {
			ID: "medium", Platform: domain.PlatformLinux, Route: "linux-medium",
			Resources: domain.Resources{CPU: 2, MemoryMB: 4_096, Slots: 1},
		},
		"builder": {
			ID: "builder", Platform: domain.PlatformMacOS, Route: "macos-builder",
			Resources: domain.Resources{CPU: 8, MemoryMB: 12_288, Slots: 1}, MaxActive: 1,
		},
	}
	config := scheduler.Config{
		LinuxCapacity: domain.Resources{CPU: 8, MemoryMB: 16_384, Slots: 4},
		FairnessAge:   5 * time.Minute,
		RepoCaps:      map[string]int{"vitalyiegorov/knee-doctor": 1, "vitalyiegorov/tart-runner-fleet": 3},
		Profiles:      profiles,
	}
	mac := domain.Demand{
		Key:       domain.DemandKey{Repo: "budgie-at/budgie", RunID: 13, Attempt: 1, JobID: 1},
		CreatedAt: now.Add(-30 * time.Minute), Profile: "builder", Route: "macos-builder",
		Platform: domain.PlatformMacOS, Event: domain.EventPullRequest, RunStatus: domain.RunQueued,
	}
	gate := domain.Demand{
		Key:       domain.DemandKey{Repo: "vitalyiegorov/tart-runner-fleet", RunID: 12, Attempt: 1, JobID: 2},
		CreatedAt: now.Add(-19 * time.Minute), Profile: "small", Route: "linux-small",
		Platform: domain.PlatformLinux, Event: domain.EventPullRequest, RunStatus: domain.RunQueued,
	}
	holder := domain.Instance{
		ID: "gha-linux-burst-seo", Repo: "vitalyiegorov/knee-doctor",
		Platform: domain.PlatformLinux, Profile: "medium", Route: "linux-medium",
		Resources: profiles["medium"].Resources, State: domain.InstanceRunning,
	}

	plan := func(demands []domain.Demand) scheduler.Plan {
		return scheduler.PlanTick(scheduler.Input{
			Now: now, Config: config,
			Demands: domain.Fresh(demands, now), Instances: domain.Fresh([]domain.Instance{holder}, now),
			Host: domain.Fresh(domain.Host{Available: domain.Resources{CPU: 6, MemoryMB: 12_288, Slots: 3}}, now),
		})
	}
	first := plan([]domain.Demand{mac, gate})
	second := plan([]domain.Demand{gate, mac})
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("issue #13 replay is input-order dependent: %#v / %#v", first, second)
	}

	var spawned []scheduler.Operation
	for _, operation := range first.Operations {
		if operation.Kind == scheduler.OperationSpawn {
			spawned = append(spawned, operation)
		}
	}
	if len(spawned) != 1 || spawned[0].Demand != gate.Key || spawned[0].Profile != gate.Profile {
		t.Fatalf("issue #13 bounded backfill = %#v, want only small gate %s", spawned, gate.Key)
	}
	used := holder.Resources.Add(profiles[spawned[0].Profile].Resources)
	if !config.LinuxCapacity.CanFit(used) {
		t.Fatalf("issue #13 backfill exceeded Linux capacity: used=%#v capacity=%#v", used, config.LinuxCapacity)
	}
	for _, operation := range spawned {
		if profiles[operation.Profile].Platform == domain.PlatformMacOS {
			t.Fatalf("issue #13 introduced Linux/macOS overlap: %#v", first)
		}
	}
	if first.Next.MacHandoff == nil || !first.Next.MacHandoff.BackfillAdmitted {
		t.Fatalf("issue #13 did not persist its one-shot backfill budget: %#v", first.Next.MacHandoff)
	}
	replayed := scheduler.PlanTick(scheduler.Input{
		Now: now.Add(time.Minute), Config: config,
		Demands: domain.Fresh([]domain.Demand{mac, gate}, now.Add(time.Minute)), Instances: domain.Fresh([]domain.Instance{holder}, now.Add(time.Minute)),
		Host: domain.Fresh(domain.Host{Available: domain.Resources{CPU: 6, MemoryMB: 12_288, Slots: 3}}, now.Add(time.Minute)), Prior: first.Next,
	})
	for _, operation := range replayed.Operations {
		if operation.Kind == scheduler.OperationSpawn {
			t.Fatalf("issue #13 admitted repeated backfill on a later tick: %#v", replayed)
		}
	}
}
