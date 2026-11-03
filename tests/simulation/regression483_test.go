package simulation_test

import (
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Issue #483 -- a busy node whose gate flaps kept attracting jobs it could not start
// ---------------------------------------------------------------------------
//
// On 2026-11-03 ubuntu-vitalii ran guests while non-fleet work held it over its
// swap and CPU ceilings. Its admission gate opened for 1 to 23 s every minute or
// two. Session yield correctly kept its sessions, because its running guests
// report through them (#432). But each opening put its sets back on ADR 0082's
// standing offer for a whole long poll, so six jobs were assigned to it and
// waited 35 to 65 minutes while siblings serving the same labels were idle.
//
// flappingWorld is that node: gatedWorld, with the production reopen bound.
func flappingWorld() worldConfig {
	cfg := gatedWorld()
	cfg.Name = "gated-and-flapping"
	cfg.AdmissionReopen = 2 * time.Minute
	return cfg
}

// flappingTenant holds 15 GiB of the world's 24 GiB. Beside one running large
// guest (8 GiB), that leaves 1 GiB, below the 2 GiB reserve.
const flappingTenant = 15

// TestIssue483ABusyNodeWhoseGateFlapsAttractsNoJob: one long suite runs here.
// The tenant arrives, then leaves for a single tick and returns, which is one
// brief opening of the gate. A job queued just after that opening goes to the
// idle sibling. Once the tenant leaves for good and the gate has held open for
// the reopen bound, the next job is assigned here and runs.
func TestIssue483ABusyNodeWhoseGateFlapsAttractsNoJob(t *testing.T) {
	t.Parallel()
	cfg := flappingWorld()
	trace := simTrace{Seed: 483, Ticks: 90, Config: cfg.Name, Events: []simEvent{
		{Tick: 1, Kind: eventLongJob, Count: 6},
		{Tick: 1, Kind: eventArrive, Repo: "a/repo", Profile: "large", Event: "push"},
		{Tick: 8, Kind: eventHostTenant, Count: flappingTenant},
		{Tick: 14, Kind: eventHostTenant, Count: 0},
		{Tick: 15, Kind: eventHostTenant, Count: flappingTenant},
		{Tick: 15, Kind: eventArrive, Repo: "a/repo", Profile: "large", Event: "pull_request"},
		{Tick: 20, Kind: eventHostTenant, Count: 0},
		{Tick: 30, Kind: eventArrive, Repo: "a/repo", Profile: "large", Event: "pull_request"},
	}}
	world := newWorld(t, cfg, trace)
	defer world.close()
	if findings := world.run(); len(findings) > 0 {
		t.Fatalf("property violation: %s", findings[0])
	}
	if world.toSibling != 1 {
		t.Fatalf("%d jobs went to the idle sibling, want the one queued after the brief opening", world.toSibling)
	}
	if len(world.jobs) != 2 {
		t.Fatalf("%d jobs were assigned here, want the long suite and the one queued after the gate reopened for good",
			len(world.jobs))
	}
	if job := world.jobs[1]; job.startedAt.IsZero() || job.queuedAt.Sub(simEpoch) < 30*simTick {
		t.Fatalf("the job assigned after the gate reopened must run here: %+v", job)
	}
}
