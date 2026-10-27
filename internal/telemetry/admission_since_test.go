package telemetry

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestAdmissionSinceReachesTheVersionedStatus is issue #436. A gate that flaps
// restarts the yield clock and makes every "delivered while gated" sighting
// ambiguous unless status says when the current admission state began.
func TestAdmissionSinceReachesTheVersionedStatus(t *testing.T) {
	health, _ := newTestHealth(t)
	since := time.Date(2026, 10, 27, 8, 28, 0, 0, time.UTC)
	pressure := studioPressure()
	pressure.AdmissionSince = since
	if err := health.SetHostPressure(pressure); err != nil {
		t.Fatalf("SetHostPressure: %v", err)
	}
	envelope := statusEnvelope(health.Snapshot(), "v", "authority", HealthResult{OK: true}, HealthResult{OK: true},
		HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true},
		HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true})
	if got := envelope.Data.HostPressure.AdmissionSince; !got.Equal(since) {
		t.Fatalf("admissionSince=%v, want %v", got, since)
	}
	encoded, err := json.Marshal(envelope.Data.HostPressure)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"admissionSince":"2026-10-27T08:28:00Z"`) {
		t.Fatalf("the versioned field is missing: %s", encoded)
	}

	// A host never observed has no state to date; the field is omitted rather
	// than published as the zero time.
	empty, err := json.Marshal(statusEnvelope(Snapshot{}, "v", "observe", HealthResult{OK: true},
		HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true},
		HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true},
		HealthResult{OK: true}).Data.HostPressure)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(empty), "admissionSince") {
		t.Fatalf("an unobserved gate published a start time: %s", empty)
	}
}
