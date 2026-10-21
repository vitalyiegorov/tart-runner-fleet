package simulation_test

import (
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// ---------------------------------------------------------------------------
// Issue #388 -- scale sets advertised capacity the shared envelope could not start
// ---------------------------------------------------------------------------
//
// On 2026-10-21 the MacBook (operator budget 10 CPU / 14 GiB / 2 slots) ran one
// maestro guest (4 CPU / 7 GiB). Its residual was 6 CPU / 7 GiB / one slot, so no
// 4x8 guest could start there for up to another ninety minutes. Yet two 4x8 sets
// of two different scopes each kept advertising their static, per-scope truthful
// `maxCapacity`, GitHub summed them, and two jobs were assigned to a node that
// could not run them while sibling nodes sat idle -- 24 and 26 minutes of wait,
// unmovable without a cancel.
//
// macbookWorld is that node. Each 4x8 set belongs to its own scope and serves
// its own repository, exactly as the two incident sets did.
func macbookWorld() worldConfig {
	cfg := defaultWorld()
	cfg.Name = "macbook-10x14-budget"
	cfg.PhysicalCPU = 12
	cfg.PhysicalMemoryMB = 24_576
	cfg.Scheduler.LinuxCapacity = domain.Resources{CPU: 10, MemoryMB: 14_336, Slots: 2}
	cfg.Scheduler.HostBudget = domain.Resources{CPU: 10, MemoryMB: 14_336, Slots: 2}
	all := simProfiles()
	profiles := map[domain.ProfileID]domain.Profile{"maestro": all["maestro"], "large": all["large"]}
	cfg.Scheduler.Profiles = profiles
	cfg.Profiles = sortedProfileIDs(profiles)
	large := all["large"]
	largeLabels := []string{"self-hosted", string(large.Route)}
	cfg.Bindings = []app.Binding{
		{StoreKey: 1, ScaleSetID: 1, Scope: "sim/maestro", Profile: all["maestro"],
			ScaleSetLabels: []string{"self-hosted", string(all["maestro"].Route)}},
		{StoreKey: 2, ScaleSetID: 2, Scope: "sim/fleet", Targets: []string{"c/repo"}, Profile: large,
			ScaleSetLabels: largeLabels},
		{StoreKey: 3, ScaleSetID: 3, Scope: "sim/sudoku", Targets: []string{"b/repo"}, Profile: large,
			ScaleSetLabels: largeLabels},
	}
	cfg.BrokerHonorsAdvertisedCapacity = true
	return cfg
}

// TestIssue388SmallNodeStopsAdvertisingAVectorItCannotStart replays the
// incident. A maestro guest runs a long suite; while it holds 7 GiB of the 14
// GiB budget, a 4x8 job arrives in each of the two other scopes. GitHub must not
// place either on this node -- its sets advertise only what they hold, which is
// nothing -- and once the maestro has exited, a 4x8 job is placed here and runs,
// so the clamp never starves the node of work it can start.
func TestIssue388SmallNodeStopsAdvertisingAVectorItCannotStart(t *testing.T) {
	t.Parallel()
	cfg := macbookWorld()
	trace := simTrace{Seed: 388, Ticks: 120, Config: cfg.Name, Events: []simEvent{
		{Tick: 1, Kind: eventLongJob, Count: 6},
		{Tick: 1, Kind: eventArrive, Repo: "a/repo", Profile: "maestro", Event: "push"},
		{Tick: 12, Kind: eventArrive, Repo: "c/repo", Profile: "large", Event: "pull_request"},
		{Tick: 14, Kind: eventArrive, Repo: "b/repo", Profile: "large", Event: "pull_request"},
		{Tick: 90, Kind: eventArrive, Repo: "b/repo", Profile: "large", Event: "push"},
	}}
	world := newWorld(t, cfg, trace)
	defer world.close()
	if findings := world.run(); len(findings) > 0 {
		t.Fatalf("property violation on the MacBook: %s", findings[0])
	}
	var maestro *simJob
	for _, job := range world.jobs {
		if job.binding == 0 {
			maestro = job
		}
	}
	if maestro == nil || maestro.startedAt.IsZero() || maestro.finishedAt.IsZero() {
		t.Fatalf("the maestro suite never ran to completion, so the scenario proves nothing: %+v", maestro)
	}
	if !maestro.startedAt.Before(simEpoch.Add(12*simTick)) || !maestro.finishedAt.After(simEpoch.Add(14*simTick)) {
		t.Fatalf("the 4x8 jobs must arrive while the maestro holds the envelope: started %s finished %s",
			maestro.startedAt, maestro.finishedAt)
	}
	for _, job := range world.jobs {
		if job.binding == 0 || !job.queuedAt.Before(maestro.finishedAt) {
			continue
		}
		t.Fatalf("GitHub placed %s's 4x8 job on set %d at %s while the maestro held the residual "+
			"(until %s): the node advertised capacity its envelope could not start",
			job.repo, cfg.Bindings[job.binding].ScaleSetID, job.queuedAt.Sub(simEpoch), maestro.finishedAt.Sub(simEpoch))
	}
	if world.toSibling != 2 {
		t.Fatalf("%d jobs went to a sibling, want the two 4x8 jobs that arrived beside the maestro", world.toSibling)
	}
	var later *simJob
	for _, job := range world.jobs {
		if job.binding == 2 {
			later = job
		}
	}
	if later == nil || later.startedAt.IsZero() || later.startedAt.Sub(later.queuedAt) > 10*time.Minute {
		t.Fatalf("once the maestro exited the node must advertise and run a 4x8 again: %+v", later)
	}
}

// TestIssue388BrokerPlacesOnTheOverlappingSetThatAdvertisesRoom: when two sets
// serve the same job, GitHub places it on one that advertised room. A set
// advertising nothing must not divert the job to a sibling while its peer on
// this node still has room.
func TestIssue388BrokerPlacesOnTheOverlappingSetThatAdvertisesRoom(t *testing.T) {
	t.Parallel()
	cfg := macbookWorld()
	large := cfg.Scheduler.Profiles["large"]
	labels := []string{"self-hosted", string(large.Route)}
	cfg.Bindings = []app.Binding{
		{StoreKey: 1, ScaleSetID: 1, Scope: "sim/shared", Profile: large, ScaleSetLabels: labels, SharedLabels: true},
		{StoreKey: 2, ScaleSetID: 2, Scope: "sim/shared", Profile: large, ScaleSetLabels: labels, SharedLabels: true},
	}
	world := newWorld(t, cfg, simTrace{Seed: 388, Ticks: 1, Config: cfg.Name})
	defer world.close()
	world.lastTick = app.TickResult{Headroom: []app.ScaleSetHeadroom{
		{StoreKey: 1, Held: 0, Fit: 0}, {StoreKey: 2, Held: 0, Fit: 1}}}
	job := world.arrive("b/repo", "large", "push")
	if job == nil || job.binding != 1 || world.toSibling != 0 {
		t.Fatalf("job placed on %+v with %d diverted; want the set that advertised room", job, world.toSibling)
	}
	if second := world.arrive("b/repo", "large", "push"); second != nil || world.toSibling != 1 {
		t.Fatalf("with no set advertising room the job goes to a sibling: %+v, %d diverted", second, world.toSibling)
	}
}
