package simulation_test

import (
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

// ---------------------------------------------------------------------------
// Incident 2026-10-11 -- ADR 0061, a provisioning that retried forever
// ---------------------------------------------------------------------------
//
// On the Mac Studio a macOS guest booted and answered on the network, but `tart
// exec` into it hung, so its bootstrap failed 395 times over seven hours against
// the same VM. The instance never left Reachable, no recovery cause covered
// Reachable, and the node's whole macOS queue waited behind the vector it held.

// wedgedBootstrapTrace is that incident on the builder profile, whose MaxActive
// of 1 makes the second job wait for the vector the wedged guest is holding.
func wedgedBootstrapTrace(cfg worldConfig) simTrace {
	return simTrace{Seed: 20_261_011, Ticks: 160, Config: cfg.Name, Events: []simEvent{
		{Tick: 1, Kind: eventWedgedBootstrap},
		{Tick: 2, Kind: eventArrive, Repo: "a/repo", Profile: "builder", Event: "push"},
		{Tick: 3, Kind: eventArrive, Repo: "a/repo", Profile: "builder", Event: "push"},
		{Tick: 4, Kind: eventStopArrivals},
	}}
}

func (w *world) provisionStalledReclaims() []string {
	var reclaimed []string
	for _, observation := range w.observations {
		for _, operation := range observation.Plan.Operations {
			if operation.Kind == scheduler.OperationDrain && operation.ProvisionStalled {
				reclaimed = append(reclaimed, operation.Instance)
			}
		}
	}
	return reclaimed
}

// TestAWedgedBootstrapIsReprovisionedOnAFreshClone is the incident with the
// mechanism in place: the wedged guest is reclaimed on the provisioning deadline,
// its job is provisioned again on a fresh clone and runs, and the job queued
// behind it runs too.
func TestAWedgedBootstrapIsReprovisionedOnAFreshClone(t *testing.T) {
	t.Parallel()
	cfg := defaultWorld()
	w := newWorld(t, cfg, wedgedBootstrapTrace(cfg))
	defer w.close()
	if findings := w.run(); len(findings) > 0 {
		t.Fatalf("the wedged-bootstrap incident must not violate a property: %s", findings[0])
	}
	// The harness really did reach the incident state. A green run over a trace
	// whose guest bootstrapped normally would prove nothing at all.
	if len(w.wedgedBootstrap) != 1 {
		t.Fatalf("no guest ever wedged at bootstrap: %v", w.wedgedBootstrap)
	}
	reclaimed := w.provisionStalledReclaims()
	if len(reclaimed) != 1 || !w.wedgedBootstrap[reclaimed[0]] {
		t.Fatalf("reclaims = %v, want exactly the wedged guest %v", reclaimed, w.wedgedBootstrap)
	}
	for index, job := range w.jobs {
		if job.status != jobDone {
			t.Fatalf("job %d never ran after the wedged guest was reclaimed: status %s", index, job.status)
		}
	}
	if live := w.liveInstanceCount(); live != 0 {
		t.Fatalf("the fleet never settled: %d live instances", live)
	}
	if final := w.observations[len(w.observations)-1]; final.Queued != 0 {
		t.Fatalf("the queue never drained: %d still queued", final.Queued)
	}
}
