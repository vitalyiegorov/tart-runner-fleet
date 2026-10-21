package app

import (
	"context"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

// TestAdvertisedCapacityOnlyLowersAndNeverBelowWhatIsHeld is issue #388's
// composition rule: min(configured, held + fit), with the configured value as
// the answer whenever the headroom is unknown.
func TestAdvertisedCapacityOnlyLowersAndNeverBelowWhatIsHeld(t *testing.T) {
	cases := []struct {
		name       string
		configured int
		headroom   ScaleSetHeadroom
		known      bool
		want       int
	}{
		{"unknown fails open", 2, ScaleSetHeadroom{}, false, 2},
		{"nothing held, nothing fits", 2, ScaleSetHeadroom{}, true, 0},
		{"already assigned work is kept", 2, ScaleSetHeadroom{Held: 1}, true, 1},
		{"held plus fit", 4, ScaleSetHeadroom{Held: 1, Fit: 2}, true, 3},
		{"never above the configured ceiling", 2, ScaleSetHeadroom{Held: 1, Fit: 5}, true, 2},
		{"negative terms are not capacity", 2, ScaleSetHeadroom{Held: -1, Fit: -4}, true, 0},
	}
	for _, tc := range cases {
		if got := AdvertisedCapacity(tc.configured, tc.headroom, tc.known); got != tc.want {
			t.Fatalf("%s: AdvertisedCapacity = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func headroomInput(instances domain.Observation[[]domain.Instance]) scheduler.Input {
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	profile := domain.Profile{ID: "small", Route: "tiered", Platform: domain.PlatformLinux,
		Resources: domain.Resources{CPU: 1, MemoryMB: 2_048, Slots: 1}}
	return scheduler.Input{Now: now, Demands: domain.Fresh([]domain.Demand{}, now), Instances: instances,
		Host: domain.Fresh(domain.Host{Available: domain.Resources{CPU: 4, MemoryMB: 8_192, Slots: 4}}, now),
		Config: scheduler.Config{LinuxCapacity: domain.Resources{CPU: 4, MemoryMB: 8_192, Slots: 4},
			Profiles: map[domain.ProfileID]domain.Profile{"small": profile}}}
}

// TestScaleSetHeadroomFloorsAtTheLargerHeldCountAndOmitsTheUnknown: what a set
// holds is the larger of its own record and GitHub's fresh statistics, and a set
// whose statistics cannot be read has no row at all, so it fails open.
func TestScaleSetHeadroomFloorsAtTheLargerHeldCountAndOmitsTheUnknown(t *testing.T) {
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	binding := Binding{StoreKey: 7, ScaleSetID: 7, Targets: []string{"owner/repo"},
		Profile: domain.Profile{ID: "small", Route: "tiered", Platform: domain.PlatformLinux}}
	live := domain.Instance{ID: "i-1", Repo: "owner/repo", Profile: "small", Platform: domain.PlatformLinux,
		Resources: domain.Resources{CPU: 1, MemoryMB: 2_048, Slots: 1}, State: domain.InstanceRunning,
		Power: domain.InstancePowerRunning}
	foreign := live
	foreign.ID, foreign.Repo = "i-2", "other/repo"
	in := headroomInput(domain.Fresh([]domain.Instance{live, foreign}, now))
	ctx := context.Background()

	store := &fakeDemandStore{statistics: operations.DemandStatistics{MessageID: 3, ObservedAt: now,
		Available: 1, Acquired: 1, Assigned: 3, Running: 1}}
	coordinator := DemandCoordinator{Store: store, Now: func() time.Time { return now }}
	rows := coordinator.scaleSetHeadroom(ctx, []Binding{binding}, in, map[int64]int{7: 1})
	if len(rows) != 1 || rows[0].Held != 5 || rows[0].Fit != 2 {
		t.Fatalf("fresh statistics outrank a smaller local count: %+v", rows)
	}
	store.statistics.ObservedAt = now.Add(-time.Hour)
	rows = coordinator.scaleSetHeadroom(ctx, []Binding{binding}, in, map[int64]int{7: 1})
	if len(rows) != 1 || rows[0].Held != 2 {
		t.Fatalf("stale statistics leave the local count (one queued, one live): %+v", rows)
	}
	missing := DemandCoordinator{Store: statisticsNotFoundStore{store}, Now: coordinator.Now}
	if rows = missing.scaleSetHeadroom(ctx, []Binding{binding}, in, nil); len(rows) != 1 || rows[0].Held != 1 {
		t.Fatalf("absent statistics leave the live instance: %+v", rows)
	}
	broken := DemandCoordinator{Store: statisticsReadErrorStore{store}, Now: coordinator.Now}
	if rows = broken.scaleSetHeadroom(ctx, []Binding{binding}, in, nil); len(rows) != 0 {
		t.Fatalf("unreadable statistics are unknown, not zero: %+v", rows)
	}
	unknownProfile := binding
	unknownProfile.Profile.ID = "absent"
	if rows = coordinator.scaleSetHeadroom(ctx, []Binding{unknownProfile}, in, nil); len(rows) != 0 {
		t.Fatalf("a profile the scheduler cannot size is unknown: %+v", rows)
	}
	unavailable := headroomInput(domain.Unavailable[[]domain.Instance]("tart down"))
	if rows = coordinator.scaleSetHeadroom(ctx, []Binding{binding}, unavailable, nil); rows != nil {
		t.Fatalf("an unavailable inventory has no headroom at all: %+v", rows)
	}
	plain := DemandCoordinator{Store: noStatisticsStore{store}, Now: coordinator.Now}
	if rows = plain.scaleSetHeadroom(ctx, []Binding{binding}, in, nil); len(rows) != 1 || rows[0].Held != 1 {
		t.Fatalf("a store without statistics leaves the local count: %+v", rows)
	}
}
