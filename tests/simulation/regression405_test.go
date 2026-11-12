package simulation_test

import "testing"

// Cancellation before the first REST snapshot cannot accrue corroboration.
// Positive terminal-run evidence must nevertheless empty the fleet.
func TestIssue405UncorroboratedSilentCancelQuiesces(t *testing.T) {
	cfg := defaultWorld()
	trace := simTrace{Seed: 405, Ticks: 100, Config: cfg.Name, Events: []simEvent{
		{Tick: 1, Kind: eventArrive, Repo: "a/repo", Profile: "large", Event: "push"},
		{Tick: 1, Kind: eventSilentCancel},
		{Tick: 2, Kind: eventStopArrivals},
	}}
	w := newWorld(t, cfg, trace)
	defer w.close()
	if findings := w.run(); len(findings) > 0 {
		t.Fatalf("property violation: %s", findings[0])
	}
	final := w.observations[len(w.observations)-1]
	if final.Queued != 0 || w.liveInstanceCount() != 0 {
		t.Fatalf("ghost remains: queued=%d live=%d", final.Queued, w.liveInstanceCount())
	}
}
