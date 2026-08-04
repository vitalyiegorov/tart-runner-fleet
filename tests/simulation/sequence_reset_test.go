package simulation_test

import (
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// This file is issue #165 as physics: GitHub restarts the broker's message-id
// sequence for a scale set that has been serving jobs for weeks, and every
// message it then delivers carries an id the fleet's inbox has already recorded
// under different content.
//
// Production, 2026-08-01T18:32Z to 2026-08-04T19:40Z: scale set
// 8077185082566234948 (vitalyiegorov/knee-doctor, linux-large) restarted at
// 100000001 while the inbox held 100000004..100000086 written 13-20 July. From
// the first collision onward ApplyDemandBatch returned ErrConflict, ScaleSet
// .Handle nacked, ADR 0009 recreated the session, GitHub redelivered on its
// five-minute cadence, and the same collision repeated for three days. Every
// linux-large job stayed queued on GitHub. `fleet queues` read 0.

// sequenceResetWorld is one scale set on a Linux node, which is the arrangement
// the incident needs: with a single binding every restarted id lands back on the
// inbox that already holds it, exactly as a per-scale-set sequence does in
// production. Multi-binding arms would interleave the restarted ids across
// ledgers and dilute the collision the incident is about.
func sequenceResetWorld() worldConfig {
	cfg := containerNodeWorld()
	cfg.Name = "sequence-reset-linux-large"
	profile := simProfiles()["large"]
	profiles := map[domain.ProfileID]domain.Profile{"large": profile}
	cfg.Scheduler.Profiles = profiles
	cfg.Profiles = sortedProfileIDs(profiles)
	cfg.Bindings = []app.Binding{{StoreKey: 1, ScaleSetID: 1, Scope: "sim/knee-repo",
		ScaleSetLabels: []string{"self-hosted", string(profile.Route)}, Profile: profile}}
	// Late enough that the binding has a ledger worth colliding with, early
	// enough that a bounded run still has to serve jobs afterwards.
	cfg.SequenceResetAt = 20
	return cfg
}

// TestIncidentBrokerSequenceResetKeepsTheBindingHearing replays the incident end
// to end over the real store, the real DemandCoordinator, and the real engine.
//
// Before the fix the run stops at property (j) within a few ticks of the reset:
// the broker keeps handing the binding its jobs, the binding keeps refusing
// them, and nothing else in the harness notices -- every other oracle reads the
// fleet's own demand view, which is empty, and an empty queue looks exactly like
// an idle fleet. That silence is the operability half of the incident.
func TestIncidentBrokerSequenceResetKeepsTheBindingHearing(t *testing.T) {
	t.Parallel()
	cfg := sequenceResetWorld()
	// A pinned seed, not a sweep: the incident is a physics event, so the trace
	// only has to keep jobs arriving across the restart.
	trace := generateTrace(7, 60, cfg)
	findings := runTrace(t, cfg, trace)
	for _, item := range findings {
		if item.Kind == findingUnheardDemand {
			t.Fatalf("a restarted message-id sequence deafened the binding: %s", item)
		}
	}
	if len(findings) != 0 {
		t.Fatalf("restarting the broker sequence must not break any property: %v", findings)
	}
}

// TestSequenceResetSweepHoldsEveryProperty widens the same physics over a seed
// sweep, so the fix is not pinned to one arrival pattern.
func TestSequenceResetSweepHoldsEveryProperty(t *testing.T) {
	t.Parallel()
	cfg := sequenceResetWorld()
	for seed := int64(1); seed <= 8; seed++ {
		trace := generateTrace(seed, 60, cfg)
		if findings := runTrace(t, cfg, trace); len(findings) != 0 {
			t.Fatalf("seed %d: %v", seed, findings)
		}
	}
}
