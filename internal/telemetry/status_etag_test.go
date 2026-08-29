package telemetry

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The status endpoint is polled continuously — by the updater's quiescence
// check, by `fleet status`, by any operator loop — so its ETag is what keeps
// that polling from re-serialising the whole envelope every time. A revision
// that has not moved must answer 304 and nothing else.
func TestStatusEndpointAnswers304ForAnUnchangedRevision(t *testing.T) {
	health, err := NewHealth(&fakeClock{now: time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC)},
		HealthConfig{Profiles: []string{"small"}})
	if err != nil {
		t.Fatalf("NewHealth: %v", err)
	}
	handler := statusHandler(health, "v0.1.510", "authority")

	first := httptest.NewRecorder()
	handler(first, httptest.NewRequest(http.MethodGet, "/status", nil))
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200", first.Code)
	}
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("status served no ETag, so every poll re-serialises the envelope")
	}

	repeat := httptest.NewRequest(http.MethodGet, "/status", nil)
	repeat.Header.Set("If-None-Match", etag)
	unchanged := httptest.NewRecorder()
	handler(unchanged, repeat)
	if unchanged.Code != http.StatusNotModified {
		t.Fatalf("unchanged revision = %d, want 304", unchanged.Code)
	}
	if body := unchanged.Body.Len(); body != 0 {
		t.Fatalf("a 304 carried %d bytes of body", body)
	}

	// Any published fact moves the revision, so the same conditional request
	// now has to be answered in full.
	health.SetUpdateDrain(UpdateDrainMetric{Draining: true, Candidate: "v0.1.511"})
	moved := httptest.NewRecorder()
	handler(moved, repeat)
	if moved.Code != http.StatusOK {
		t.Fatalf("changed revision = %d, want 200 — a stale ETag must not hide a new posture", moved.Code)
	}
}
