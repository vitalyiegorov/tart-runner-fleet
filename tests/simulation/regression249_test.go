package simulation_test

import "testing"

// ---------------------------------------------------------------------------
// Issue #249 -- the durable store stamped availability from the wall clock
// ---------------------------------------------------------------------------
//
// TestIssue249OrdinaryTeardownIsClaimable pins the trace issue #247's sweep
// shrank the finding to, on the arm it was found on:
//
//	go test ./tests/simulation -run TestSimFuzzContainerNode -seeds=1 -seed-base=93 -sim-ticks=320
//	tick 189: released_vector_conservation: dead guest trf-large-c457426ba00cd5ed
//	          held for 25 ticks past its release bound
//
// Four events, and not one of them is a fault that touches power, liveness, or
// the drain: a job arrives, the broker is late twice, and a guest goes silent.
// The instance's job finishes, `sqlite.enqueueDemandDrain` writes the ordinary
// teardown -- and that operation's `available_at` came from `time.Now()`, thirteen
// real days after the instant this world claims on. Probed directly at tick 185:
//
//	t185 CLAIM virtual=0 wall=1
//	  wall op id=event-drain-trf-large-c457426ba00cd5ed kind=deregister
//	       availableAt=2026-08-18 12:28:33 UTC
//
// So the operation was never claimable, the instance never left `draining`, and
// it held its entire vector for the rest of the run. This is a FLEET defect
// under #242's taxonomy, not a world-model one: property (p) judged correctly,
// and the state the world reached -- a durable operation whose availability the
// claiming clock can never reach -- is one a real fleet reaches the moment its
// two clocks differ. It is pinned as a trace rather than a seed because the
// generator's draw indices move whenever a fault is added, which is exactly how
// this combination went undrawn until #247 added `misreported_power`.
func TestIssue249OrdinaryTeardownIsClaimable(t *testing.T) {
	t.Parallel()
	cfg := containerNodeWorld()
	trace := simTrace{Seed: 93, Ticks: 320, Config: cfg.Name, Events: []simEvent{
		{Tick: 164, Kind: eventArrive, Repo: "b/repo", Profile: "large", Event: "push"},
		{Tick: 165, Kind: eventBrokerDelay, Count: 6},
		{Tick: 169, Kind: eventBrokerDelay, Count: 6},
		{Tick: 178, Kind: eventSilentGuest, Count: 6},
	}}
	if findings := runTrace(t, cfg, trace); len(findings) > 0 {
		t.Fatalf("seed 93's unclaimable teardown is back: %s", findings[0])
	}
}
