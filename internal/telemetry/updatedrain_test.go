package telemetry

import (
	"strings"
	"testing"
	"time"
)

func drainHealth(t *testing.T) *Health {
	t.Helper()
	health, err := NewHealth(&fakeClock{now: time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC)},
		HealthConfig{Profiles: []string{"small"}})
	if err != nil {
		t.Fatalf("NewHealth: %v", err)
	}
	return health
}

func TestUpdateDrainPassesUntilTheNodeActuallyRefusesAdmission(t *testing.T) {
	health := drainHealth(t)
	if result := health.UpdateDrain(); !result.OK {
		t.Fatalf("unpublished posture failed the check: %v", result.Reasons)
	}
	health.SetUpdateDrain(UpdateDrainMetric{Candidate: "v0.1.510+main.b"})
	if result := health.UpdateDrain(); !result.OK {
		t.Fatalf("a node merely holding a candidate failed the check: %v", result.Reasons)
	}
	if snapshot := health.Snapshot(); snapshot.UpdateDrain == nil || snapshot.UpdateDrain.Candidate != "v0.1.510+main.b" {
		t.Fatal("the published posture never reached the snapshot")
	}
}

func TestUpdateDrainFailsLoudlyAndNamesTheCandidate(t *testing.T) {
	health := drainHealth(t)
	health.SetUpdateDrain(UpdateDrainMetric{Draining: true, Candidate: "v0.1.510+main.b",
		PendingSince: time.Now(), Since: time.Now()})
	result := health.UpdateDrain()
	if result.OK {
		t.Fatal("a node admitting nothing on purpose passed its own check")
	}
	if len(result.Reasons) != 1 || !strings.Contains(result.Reasons[0], "v0.1.510+main.b") {
		t.Fatalf("check did not name the candidate: %v", result.Reasons)
	}
	if !strings.Contains(result.Reasons[0], "running work finishes untouched") {
		t.Fatalf("check does not say the ADR 0011 guarantee holds: %v", result.Reasons)
	}
}

// A drain whose candidate could not be named still explains itself rather than
// printing an empty phrase.
func TestUpdateDrainWithoutACandidateNameStillExplainsItself(t *testing.T) {
	health := drainHealth(t)
	health.SetUpdateDrain(UpdateDrainMetric{Draining: true})
	result := health.UpdateDrain()
	if result.OK || !strings.Contains(result.Reasons[0], "a newer generation") {
		t.Fatalf("nameless drain rendered as %v", result.Reasons)
	}
}

func TestUpdateDrainReachesTheVersionedStatusAndTheMetrics(t *testing.T) {
	health := drainHealth(t)
	envelope := statusEnvelope(health.Snapshot(), "v", "authority", HealthResult{OK: true}, HealthResult{OK: true},
		HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true},
		HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true})
	if envelope.Data.UpdateDrain != nil {
		t.Fatal("an unpublished posture became a published one")
	}
	if check := envelope.Data.EffectiveUpdateDrainCheck(); !check.OK {
		t.Fatalf("an unpublished posture failed its check: %v", check.Reasons)
	}

	health.SetUpdateDrain(UpdateDrainMetric{Draining: true, Candidate: "v0.1.510+main.b",
		PendingSince: time.Date(2026, 8, 29, 8, 0, 0, 0, time.UTC),
		Since:        time.Date(2026, 8, 29, 8, 30, 0, 0, time.UTC)})
	snapshot := health.Snapshot()
	envelope = statusEnvelope(snapshot, "v", "authority", HealthResult{OK: true}, HealthResult{OK: true},
		HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true},
		HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true})
	row := envelope.Data.UpdateDrain
	if row == nil || !row.Draining || row.Candidate != "v0.1.510+main.b" || row.PendingSince.IsZero() || row.Since.IsZero() {
		t.Fatalf("the versioned surface described the drain as %+v", row)
	}
	if check := envelope.Data.EffectiveUpdateDrainCheck(); check.OK {
		t.Fatal("the versioned check passed a draining node")
	}
	if metrics := renderMetrics(snapshot); !strings.Contains(metrics, "fleet_update_drain_active 1") {
		t.Fatalf("the drain is not scrapeable:\n%s", metrics)
	}
}
