package simulation_test

import "testing"

// ---------------------------------------------------------------------------
// Issue #255 -- a slot ADR 0030 holds for the reserved head is not a tier
// inversion
// ---------------------------------------------------------------------------
//
// TestIssue255ReservedRepositorySlotIsNotATierInversion pins the trace the
// tiered arm's seed 82 shrank to, the first time `unreadable_power` was drawn:
//
//	tick 140: tier_inversion: tier 8 ops/fleet/1013/1/500013 (43m30s old) left
//	          waiting while tier 2 b/repo/1021/1/500021 (7m30s old) took an
//	          identical slot
//
// The finding was an ORACLE defect under #242's taxonomy, and the trace carries
// no power fault at all: it reproduced unchanged on the merge base, and the
// power faults only shaped the history that reached the state. What decided that
// tick is ADR 0030's `slack <= 0` clause. `ops/fleet` has a cap of 2, one live
// instance, and holds the aged reserved head `ops/fleet/1008` (a Linux `xl`,
// 59 minutes old, effective tier 11) -- so the repository's one remaining slot
// is the head's, and NOTHING from that repository may be admitted beside it. The
// tier-8 demand was not passed over on order; it was ineligible, and the tier-2
// demand took a slot ADR 0004 forbids the fleet to leave idle.
//
// The whole decision is one tick of `scheduler.PlanTick`, reduced and pinned in
// `internal/scheduler/scheduler_reserved_repo_slot_tier_test.go`, which asserts
// both directions: the last slot is withheld, and one slot of slack hands the
// residual straight back to the tier-8 demand. This trace is what keeps the
// harness honest about the whole-fleet history that reaches it.
func TestIssue255ReservedRepositorySlotIsNotATierInversion(t *testing.T) {
	t.Parallel()
	cfg := tieredWorld()
	trace := simTrace{Seed: 82, Ticks: 320, Config: cfg.Name, Events: []simEvent{
		{Tick: 19, Kind: eventArrive, Repo: "ops/fleet", Profile: "builder", Event: "pull_request"},
		{Tick: 19, Kind: eventArrive, Repo: "ops/fleet", Profile: "medium", Event: "pull_request"},
		{Tick: 20, Kind: eventArrive, Repo: "a/repo", Profile: "maestro", Event: "push"},
		{Tick: 20, Kind: eventArrive, Repo: "a/repo", Profile: "xl", Event: "schedule"},
		{Tick: 21, Kind: eventArrive, Repo: "a/repo", Profile: "small", Event: "pull_request"},
		{Tick: 21, Kind: eventArrive, Repo: "c/repo", Profile: "xl", Event: "pull_request"},
		{Tick: 22, Kind: eventArrive, Repo: "b/repo", Profile: "medium", Event: "pull_request"},
		{Tick: 22, Kind: eventArrive, Repo: "ops/fleet", Profile: "xl", Event: "push"},
		{Tick: 24, Kind: eventArrive, Repo: "b/repo", Profile: "xl", Event: "push"},
		{Tick: 28, Kind: eventArrive, Repo: "b/repo", Profile: "maestro", Event: "pull_request"},
		{Tick: 32, Kind: eventArrive, Repo: "c/repo", Profile: "medium", Event: "pull_request"},
		{Tick: 51, Kind: eventOverrunJob, Count: 5},
		{Tick: 53, Kind: eventArrive, Repo: "b/repo", Profile: "maestro", Event: "schedule"},
		{Tick: 53, Kind: eventArrive, Repo: "ops/fleet", Profile: "maestro", Event: "push"},
		{Tick: 53, Kind: eventArrive, Repo: "c/repo", Profile: "large", Event: "pull_request"},
		{Tick: 53, Kind: eventArrive, Repo: "a/repo", Profile: "large", Event: "schedule"},
		{Tick: 57, Kind: eventArrive, Repo: "c/repo", Profile: "small", Event: "push"},
		{Tick: 57, Kind: eventArrive, Repo: "ops/fleet", Profile: "medium", Event: "pull_request"},
		{Tick: 60, Kind: eventArrive, Repo: "a/repo", Profile: "large", Event: "pull_request"},
		{Tick: 62, Kind: eventArrive, Repo: "b/repo", Profile: "medium", Event: "push"},
		{Tick: 62, Kind: eventArrive, Repo: "ops/fleet", Profile: "large", Event: "push"},
		{Tick: 117, Kind: eventSilentGuest, Count: 3},
		{Tick: 125, Kind: eventArrive, Repo: "b/repo", Profile: "maestro", Event: "push"},
		{Tick: 127, Kind: eventArrive, Repo: "ops/fleet", Profile: "medium", Event: "schedule"},
		{Tick: 131, Kind: eventArrive, Repo: "ops/fleet", Profile: "small", Event: "pull_request"},
	}}
	if findings := runTrace(t, cfg, trace); len(findings) > 0 {
		t.Fatalf("seed 82's reserved repository slot is being read as a tier inversion again: %s", findings[0])
	}
}
