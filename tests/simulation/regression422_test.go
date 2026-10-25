package simulation_test

import (
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// ---------------------------------------------------------------------------
// Issue #422 -- a job queued while every node was full was assigned to no set
// ---------------------------------------------------------------------------
//
// On 2026-10-25 every budgie-org Linux set read `available=0 assigned=0` on all
// four nodes while sixteen re-run jobs sat `queued` for over ninety minutes:
// when GitHub queued them, Omarchy and the mini were draining for an update or
// swap-gated, Ubuntu was full and the MacBook swap-gated. Each set advertised
// min(configured, held + fit) (ADR 0073), a full node's fit is zero, and a set
// holding nothing therefore advertised zero. GitHub assigns a job only to a set
// advertising room when it queues it and never looks again, so with every
// matching set at zero the jobs were assigned nowhere and stayed queued after
// the nodes had freed.
//
// fullSharedLabelWorld is the shared-label Mac with room for exactly one
// maestro, and every sibling full as well: a job no set here takes is assigned
// to none.
func fullSharedLabelWorld() worldConfig {
	cfg := sharedLabelWorld()
	cfg.Name = "mac-full-shared-labels"
	cfg.Scheduler.HostBudget = domain.Resources{CPU: 4, MemoryMB: 8_192}
	cfg.SiblingsFull = true
	return cfg
}

// TestIssue422AJobQueuedWhileEveryNodeIsFullIsAssigned: a long suite fills the
// node, and a job arrives in each of the two other scopes while it runs. Each
// idle set keeps a standing offer of one, so GitHub assigns both here; they wait
// in this node's queue and run once the suite has finished. Nothing starts
// beside the suite -- the offer is an assignment, never an admission.
func TestIssue422AJobQueuedWhileEveryNodeIsFullIsAssigned(t *testing.T) {
	t.Parallel()
	cfg := fullSharedLabelWorld()
	trace := simTrace{Seed: 422, Ticks: 120, Config: cfg.Name, Events: []simEvent{
		{Tick: 1, Kind: eventLongJob, Count: 6},
		{Tick: 1, Kind: eventArrive, Repo: "a/repo", Profile: "maestro", Event: "push"},
		{Tick: 12, Kind: eventArrive, Repo: "b/repo", Profile: "maestro", Event: "pull_request"},
		{Tick: 12, Kind: eventArrive, Repo: "c/repo", Profile: "maestro", Event: "pull_request"},
	}}
	world := newWorld(t, cfg, trace)
	defer world.close()
	if findings := world.run(); len(findings) > 0 {
		t.Fatalf("property violation: %s", findings[0])
	}
	if world.unassigned != 0 {
		t.Fatalf("%d jobs were assigned to no set while every node was full; GitHub never re-evaluates them",
			world.unassigned)
	}
	long := longSuite(t, world)
	waited := 0
	for _, job := range world.jobs {
		if job == long {
			continue
		}
		if job.startedAt.IsZero() || job.finishedAt.IsZero() {
			t.Fatalf("%s's job was assigned but never ran: %+v", job.repo, job)
		}
		if job.startedAt.Before(long.finishedAt) {
			t.Fatalf("%s's job started at %s beside the long suite (until %s) on a node with room for one",
				job.repo, job.startedAt.Sub(simEpoch), long.finishedAt.Sub(simEpoch))
		}
		waited++
	}
	if waited != 2 {
		t.Fatalf("%d jobs were assigned here while the node was full, want both", waited)
	}
}

// TestIssue422ABusySetKeepsOneFreeSlot: the job arrives in the very scope whose
// set already runs the long suite. That set held exactly what it advertised --
// one placed guest, no free slot -- so with every sibling full it too was
// assigned to no set. A set whose held work all has a guest keeps one free slot,
// and the job waits here for the suite to finish.
func TestIssue422ABusySetKeepsOneFreeSlot(t *testing.T) {
	t.Parallel()
	cfg := fullSharedLabelWorld()
	trace := simTrace{Seed: 4221, Ticks: 120, Config: cfg.Name, Events: []simEvent{
		{Tick: 1, Kind: eventLongJob, Count: 6},
		{Tick: 1, Kind: eventArrive, Repo: "a/repo", Profile: "maestro", Event: "push"},
		{Tick: 12, Kind: eventArrive, Repo: "a/repo", Profile: "maestro", Event: "pull_request"},
	}}
	world := newWorld(t, cfg, trace)
	defer world.close()
	if findings := world.run(); len(findings) > 0 {
		t.Fatalf("property violation: %s", findings[0])
	}
	if world.unassigned != 0 {
		t.Fatalf("%d jobs were assigned to no set while the scope's own set was busy", world.unassigned)
	}
	long := longSuite(t, world)
	for _, job := range world.jobs {
		if job == long {
			continue
		}
		if job.startedAt.IsZero() || job.finishedAt.IsZero() {
			t.Fatalf("%s's second job was assigned but never ran: %+v", job.repo, job)
		}
		if job.startedAt.Before(long.finishedAt) {
			t.Fatalf("the second job started beside the long suite on a node with room for one")
		}
	}
}
