package app

import (
	"context"
	"errors"
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
		Power: domain.InstancePowerRunning, Demand: domain.DemandKey{Repo: "owner/repo", RunID: 1, Attempt: 1, JobID: 1}}
	foreign := live
	foreign.ID, foreign.Repo = "i-2", "other/repo"
	foreign.Demand.Repo = "other/repo"
	in := headroomInput(domain.Fresh([]domain.Instance{live, foreign}, now))
	ctx := context.Background()

	store := &fakeDemandStore{statistics: operations.DemandStatistics{MessageID: 3, ObservedAt: now,
		Available: 1, Acquired: 1, Assigned: 3, Running: 1}}
	coordinator := DemandCoordinator{Store: store, Now: func() time.Time { return now }}
	rows := coordinator.scaleSetHeadroom(ctx, []Binding{binding}, in, map[int64]int{7: 1}, nil)
	if len(rows) != 1 || rows[0].Held != 3 || rows[0].Fit != 2 {
		t.Fatalf("fresh assigned statistics outrank a smaller local count: %+v", rows)
	}
	// GitHub documents no disjointness between its counters: one job may be
	// reported as both acquired and assigned. TotalAssignedJobs is the scaling
	// truth, so that job is held once, not twice.
	store.statistics = operations.DemandStatistics{MessageID: 4, ObservedAt: now, Acquired: 1, Assigned: 1}
	rows = coordinator.scaleSetHeadroom(ctx, []Binding{binding}, in, nil, nil)
	if len(rows) != 1 || rows[0].Held != 1 {
		t.Fatalf("a job reported acquired and assigned must be held once: %+v", rows)
	}
	store.statistics = operations.DemandStatistics{MessageID: 3, ObservedAt: now,
		Available: 1, Acquired: 1, Assigned: 3, Running: 1}
	store.statistics.ObservedAt = now.Add(-time.Hour)
	rows = coordinator.scaleSetHeadroom(ctx, []Binding{binding}, in, map[int64]int{7: 1}, nil)
	if len(rows) != 1 || rows[0].Held != 2 {
		t.Fatalf("stale statistics leave the local count (one queued, one live): %+v", rows)
	}
	missing := DemandCoordinator{Store: statisticsNotFoundStore{store}, Now: coordinator.Now}
	if rows = missing.scaleSetHeadroom(ctx, []Binding{binding}, in, nil, nil); len(rows) != 1 || rows[0].Held != 1 {
		t.Fatalf("absent statistics leave the live instance: %+v", rows)
	}
	broken := DemandCoordinator{Store: statisticsReadErrorStore{store}, Now: coordinator.Now}
	if rows = broken.scaleSetHeadroom(ctx, []Binding{binding}, in, nil, nil); len(rows) != 0 {
		t.Fatalf("unreadable statistics are unknown, not zero: %+v", rows)
	}
	unknownProfile := binding
	unknownProfile.Profile.ID = "absent"
	if rows = coordinator.scaleSetHeadroom(ctx, []Binding{unknownProfile}, in, nil, nil); len(rows) != 0 {
		t.Fatalf("a profile the scheduler cannot size is unknown: %+v", rows)
	}
	unavailable := headroomInput(domain.Unavailable[[]domain.Instance]("tart down"))
	if rows = coordinator.scaleSetHeadroom(ctx, []Binding{binding}, unavailable, nil, nil); rows != nil {
		t.Fatalf("an unavailable inventory has no headroom at all: %+v", rows)
	}
	plain := DemandCoordinator{Store: noStatisticsStore{store}, Now: coordinator.Now}
	if rows = plain.scaleSetHeadroom(ctx, []Binding{binding}, in, nil, nil); len(rows) != 1 || rows[0].Held != 1 {
		t.Fatalf("a store without statistics leaves the local count: %+v", rows)
	}
}

