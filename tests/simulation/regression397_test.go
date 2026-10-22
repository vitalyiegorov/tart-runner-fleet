package simulation_test

import (
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// ---------------------------------------------------------------------------
// Issue #397 -- a node offered the same free slot once per scope
// ---------------------------------------------------------------------------
//
// The Mac mini and the Mac Studio both serve seven scopes, and every scope binds
// one maestro set on each of them with the same labels (ADR 0034 as amended).
// GitHub fills each set to the capacity it last advertised, and before this fix
// each set advertised what it held plus the node's WHOLE residual, computed once
// per set. A node with one free slot was therefore offered it seven times. Over
// 82 hours of 2026-10-19..22 the fleet DBs show 80 maestro jobs placed on a node
// with no free slot while the other Mac had one, and 22 slot-hours in which a
// Mac sat idle with nothing of its own queued while its sibling's queue waited.
//
// sharedLabelWorld is one of those Macs. Its sibling is the node GitHub places a
// job on when no set here advertises room for it (`toSibling`), which is the
// other half of the incident: the job is not lost, it is run elsewhere.
func sharedLabelWorld() worldConfig {
	cfg := macOSOnlyNodeWorld()
	cfg.Name = "mac-studio-shared-labels"
	cfg.Scheduler.HostBudget = domain.Resources{CPU: 8, MemoryMB: 16_384}
	cfg.Scheduler.LinuxCapacity.Slots = 2
	maestro := simProfiles()["maestro"]
	profiles := map[domain.ProfileID]domain.Profile{"maestro": maestro}
	cfg.Scheduler.Profiles = profiles
	cfg.Profiles = sortedProfileIDs(profiles)
	labels := []string{"self-hosted", string(maestro.Route)}
	cfg.Bindings = nil
	for index, repo := range []string{"a/repo", "b/repo", "c/repo"} {
		cfg.Bindings = append(cfg.Bindings, app.Binding{StoreKey: int64(index + 1), ScaleSetID: int64(index + 1),
			Scope: "sim/" + repo[:1], Targets: []string{repo}, Profile: maestro, ScaleSetLabels: labels,
			SharedLabels: true})
	}
	cfg.Repos = []string{"a/repo", "b/repo", "c/repo"}
	cfg.BrokerHonorsAdvertisedCapacity = true
	return cfg
}

// placedBeside counts the jobs GitHub placed on this node while the long suite
// held one of its two maestro slots.
func placedBeside(world *world, long *simJob) int {
	placed := 0
	for _, job := range world.jobs {
		if job != long && job.queuedAt.Before(long.finishedAt) && !job.queuedAt.Before(long.startedAt) {
			placed++
		}
	}
	return placed
}

func longSuite(t *testing.T, world *world) *simJob {
	t.Helper()
	for _, job := range world.jobs {
		if job.repo == "a/repo" && !job.startedAt.IsZero() && !job.finishedAt.IsZero() {
			return job
		}
	}
	t.Fatal("the long suite never ran, so the scenario proves nothing")
	return nil
}

// TestIssue397ABurstAcrossScopesTakesTheFreeSlotOnce: one maestro suite runs, so
// one slot is free. Four jobs from three scopes arrive in one poll. One may be
// placed here; the other three belong to the sibling, which has room.
func TestIssue397ABurstAcrossScopesTakesTheFreeSlotOnce(t *testing.T) {
	t.Parallel()
	cfg := sharedLabelWorld()
	trace := simTrace{Seed: 397, Ticks: 90, Config: cfg.Name, Events: []simEvent{
		{Tick: 1, Kind: eventLongJob, Count: 6},
		{Tick: 1, Kind: eventArrive, Repo: "a/repo", Profile: "maestro", Event: "push"},
		{Tick: 12, Kind: eventArrive, Repo: "b/repo", Profile: "maestro", Event: "pull_request"},
		{Tick: 12, Kind: eventArrive, Repo: "c/repo", Profile: "maestro", Event: "pull_request"},
		{Tick: 12, Kind: eventArrive, Repo: "a/repo", Profile: "maestro", Event: "pull_request"},
		{Tick: 12, Kind: eventArrive, Repo: "b/repo", Profile: "maestro", Event: "push"},
	}}
	world := newWorld(t, cfg, trace)
	defer world.close()
	if findings := world.run(); len(findings) > 0 {
		t.Fatalf("property violation: %s", findings[0])
	}
	long := longSuite(t, world)
	if placed := placedBeside(world, long); placed != 1 || world.toSibling != 3 {
		t.Fatalf("GitHub placed %d jobs beside the long suite and %d on the sibling; "+
			"the node has one free slot, so want 1 and 3", placed, world.toSibling)
	}
}

// TestIssue397ATickThatCannotPlanDoesNotInviteTheConfiguredMaximum: the same
// burst lands while the node cannot read its inventory. The node cannot start
// anything then, and it used to advertise every set's configured maximum, so the
// burst was placed here in full. It keeps offering what it last knew it could.
func TestIssue397ATickThatCannotPlanDoesNotInviteTheConfiguredMaximum(t *testing.T) {
	t.Parallel()
	cfg := sharedLabelWorld()
	trace := simTrace{Seed: 397, Ticks: 90, Config: cfg.Name, Events: []simEvent{
		{Tick: 1, Kind: eventLongJob, Count: 6},
		{Tick: 1, Kind: eventArrive, Repo: "a/repo", Profile: "maestro", Event: "push"},
		{Tick: 10, Kind: eventTartUnavailable, Count: 4},
		{Tick: 12, Kind: eventArrive, Repo: "b/repo", Profile: "maestro", Event: "pull_request"},
		{Tick: 12, Kind: eventArrive, Repo: "c/repo", Profile: "maestro", Event: "pull_request"},
		{Tick: 12, Kind: eventArrive, Repo: "a/repo", Profile: "maestro", Event: "pull_request"},
		{Tick: 12, Kind: eventArrive, Repo: "b/repo", Profile: "maestro", Event: "push"},
	}}
	world := newWorld(t, cfg, trace)
	defer world.close()
	if findings := world.run(); len(findings) > 0 {
		t.Fatalf("property violation: %s", findings[0])
	}
	long := longSuite(t, world)
	if placed := placedBeside(world, long); placed > 1 {
		t.Fatalf("GitHub placed %d jobs beside the long suite while the node could not plan (%d to the sibling); "+
			"want at most the one free slot", placed, world.toSibling)
	}
}
