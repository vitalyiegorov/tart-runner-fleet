package simulation_test

import "testing"

// ---------------------------------------------------------------------------
// Issue #428 -- a gated node attracted work a sibling with room could have run
// ---------------------------------------------------------------------------
//
// On 2026-10-26 at 06:26Z and 06:47Z two suuudokuuu `trf-linux-amd64-4x8` jobs
// were assigned to Omarchy, whose admission was refused by its memory reserve
// (about 2.7 GiB available, taken by the owner's own workloads), while
// ubuntu-vitalii, serving the same labels, sat idle and admitting. Omarchy's
// idle sets kept ADR 0082's standing offer of one, so GitHub assigned the jobs
// there, and an assigned job does not move.
//
// gatedWorld is that node: production-shaped, with GitHub honouring what each
// set advertises. The host tenant takes all but 1 GiB of memory, below the 2 GiB
// reserve, so the node's own guardrails refuse admission ("memory reserve").
func gatedWorld() worldConfig {
	cfg := defaultWorld()
	cfg.Name = "gated-by-memory-reserve"
	cfg.BrokerHonorsAdvertisedCapacity = true
	return cfg
}

// gatedTenant is a host tenant holding 23 GiB of the world's 24 GiB.
const gatedTenant = 23

// TestIssue428AGatedNodeLeavesTheJobToAnIdleSibling: while the node is gated, a
// job arrives. Its set offers only what it holds, so GitHub gives the job to the
// sibling with room instead of to a node that cannot start it. Once the tenant
// leaves and the gate reopens, the standing offer returns and the next job is
// assigned here and runs.
func TestIssue428AGatedNodeLeavesTheJobToAnIdleSibling(t *testing.T) {
	t.Parallel()
	cfg := gatedWorld()
	trace := simTrace{Seed: 428, Ticks: 60, Config: cfg.Name, Events: []simEvent{
		{Tick: 1, Kind: eventHostTenant, Count: gatedTenant},
		{Tick: 4, Kind: eventArrive, Repo: "a/repo", Profile: "large", Event: "push"},
		{Tick: 20, Kind: eventHostTenant, Count: 0},
		{Tick: 24, Kind: eventArrive, Repo: "a/repo", Profile: "large", Event: "pull_request"},
	}}
	world := newWorld(t, cfg, trace)
	defer world.close()
	if findings := world.run(); len(findings) > 0 {
		t.Fatalf("property violation: %s", findings[0])
	}
	if world.toSibling != 1 {
		t.Fatalf("%d jobs went to the idle sibling, want the one queued while this node was gated", world.toSibling)
	}
	if len(world.jobs) != 1 {
		t.Fatalf("%d jobs were assigned here, want only the one queued after the gate reopened", len(world.jobs))
	}
	if job := world.jobs[0]; job.startedAt.IsZero() || job.queuedAt.Sub(simEpoch) < 20*simTick {
		t.Fatalf("the job assigned after the gate reopened must run here: %+v", job)
	}
}

// TestIssue428EveryMatchingNodeGatedLeavesTheJobUnassigned records the cost the
// rule accepts (ADR 0084). When every node serving a job's labels is behind a
// host-pressure gate at once, no set advertises room and GitHub assigns the job
// to none, exactly as in issue #422. The rule keeps the standing offer on a node
// that is merely full or draining for an update, which is what every node of
// the three #422 incidents was except the swap- or inode-gated ones, so each of
// them still had a set offering one. A job no set takes is the alarm issue
// #422's `doctor` follow-up exists for.
func TestIssue428EveryMatchingNodeGatedLeavesTheJobUnassigned(t *testing.T) {
	t.Parallel()
	cfg := gatedWorld()
	cfg.SiblingsFull = true
	trace := simTrace{Seed: 4281, Ticks: 30, Config: cfg.Name, Events: []simEvent{
		{Tick: 1, Kind: eventHostTenant, Count: gatedTenant},
		{Tick: 4, Kind: eventArrive, Repo: "a/repo", Profile: "large", Event: "push"},
	}}
	world := newWorld(t, cfg, trace)
	defer world.close()
	if findings := world.run(); len(findings) > 0 {
		t.Fatalf("property violation: %s", findings[0])
	}
	if world.unassigned != 1 || len(world.jobs) != 0 {
		t.Fatalf("with every matching node gated the job is assigned to no set: %d unassigned, %d here",
			world.unassigned, len(world.jobs))
	}
}
