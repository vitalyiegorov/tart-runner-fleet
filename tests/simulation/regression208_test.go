package simulation_test

import "testing"

// ---------------------------------------------------------------------------
// Nightly sweep 2026-08-05 -- issue #208
// ---------------------------------------------------------------------------
//
// The nightly long sweep found two property violations the pull-request budget
// (80 seeds, 200 ticks) does not reach. Both are pinned here as the traces the
// harness's own delta debugging reduced them to, not as seed numbers: a shrunk
// trace runs a tenth of the ticks, names the events that matter instead of
// hiding them behind an rng, and cannot be invalidated by a change to the
// generator. The seeds themselves stay covered by the nightly sweep, which
// explores 1..500 on every arm.
//
// Each trace is quoted verbatim from the failure it reproduces, including the
// command line that produced it.

// TestNightly208LivenessWedgeIsFixed pins the seed 210 wedge of the default
// arm:
//
//	go test ./tests/simulation -run TestSimFuzz -seeds=1 -seed-base=210 -sim-ticks=320
//	tick 97: liveness_wedge: no admission for 12 ticks while ops/fleet/1003/1/500003 was feasible
//
// A stalled `builder` (6 CPU / 12288 MB) holds the macOS cohort at its own
// maxActive of one. The aged Linux head is an `xl` that cannot fit in the four
// cores left, so `planLinux` reserves for it. An aged `maestro` (4 CPU /
// 7168 MB) fits those four cores exactly and `mixedProfileCohorts` is on, yet
// `fillMacRemainder` read the LIVE cohort as its target and returned before it
// ever looked at the queue -- twelve ticks in a row, on a quarter-idle machine.
func TestNightly208LivenessWedgeIsFixed(t *testing.T) {
	t.Parallel()
	cfg := defaultWorld()
	trace := simTrace{Seed: 210, Ticks: 110, Config: cfg.Name, Events: []simEvent{
		{Tick: 70, Kind: eventArrive, Repo: "a/repo", Profile: "builder", Event: "push"},
		{Tick: 70, Kind: eventArrive, Repo: "c/repo", Profile: "xl", Event: "push"},
		{Tick: 74, Kind: eventStalledRunner, Count: 4},
		{Tick: 85, Kind: eventArrive, Repo: "ops/fleet", Profile: "maestro", Event: "push"},
	}}
	if findings := runTrace(t, cfg, trace); len(findings) > 0 {
		t.Fatalf("seed 210's wedge is back: %s", findings[0])
	}
}

// TestNightly208BoundedStarvationIsFixed pins the seed 55 pass-over of the
// container-node arm -- the amd64 world of ADR 0034 §3:
//
//	go test ./tests/simulation -run TestSimFuzzContainerNode -seeds=1 -seed-base=55 -sim-ticks=320
//	tick 222: bounded_starvation: aged feasible a/repo/1007/1/500007 (1h43m30s old)
//	          passed over 4 times by one cause, this tick by a/repo/1018/1/500018 (1h10m30s old)
//
// Same repository, same profile, same platform, same size: age is the only
// difference between the two demands, and the wrong one won. A silently
// cancelled run's VM registers, never receives work, is reaped fifteen minutes
// later for a stalled assignment, and releases its demand back into the queue on
// the very tick the vector it needs frees -- where the standing reservation,
// held for a YOUNGER demand, took it. Four times, fifty ticks apart.
func TestNightly208BoundedStarvationIsFixed(t *testing.T) {
	t.Parallel()
	cfg := containerNodeWorld()
	trace := simTrace{Seed: 55, Ticks: 230, Config: cfg.Name, Events: []simEvent{
		{Tick: 3, Kind: eventTartUnavailable, Count: 4},
		{Tick: 4, Kind: eventArrive, Repo: "a/repo", Profile: "small", Event: "pull_request"},
		{Tick: 5, Kind: eventArrive, Repo: "b/repo", Profile: "small", Event: "push"},
		{Tick: 5, Kind: eventSlowBoot, Count: 1},
		{Tick: 7, Kind: eventSilentCancel, Count: 1},
		{Tick: 9, Kind: eventArrive, Repo: "c/repo", Profile: "xl", Event: "schedule"},
		{Tick: 10, Kind: eventArrive, Repo: "b/repo", Profile: "xl", Event: "push"},
		{Tick: 13, Kind: eventArrive, Repo: "c/repo", Profile: "small", Event: "pull_request"},
		{Tick: 14, Kind: eventArrive, Repo: simControlPlaneRepo, Profile: "small", Event: "schedule"},
		{Tick: 15, Kind: eventArrive, Repo: "a/repo", Profile: "xl", Event: "pull_request"},
		{Tick: 15, Kind: eventLoudCancel, Count: 6},
		{Tick: 16, Kind: eventArrive, Repo: simControlPlaneRepo, Profile: "large", Event: "pull_request"},
		{Tick: 16, Kind: eventSilentCancel, Count: 6},
		{Tick: 17, Kind: eventArrive, Repo: "a/repo", Profile: "small", Event: "push"},
		{Tick: 17, Kind: eventArrive, Repo: "b/repo", Profile: "medium", Event: "schedule"},
		{Tick: 19, Kind: eventArrive, Repo: "b/repo", Profile: "medium", Event: "pull_request"},
		{Tick: 20, Kind: eventArrive, Repo: "c/repo", Profile: "large", Event: "push"},
		{Tick: 21, Kind: eventArrive, Repo: "c/repo", Profile: "small", Event: "schedule"},
		{Tick: 21, Kind: eventArrive, Repo: simControlPlaneRepo, Profile: "xl", Event: "schedule"},
		{Tick: 21, Kind: eventArrive, Repo: "a/repo", Profile: "small", Event: "push"},
		{Tick: 29, Kind: eventSiblingSubstitute, Count: 2},
		{Tick: 30, Kind: eventWedgedDrain, Count: 6},
		{Tick: 67, Kind: eventArrive, Repo: simControlPlaneRepo, Profile: "xl", Event: "schedule"},
		{Tick: 67, Kind: eventStalledRunner, Count: 1},
		{Tick: 80, Kind: eventArrive, Repo: "c/repo", Profile: "xl", Event: "push"},
		{Tick: 81, Kind: eventArrive, Repo: "a/repo", Profile: "xl", Event: "pull_request"},
	}}
	if findings := runTrace(t, cfg, trace); len(findings) > 0 {
		t.Fatalf("seed 55's pass-over is back: %s", findings[0])
	}
}
