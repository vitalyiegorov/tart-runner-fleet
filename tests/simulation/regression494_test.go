package simulation_test

import "testing"

// TestIssue494MacHandoffLivenessSeed19 pins the shrunk m4-mac-mini trace from
// issue #494: a builder Mac handoff at the head of the plan and a feasible
// maestro demand behind it, with no admission for 12 ticks.
func TestIssue494MacHandoffLivenessSeed19(t *testing.T) {
	t.Parallel()
	cfg := defaultWorld()
	trace := simTrace{Seed: 19, Config: cfg.Name, Ticks: 100, Events: []simEvent{
		{Tick: 8, Kind: eventArrive, Repo: "ops/fleet", Profile: "medium", Event: "schedule"},
		{Tick: 9, Kind: eventArrive, Repo: "ops/fleet", Profile: "small", Event: "pull_request"},
		{Tick: 11, Kind: eventArrive, Repo: "c/repo", Profile: "large", Event: "push"},
		{Tick: 14, Kind: eventOverrunJob, Count: 6},
		{Tick: 15, Kind: eventArrive, Repo: "b/repo", Profile: "medium", Event: "pull_request"},
		{Tick: 16, Kind: eventArrive, Repo: "ops/fleet", Profile: "small", Event: "pull_request"},
		{Tick: 16, Kind: eventArrive, Repo: "a/repo", Profile: "builder", Event: "pull_request"},
		{Tick: 25, Kind: eventOverrunJob, Count: 3},
		{Tick: 38, Kind: eventArrive, Repo: "c/repo", Profile: "maestro", Event: "schedule"},
	}}
	if findings := runTrace(t, cfg, trace); len(findings) > 0 {
		t.Fatalf("seed 19 reduced replay: %s", findings[0])
	}
}
