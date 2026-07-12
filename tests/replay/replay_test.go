package replay_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

func TestIncumbentSmokeReplayIsDeterministic(t *testing.T) {
	now := time.Date(2026, 7, 12, 18, 0, 0, 0, time.UTC)
	profiles := map[domain.ProfileID]domain.Profile{
		"small": {ID: "small", Platform: domain.PlatformLinux, Route: "tiered", Resources: domain.Resources{CPU: 1, MemoryMB: 2_048, Slots: 1}},
		"large": {ID: "large", Platform: domain.PlatformLinux, Route: "legacy", Resources: domain.Resources{CPU: 4, MemoryMB: 8_192, Slots: 1}},
	}
	config := scheduler.Config{
		LinuxCapacity: domain.Resources{CPU: 8, MemoryMB: 16_384, Slots: 4},
		FairnessAge:   5 * time.Minute,
		RepoCaps:      map[string]int{"old/repo": 2, "young/repo": 2},
		Profiles:      profiles,
	}
	demands := []domain.Demand{
		{Key: domain.DemandKey{Repo: "young/repo", RunID: 2, Attempt: 1, JobID: 2}, CreatedAt: now.Add(-time.Minute), Platform: domain.PlatformLinux, Profile: "small", Route: "tiered", Event: domain.EventPullRequest},
		{Key: domain.DemandKey{Repo: "old/repo", RunID: 1, Attempt: 1, JobID: 1}, CreatedAt: now.Add(-10 * time.Minute), Platform: domain.PlatformLinux, Profile: "large", Route: "legacy", Event: domain.EventSchedule},
	}
	in := scheduler.Input{
		Now: now, Config: config,
		Demands:   domain.Fresh(demands, now),
		Instances: domain.Fresh([]domain.Instance(nil), now),
		Host:      domain.Fresh(domain.Host{Available: config.LinuxCapacity}, now),
	}
	first := scheduler.PlanTick(in)
	second := scheduler.PlanTick(in)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("replay diverged: %#v / %#v", first, second)
	}
	if len(first.Operations) != 1 || first.Operations[0].Demand != demands[1].Key {
		t.Fatalf("aged global FIFO characterization changed: %#v", first)
	}
}

