package simulation_test

import "testing"

// ---------------------------------------------------------------------------
// Issue #480 -- swap full of cold pages closed the gate on an idle node
// ---------------------------------------------------------------------------
//
// At 2026-11-19 08:56Z ubuntu-vitalii's swap was full, 8191 of 8191 MiB
// against a 1024 MiB ceiling, and the kernel rotated cold pages out at about
// 50 a second. Both clauses of ADR 0089's swap guard held, so the gate stayed
// closed and the node ran no guest, with 38 GiB available. Memory stall (PSI
// `some avg10`) was 0.54%: no task was waiting on memory.
//
// TestIssue480ColdSwapResidueKeepsTheNodesOffer: work arrives through the
// residue on a node with the production reopen bound. The gate never refuses,
// so no refusal episode begins, every set keeps its offer, no job is handed to
// the sibling, and every job starts inside the residue window.
func TestIssue480ColdSwapResidueKeepsTheNodesOffer(t *testing.T) {
	t.Parallel()
	cfg := flappingWorld()
	trace := simTrace{Seed: 480, Ticks: 60, Config: cfg.Name, Events: []simEvent{
		{Tick: 1, Kind: eventSwapColdResidue, Count: 40},
		{Tick: 2, Kind: eventArrive, Repo: "a/repo", Profile: "small", Event: "push"},
		{Tick: 10, Kind: eventArrive, Repo: "b/repo", Profile: "small", Event: "pull_request"},
		{Tick: 20, Kind: eventArrive, Repo: "a/repo", Profile: "large", Event: "pull_request"},
	}}
	world := newWorld(t, cfg, trace)
	defer world.close()
	if findings := world.run(); len(findings) > 0 {
		t.Fatalf("property violation: %s", findings[0])
	}
	for _, observation := range world.observations {
		if pressure := observation.Host.Pressure; !pressure.AdmissionAllowed && pressure.AdmissionReason == "swap pressure" {
			t.Fatalf("tick %d: cold swap residue closed the gate", observation.Tick)
		}
	}
	if world.toSibling != 0 || len(world.jobs) != 3 {
		t.Fatalf("%d jobs went to the sibling and %d were assigned here, want all three here",
			world.toSibling, len(world.jobs))
	}
	for _, job := range world.jobs {
		if job.startedAt.IsZero() || job.startedAt.Sub(simEpoch) >= 40*simTick {
			t.Fatalf("%s's job did not start inside the cold residue: %+v", job.repo, job)
		}
	}
}
