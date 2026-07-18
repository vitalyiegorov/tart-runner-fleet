package replay_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

func TestMacOSExclusiveFourMaestroIncidentReplay(t *testing.T) {
	now := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	profiles := map[domain.ProfileID]domain.Profile{
		"small":   {ID: "small", Platform: domain.PlatformLinux, Route: "linux-small", Resources: domain.Resources{CPU: 1, MemoryMB: 2_048, Slots: 1}},
		"maestro": {ID: "maestro", Platform: domain.PlatformMacOS, Route: "macos-maestro", Resources: domain.Resources{CPU: 2, MemoryMB: 4_096, Slots: 1}, MaxActive: 4},
	}
	config := scheduler.Config{
		LinuxCapacity: domain.Resources{CPU: 8, MemoryMB: 16_384, Slots: 4},
		FairnessAge:   5 * time.Minute, RepoCaps: map[string]int{"mobile/a": 4, "mobile/b": 4, "linux/a": 4},
		Profiles: profiles, MacOSExclusive: true,
	}
	job := func(repo string, id int64, age time.Duration, profile domain.ProfileID) domain.Demand {
		return domain.Demand{Key: domain.DemandKey{Repo: repo, RunID: 10, Attempt: 1, JobID: id}, CreatedAt: now.Add(-age),
			Profile: profile, Route: profiles[profile].Route, Platform: profiles[profile].Platform, Event: domain.EventPullRequest}
	}
	macA, macB, macC := job("mobile/a", 1, 4*time.Minute, "maestro"), job("mobile/b", 2, 3*time.Minute, "maestro"), job("mobile/a", 3, 2*time.Minute, "maestro")
	linux := job("linux/a", 4, 20*time.Minute, "small")
	running := domain.Instance{ID: "maestro-1", Repo: "mobile/b", Platform: domain.PlatformMacOS, Profile: "maestro", Route: "macos-maestro", Resources: profiles["maestro"].Resources, State: domain.InstanceRunning}
	in := scheduler.Input{Now: now, Config: config, Demands: domain.Fresh([]domain.Demand{linux, macC, macA, macB}, now),
		Instances: domain.Fresh([]domain.Instance{running}, now), Host: domain.Fresh(domain.Host{Available: config.LinuxCapacity}, now)}

	first := scheduler.PlanTick(in)
	in.Demands = domain.Fresh([]domain.Demand{macB, macA, linux, macC}, now)
	second := scheduler.PlanTick(in)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("exclusive incident replay depends on input order: %#v / %#v", first, second)
	}
	var got []domain.DemandKey
	for _, operation := range first.Operations {
		if operation.Kind == scheduler.OperationSpawn {
			got = append(got, operation.Demand)
		}
	}
	if want := []domain.DemandKey{macA.Key, macB.Key, macC.Key}; !reflect.DeepEqual(got, want) {
		t.Fatalf("exclusive incident cohort = %#v, want %#v", got, want)
	}
}