// TestScaleSetHeadroomSharesOneResidualAcrossTheNodesSets is issue #397. Three
// scopes each bind one set of the same profile on a node whose residual starts
// four more instances. Every set used to be handed all four, so GitHub, which
// fills each set to what it advertised, could place twelve jobs on a node that
// can start four while a sibling advertising the same labels sat idle. What the
// sets are offered beyond what they hold must add up to the residual, once.
func TestScaleSetHeadroomSharesOneResidualAcrossTheNodesSets(t *testing.T) {
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	in := headroomInput(domain.Fresh([]domain.Instance{}, now))
	profile := domain.Profile{ID: "small", Route: "tiered", Platform: domain.PlatformLinux}
	bindings := []Binding{
		{StoreKey: 1, ScaleSetID: 1, Scope: "fleet", Targets: []string{"a/repo"}, Profile: profile},
		{StoreKey: 2, ScaleSetID: 2, Scope: "knee", Targets: []string{"b/repo"}, Profile: profile},
		{StoreKey: 3, ScaleSetID: 3, Scope: "sudoku", Targets: []string{"c/repo"}, Profile: profile},
	}
	coordinator := DemandCoordinator{Store: noStatisticsStore{&fakeDemandStore{}}, Now: func() time.Time { return now }}
	rows := coordinator.scaleSetHeadroom(context.Background(), bindings, in, nil, nil)
	if len(rows) != len(bindings) {
		t.Fatalf("every set is sized: %+v", rows)
	}
	offered := 0
	for _, row := range rows {
		offered += row.Fit
	}
	if offered != 4 {
		t.Fatalf("the sets are offered %d beyond what they hold in total, want the residual of 4 once: %+v", offered, rows)
	}
}

// TestScaleSetHeadroomHoldsOnlyWorkGitHubStillAssignsTheSet: a set's floor is
// the work GitHub has given it and not finished. A failed guest and a guest
// tearing down after its job are on the node but hold no assignment, so counting
// them advertised a slot GitHub then filled while the node had none (#397).
func TestScaleSetHeadroomHoldsOnlyWorkGitHubStillAssignsTheSet(t *testing.T) {
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	binding := Binding{StoreKey: 7, ScaleSetID: 7, Targets: []string{"owner/repo"},
		Profile: domain.Profile{ID: "small", Route: "tiered", Platform: domain.PlatformLinux}}
	key := func(job int64) domain.DemandKey {
		return domain.DemandKey{Repo: "owner/repo", RunID: 1, Attempt: 1, JobID: job}
	}
	running := domain.Instance{ID: "i-run", Repo: "owner/repo", Profile: "small", Platform: domain.PlatformLinux,
		Resources: domain.Resources{CPU: 1, MemoryMB: 2_048, Slots: 1}, State: domain.InstanceRunning,
		Power: domain.InstancePowerRunning, Demand: key(1)}
	failed, draining, stopping := running, running, running
	failed.ID, failed.State, failed.Demand = "i-failed", domain.InstanceFailed, key(2)
	draining.ID, draining.State, draining.Demand = "i-drain", domain.InstanceDraining, key(3)
	stopping.ID, stopping.State, stopping.Demand = "i-stop", domain.InstanceStopping, key(4)
	in := headroomInput(domain.Fresh([]domain.Instance{running, failed, draining, stopping}, now))
	coordinator := DemandCoordinator{Store: noStatisticsStore{&fakeDemandStore{}}, Now: func() time.Time { return now }}
	rows := coordinator.scaleSetHeadroom(context.Background(), []Binding{binding}, in, nil, nil)
	if len(rows) != 1 || rows[0].Held != 1 {
		t.Fatalf("only the running guest holds an assignment: %+v", rows)
	}
}