func TestIncidentReplayMatrix(t *testing.T) {
	now := time.Date(2026, 7, 12, 18, 0, 0, 0, time.UTC)
	profiles := map[domain.ProfileID]domain.Profile{
		"small":   {ID: "small", Platform: domain.PlatformLinux, Route: "tiered", Resources: domain.Resources{CPU: 1, MemoryMB: 2_048, Slots: 1}},
		"large":   {ID: "large", Platform: domain.PlatformLinux, Route: "legacy", Resources: domain.Resources{CPU: 4, MemoryMB: 8_192, Slots: 1}},
		"builder": {ID: "builder", Platform: domain.PlatformMacOS, Route: "macos-builder", Resources: domain.Resources{CPU: 8, MemoryMB: 12_288, Slots: 1}, MaxActive: 1},
		"maestro": {ID: "maestro", Platform: domain.PlatformMacOS, Route: "macos-maestro", Resources: domain.Resources{CPU: 4, MemoryMB: 7_168, Slots: 1}, MaxActive: 2},
	}
	config := scheduler.Config{
		LinuxCapacity: domain.Resources{CPU: 8, MemoryMB: 16_384, Slots: 4},
		FairnessAge:   5 * time.Minute,
		RepoCaps:      map[string]int{"a/repo": 4, "b/repo": 4, "c/repo": 4},
		Profiles:      profiles,
	}
	job := func(repo string, id int64, age time.Duration, profile domain.ProfileID) domain.Demand {
		return domain.Demand{
			Key:       domain.DemandKey{Repo: repo, RunID: 100, Attempt: 1, JobID: id},
			CreatedAt: now.Add(-age), Profile: profile, Route: profiles[profile].Route,
			Platform: profiles[profile].Platform, Event: domain.EventPullRequest,
		}
	}
	plan := func(demands []domain.Demand, instances []domain.Instance, mutate func(*scheduler.Config)) scheduler.Plan {
		candidate := config
		candidate.RepoCaps = map[string]int{"a/repo": 4, "b/repo": 4, "c/repo": 4}
		if mutate != nil {
			mutate(&candidate)
		}
		return scheduler.PlanTick(scheduler.Input{
			Now: now, Config: candidate,
			Demands: domain.Fresh(demands, now), Instances: domain.Fresh(instances, now),
			Host: domain.Fresh(domain.Host{Available: domain.Resources{CPU: 8, MemoryMB: 16_384, Slots: 4}}, now),
		})
	}
	spawnKeys := func(result scheduler.Plan) []domain.DemandKey {
		var keys []domain.DemandKey
		for _, operation := range result.Operations {
			if operation.Kind == scheduler.OperationSpawn {
				keys = append(keys, operation.Demand)
			}
		}
		return keys
	}

	t.Run("queued siblings in an in-progress run", func(t *testing.T) {
		first, second := job("a/repo", 1, time.Minute, "small"), job("a/repo", 2, time.Minute, "small")
		first.RunStatus, second.RunStatus = domain.RunInProgress, domain.RunInProgress
		if got := spawnKeys(plan([]domain.Demand{first, second}, nil, nil)); !reflect.DeepEqual(got, []domain.DemandKey{first.Key, second.Key}) {
			t.Fatalf("siblings lost: %#v", got)
		}
	})

	t.Run("wrong-route idle does not consume cap", func(t *testing.T) {
		wanted := job("a/repo", 1, time.Minute, "small")
		idle := domain.Instance{ID: "legacy", Repo: "a/repo", Platform: domain.PlatformLinux, Profile: "large", Route: "legacy", Resources: profiles["large"].Resources, State: domain.InstanceOnlineIdle}
		got := plan([]domain.Demand{wanted}, []domain.Instance{idle}, func(config *scheduler.Config) { config.RepoCaps["a/repo"] = 1 })
		if keys := spawnKeys(got); len(keys) != 1 || keys[0] != wanted.Key {
			t.Fatalf("wrong route blocked demand: %#v", got)
		}
	})

	t.Run("infeasible head does not consume admission", func(t *testing.T) {
		large, small := job("a/repo", 1, 2*time.Minute, "large"), job("a/repo", 2, time.Minute, "small")
		got := plan([]domain.Demand{large, small}, nil, func(config *scheduler.Config) {
			config.LinuxCapacity = domain.Resources{CPU: 1, MemoryMB: 2_048, Slots: 1}
			config.RepoCaps["a/repo"] = 1
		})
		if keys := spawnKeys(got); len(keys) != 1 || keys[0] != small.Key {
			t.Fatalf("feasible tail hidden: %#v", got)
		}
	})

	t.Run("profile switch has no extra tick", func(t *testing.T) {
		maestro := job("a/repo", 1, 10*time.Minute, "maestro")
		builder := domain.Instance{ID: "builder", Platform: domain.PlatformMacOS, Profile: "builder", State: domain.InstanceOnlineIdle}
		got := plan([]domain.Demand{maestro}, []domain.Instance{builder}, nil)
		if len(got.Operations) != 2 || got.Operations[0].Kind != scheduler.OperationDrain || got.Operations[1].Kind != scheduler.OperationSpawn || len(got.Operations[1].DependsOn) != 1 {
			t.Fatalf("profile handoff regressed: %#v", got)
		}
	})

	t.Run("global platform age beats young builder", func(t *testing.T) {
		linux, builder := job("a/repo", 1, 4*time.Minute, "small"), job("b/repo", 2, time.Minute, "builder")
		if keys := spawnKeys(plan([]domain.Demand{builder, linux}, nil, nil)); len(keys) != 1 || keys[0] != linux.Key {
			t.Fatalf("builder priority starvation returned: %#v", keys)
		}
	})

	t.Run("maestro caps at two and spreads repos", func(t *testing.T) {
		a1, a2, b1 := job("a/repo", 1, time.Minute, "maestro"), job("a/repo", 2, 50*time.Second, "maestro"), job("b/repo", 3, 40*time.Second, "maestro")
		if keys := spawnKeys(plan([]domain.Demand{a1, a2, b1}, nil, nil)); !reflect.DeepEqual(keys, []domain.DemandKey{a1.Key, b1.Key}) {
			t.Fatalf("maestro allocation regressed: %#v", keys)
		}
	})
}
