package simulation_test

import "testing"

// ---------------------------------------------------------------------------
// Issue #451 -- a background swap-out trickle closed the gate all day
// ---------------------------------------------------------------------------
//
// On 2026-10-28 ubuntu-vitalii held 3907 MiB of swap against a 1024 MiB
// ceiling with 23189 MiB available. The kernel paged out about one page every
// twenty seconds (0.046 pages/s), and the swap guard read any non-zero rate as
// active paging, so the node flapped shut for minutes at a time while 27 jobs
// queued for work only it and omarchy could serve.
//
// TestIssue451ASwapTrickleDoesNotCloseTheGate: work arrives while the host
// carries that residue and trickle. Every job must start inside the residue
// window, as it would on a host with no swap at all.
func TestIssue451ASwapTrickleDoesNotCloseTheGate(t *testing.T) {
	t.Parallel()
	cfg := defaultWorld()
	trace := simTrace{Seed: 451, Ticks: 60, Config: cfg.Name, Events: []simEvent{
		{Tick: 1, Kind: eventSwapResidue, Count: 30},
		{Tick: 1, Kind: eventArrive, Repo: "a/repo", Profile: "small", Event: "push"},
		{Tick: 2, Kind: eventArrive, Repo: "b/repo", Profile: "small", Event: "pull_request"},
	}}
	world := newWorld(t, cfg, trace)
	defer world.close()
	if findings := world.run(); len(findings) > 0 {
		t.Fatalf("property violation: %s", findings[0])
	}
	for _, job := range world.jobs {
		if job.startedAt.IsZero() || job.startedAt.Sub(simEpoch) >= 30*simTick {
			t.Fatalf("%s's job did not start inside the swap trickle: %+v", job.repo, job)
		}
	}
}

// TestIssue451AThrashingHostStillClosesTheGate: the same arrivals on a host
// paging out in earnest. Property (v) proves nothing is spawned for the length
// of the thrash, and the run's other properties prove the demand is admitted
// once it ends -- the guard defers, it never wedges.
func TestIssue451AThrashingHostStillClosesTheGate(t *testing.T) {
	t.Parallel()
	cfg := defaultWorld()
	trace := simTrace{Seed: 4510, Ticks: 60, Config: cfg.Name, Events: []simEvent{
		{Tick: 1, Kind: eventSwapThrash, Count: 6},
		{Tick: 1, Kind: eventArrive, Repo: "a/repo", Profile: "small", Event: "push"},
		{Tick: 2, Kind: eventArrive, Repo: "b/repo", Profile: "small", Event: "pull_request"},
	}}
	world := newWorld(t, cfg, trace)
	defer world.close()
	if findings := world.run(); len(findings) > 0 {
		t.Fatalf("property violation: %s", findings[0])
	}
	for _, job := range world.jobs {
		if job.startedAt.IsZero() {
			t.Fatalf("%s's job never ran once the thrash ended: %+v", job.repo, job)
		}
		if job.startedAt.Sub(simEpoch) < 6*simTick {
			t.Fatalf("%s's job started at %s, inside the thrash", job.repo, job.startedAt.Sub(simEpoch))
		}
	}
}
