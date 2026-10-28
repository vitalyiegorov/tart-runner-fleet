package simulation_test

import (
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// ---------------------------------------------------------------------------
// Issue #439 -- a mixed-platform node offered a Linux vector its macOS guests held
// ---------------------------------------------------------------------------
//
// On 2026-10-28 two pony maestro guests held 8 of the Mac mini's 10 budgeted
// CPUs for up to their occupancy budget. The mini was not gated, only full, so
// ADR 0082's standing offer of one kept its `linux-4x8` sets inviting work: a
// knee-doctor job waited there 37 minutes and a budgie job was placed there
// while the MacBook, serving the same labels, had room. GitHub does not move
// an assigned job. The offer promises a job a wait for this node's own Linux
// work, and no Linux work here was holding the CPUs the 4x8 needed.
//
// mixedMiniWorld is that node: a 10 CPU / 23 GiB budget with every CPU open to
// Linux, one maestro set, and a 4x8 and a 2x4 set of one Linux scope. The host
// is physically larger than the budget, so the budget is the bound every
// admission here meets, as on the mini.
func mixedMiniWorld() worldConfig {
	cfg := defaultWorld()
	cfg.Name = "mini-10x23-mixed"
	cfg.PhysicalMemoryMB = 32_768
	cfg.Scheduler.LinuxCapacity = domain.Resources{CPU: 10, MemoryMB: 23_552, Slots: 4}
	cfg.Scheduler.HostBudget = domain.Resources{CPU: 10, MemoryMB: 23_552}
	all := simProfiles()
	profiles := map[domain.ProfileID]domain.Profile{"maestro": all["maestro"], "large": all["large"],
		"medium": all["medium"]}
	cfg.Scheduler.Profiles = profiles
	cfg.Profiles = sortedProfileIDs(profiles)
	cfg.Bindings = []app.Binding{
		{StoreKey: 1, ScaleSetID: 1, Scope: "sim/maestro", Profile: all["maestro"],
			ScaleSetLabels: []string{"self-hosted", string(all["maestro"].Route)}},
		{StoreKey: 2, ScaleSetID: 2, Scope: "sim/knee", Targets: []string{"b/repo"}, Profile: all["large"],
			ScaleSetLabels: []string{"self-hosted", string(all["large"].Route)}},
		{StoreKey: 3, ScaleSetID: 3, Scope: "sim/knee", Targets: []string{"b/repo"}, Profile: all["medium"],
			ScaleSetLabels: []string{"self-hosted", string(all["medium"].Route)}},
	}
	cfg.BrokerHonorsAdvertisedCapacity = true
	return cfg
}

// TestIssue439MacOSGuestsLeaveTheLinuxVectorToASibling replays the incident.
// Two long maestro suites fill 8 of the 10 CPUs. A 4x8 job queued while they
// run goes to the sibling: no Linux work here could free the CPUs it needs. A
// 2x4 job queued beside them is still assigned here and starts at once. Once
// the suites end, the 4x8 set offers room again and the next 4x8 job runs here.
func TestIssue439MacOSGuestsLeaveTheLinuxVectorToASibling(t *testing.T) {
	t.Parallel()
	cfg := mixedMiniWorld()
	trace := simTrace{Seed: 439, Ticks: 140, Config: cfg.Name, Events: []simEvent{
		{Tick: 1, Kind: eventLongJob, Count: 6},
		{Tick: 1, Kind: eventArrive, Repo: "a/repo", Profile: "maestro", Event: "push"},
		{Tick: 12, Kind: eventLongJob, Count: 6},
		{Tick: 12, Kind: eventArrive, Repo: "a/repo", Profile: "maestro", Event: "pull_request"},
		{Tick: 30, Kind: eventArrive, Repo: "b/repo", Profile: "large", Event: "push"},
		{Tick: 32, Kind: eventArrive, Repo: "b/repo", Profile: "medium", Event: "push"},
		{Tick: 110, Kind: eventArrive, Repo: "b/repo", Profile: "large", Event: "pull_request"},
	}}
	world := newWorld(t, cfg, trace)
	defer world.close()
	if findings := world.run(); len(findings) > 0 {
		t.Fatalf("property violation on the mini: %s", findings[0])
	}
	var suites []*simJob
	for _, job := range world.jobs {
		if job.binding == 0 {
			suites = append(suites, job)
		}
	}
	if len(suites) != 2 {
		t.Fatalf("%d maestro suites ran, want both", len(suites))
	}
	for _, suite := range suites {
		if suite.startedAt.IsZero() || !suite.startedAt.Before(simEpoch.Add(30*simTick)) ||
			!suite.finishedAt.After(simEpoch.Add(32*simTick)) {
			t.Fatalf("both suites must hold the CPUs while the Linux jobs arrive: %+v", suite)
		}
	}
	if world.toSibling != 1 {
		t.Fatalf("%d jobs went to a sibling, want the 4x8 queued while the macOS guests held its CPUs", world.toSibling)
	}
	var medium, later *simJob
	for _, job := range world.jobs {
		switch {
		case job.binding == 1 && job.queuedAt.Before(simEpoch.Add(100*simTick)):
			t.Fatalf("a 4x8 job was assigned here at %s while only macOS guests held its CPUs",
				job.queuedAt.Sub(simEpoch))
		case job.binding == 1:
			later = job
		case job.binding == 2:
			medium = job
		}
	}
	if medium == nil || medium.startedAt.IsZero() || medium.startedAt.Sub(medium.queuedAt) > 10*time.Minute {
		t.Fatalf("the 2x4 job fits beside the suites and must be assigned and run here: %+v", medium)
	}
	if later == nil || later.startedAt.IsZero() || later.startedAt.Sub(later.queuedAt) > 10*time.Minute {
		t.Fatalf("once the suites end the node must advertise and run a 4x8 again: %+v", later)
	}
}