// TestScaleSetHeadroomServesTheSetGitHubShowsWaitingAndRespectsItsCeiling: the
// one free slot goes to the set whose scope has a job queued that this node has
// not been handed, and a set already holding its configured capacity is offered
// nothing it could not advertise.
func TestScaleSetHeadroomServesTheSetGitHubShowsWaitingAndRespectsItsCeiling(t *testing.T) {
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	profile := domain.Profile{ID: "small", Route: "tiered", Platform: domain.PlatformLinux}
	busy := func(id string, job int64) domain.Instance {
		return domain.Instance{ID: id, Repo: "a/repo", Profile: "small", Platform: domain.PlatformLinux,
			Resources: domain.Resources{CPU: 1, MemoryMB: 2_048, Slots: 1}, State: domain.InstanceRunning,
			Power: domain.InstancePowerRunning, Demand: domain.DemandKey{Repo: "a/repo", RunID: 1, Attempt: 1, JobID: job}}
	}
	in := headroomInput(domain.Fresh([]domain.Instance{busy("i-1", 1), busy("i-2", 2), busy("i-3", 3)}, now))
	bindings := []Binding{
		{StoreKey: 1, ScaleSetID: 1, Targets: []string{"a/repo"}, Profile: profile, Capacity: 3},
		{StoreKey: 2, ScaleSetID: 2, Targets: []string{"b/repo"}, Profile: profile, Capacity: 3},
		{StoreKey: 3, ScaleSetID: 3, Targets: []string{"c/repo"}, Profile: profile, Capacity: 3},
	}
	coordinator := DemandCoordinator{Store: noStatisticsStore{&fakeDemandStore{}}, Now: func() time.Time { return now }}
	rows := coordinator.scaleSetHeadroom(context.Background(), bindings, in, nil, map[int64]int{3: 2})
	if len(rows) != 3 || rows[0].Held != 3 || rows[0].Fit != 0 || rows[1].Fit != 0 || rows[2].Fit != 1 {
		t.Fatalf("the one free slot belongs to the set with work waiting, never to the full one: %+v", rows)
	}
}

// TestResidualAdvertisementKeepsTheLastReadyView pins how the node-wide view
// moves (issues #388 and #397). Before any ready tick every set advertises its
// configured capacity. A ready tick replaces the view. Nothing else does: a
// failed tick, a tick whose plan was not ready, and a ready tick that could not
// size the envelope all leave the last view in place, for as long as they last.
func TestResidualAdvertisementKeepsTheLastReadyView(t *testing.T) {
	var advertisement ResidualAdvertisement
	if advertisement.Capacity(18, 2) != 2 {
		t.Fatal("before any tick the configured capacity is advertised")
	}
	ready := TickResult{Plan: scheduler.Plan{Status: scheduler.PlanReady},
		Headroom: []ScaleSetHeadroom{{StoreKey: 18}, {StoreKey: 20, Held: 1}}}
	advertisement.Observe(ready, nil)
	if advertisement.Capacity(18, 2) != 0 || advertisement.Capacity(20, 2) != 1 || advertisement.Capacity(99, 3) != 3 {
		t.Fatalf("ready view: %d %d %d, want 0 1 3", advertisement.Capacity(18, 2), advertisement.Capacity(20, 2),
			advertisement.Capacity(99, 3))
	}
	for name, tick := range map[string]struct {
		result TickResult
		err    error
	}{
		"a failed tick":         {err: errors.New("store")},
		"a blocked plan":        {result: TickResult{Plan: scheduler.Plan{Status: scheduler.PlanBlockedObservation}}},
		"a ready tick, unsized": {result: TickResult{Plan: scheduler.Plan{Status: scheduler.PlanReady}}},
		"a ready tick that errs": {result: TickResult{Plan: scheduler.Plan{Status: scheduler.PlanReady},
			Headroom: []ScaleSetHeadroom{{StoreKey: 20}}}, err: errors.New("commit")},
	} {
		advertisement.Observe(tick.result, tick.err)
		if got := advertisement.Capacity(20, 2); got != 1 {
			t.Fatalf("%s replaced the last view: advertised %d, want 1", name, got)
		}
	}
	advertisement.Observe(TickResult{Plan: scheduler.Plan{Status: scheduler.PlanReady},
		Headroom: []ScaleSetHeadroom{{StoreKey: 20, Held: 1, Fit: 1}}}, nil)
	if advertisement.Capacity(20, 2) != 2 || advertisement.Capacity(18, 2) != 2 {
		t.Fatal("a new ready view replaces the old one whole")
	}
	var absent *ResidualAdvertisement
	absent.Observe(ready, nil)
	if absent.Capacity(18, 2) != 2 {
		t.Fatal("no advertisement advertises the configured capacity")
	}
}
