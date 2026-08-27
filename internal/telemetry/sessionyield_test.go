package telemetry

import (
	"strings"
	"testing"
	"time"
)

func yieldHealth(t *testing.T) *Health {
	t.Helper()
	health, err := NewHealth(&fakeClock{now: time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)}, HealthConfig{Profiles: []string{"small"}})
	if err != nil {
		t.Fatalf("NewHealth: %v", err)
	}
	return health
}

func TestSessionYieldPassesUntilTheNodeActuallyWithdraws(t *testing.T) {
	health := yieldHealth(t)
	// A daemon that never published one has not withdrawn.
	if result := health.SessionYield(); !result.OK {
		t.Fatalf("unpublished yield failed the check: %v", result.Reasons)
	}
	health.SetSessionYield(SessionYieldMetric{Yielded: false, Bindings: 4, Withdrawn: 0})
	if result := health.SessionYield(); !result.OK {
		t.Fatalf("a serving node failed the check: %v", result.Reasons)
	}
	if snapshot := health.Snapshot(); snapshot.SessionYield == nil || snapshot.SessionYield.Bindings != 4 {
		t.Fatal("the published posture never reached the snapshot")
	}
}

func TestSessionYieldFailsLoudlyAndNamesTheAdmissionReason(t *testing.T) {
	health := yieldHealth(t)
	health.SetSessionYield(SessionYieldMetric{Yielded: true, Reason: "disk reserve", Bindings: 4, Withdrawn: 4})
	result := health.SessionYield()
	if result.OK {
		t.Fatal("a withdrawn node passed its own check: it is not serving, and silence is what cost eleven hours")
	}
	if len(result.Reasons) != 1 || !strings.Contains(result.Reasons[0], "disk reserve") {
		t.Fatalf("check did not name the admission reason: %v", result.Reasons)
	}
	if strings.Contains(result.Reasons[0], "refused to release") {
		t.Fatalf("a fully released node claimed a stuck session: %v", result.Reasons)
	}
}

func TestSessionYieldDistinguishesAPartialWithdrawal(t *testing.T) {
	health := yieldHealth(t)
	health.SetSessionYield(SessionYieldMetric{Yielded: true, Reason: "load average", Bindings: 4, Withdrawn: 3})
	result := health.SessionYield()
	if result.OK {
		t.Fatal("a partially withdrawn node passed")
	}
	if !strings.Contains(result.Reasons[0], "refused to release") {
		t.Fatalf("a session still held was not reported: %v", result.Reasons)
	}
}

func TestSessionYieldWithoutAnAdmissionReasonStillExplainsItself(t *testing.T) {
	health := yieldHealth(t)
	health.SetSessionYield(SessionYieldMetric{Yielded: true, Bindings: 1, Withdrawn: 1})
	result := health.SessionYield()
	if result.OK || !strings.Contains(result.Reasons[0], "admission refused") {
		t.Fatalf("a reasonless withdrawal rendered as %v", result.Reasons)
	}
}

// The versioned surface must carry the posture, and must carry nothing at all
// when a daemon never published one — an absent row is how an older controller
// says "I cannot withdraw", which is not the same as "I am serving".
func TestSessionYieldReachesTheVersionedStatusAndTheMetrics(t *testing.T) {
	health := yieldHealth(t)
	envelope := statusEnvelope(health.Snapshot(), "v", "authority", HealthResult{OK: true}, HealthResult{OK: true},
		HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true},
		HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true})
	if envelope.Data.SessionYield != nil {
		t.Fatal("an unpublished posture became a published one")
	}
	if check := envelope.Data.EffectiveSessionYieldCheck(); !check.OK {
		t.Fatalf("an unpublished posture failed its check: %v", check.Reasons)
	}

	health.SetSessionYield(SessionYieldMetric{Yielded: true, Reason: "disk reserve", Bindings: 6, Withdrawn: 6,
		Since: time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)})
	snapshot := health.Snapshot()
	envelope = statusEnvelope(snapshot, "v", "authority", HealthResult{OK: true}, HealthResult{OK: true},
		HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true},
		HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true})
	row := envelope.Data.SessionYield
	if row == nil || !row.Yielded || row.Reason != "disk reserve" || row.Bindings != 6 || row.Withdrawn != 6 {
		t.Fatalf("the versioned surface described the withdrawal as %+v", row)
	}
	if check := envelope.Data.EffectiveSessionYieldCheck(); check.OK {
		t.Fatal("the versioned check passed a withdrawn node")
	}
	metrics := renderMetrics(snapshot)
	if !strings.Contains(metrics, "fleet_session_yielded 1") || !strings.Contains(metrics, "fleet_session_bindings_withdrawn 6") {
		t.Fatalf("the withdrawal is not scrapeable:\n%s", metrics)
	}
}
