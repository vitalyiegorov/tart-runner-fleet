package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
)

// TestGuestTrafficCountersAccumulatePerBoundedSeries is the metric half of
// ADR 0080: bytes add up per {platform, profile, repo}, and an unmeasured job
// adds a job count, never bytes.
func TestGuestTrafficCountersAccumulatePerBoundedSeries(t *testing.T) {
	health, _ := newTestHealth(t)
	for _, metric := range []GuestTrafficMetric{
		{Platform: "linux", Profile: "linux-4x8", Repo: "budgie-at/budgie", Measured: true, ReceiveBytes: 1000, TransmitBytes: 10},
		{Platform: "linux", Profile: "linux-4x8", Repo: "budgie-at/budgie", Measured: true, ReceiveBytes: 500, TransmitBytes: 5},
		{Platform: "linux", Profile: "linux-4x8", Repo: "budgie-at/budgie"},
		{Platform: "macos", Profile: "macos-4x7", Repo: "o/r", Measured: true, ReceiveBytes: 314828521},
	} {
		if err := health.AddGuestTraffic(metric); err != nil {
			t.Fatalf("AddGuestTraffic(%+v) = %v", metric, err)
		}
	}
	metrics := renderTestMetrics(t, health)
	for _, want := range []string{
		`fleet_guest_network_receive_bytes_total{platform="linux",profile="linux-4x8",repo="budgie-at/budgie"} 1500`,
		`fleet_guest_network_transmit_bytes_total{platform="linux",profile="linux-4x8",repo="budgie-at/budgie"} 15`,
		`fleet_guest_network_jobs_total{platform="linux",profile="linux-4x8",repo="budgie-at/budgie",measured="true"} 2`,
		`fleet_guest_network_jobs_total{platform="linux",profile="linux-4x8",repo="budgie-at/budgie",measured="false"} 1`,
		`fleet_guest_network_receive_bytes_total{platform="macos",profile="macos-4x7",repo="o/r"} 314828521`,
	} {
		if !strings.Contains(metrics, want) {
			t.Fatalf("metrics missing %q:\n%s", want, metrics)
		}
	}
}

func TestGuestTrafficCountersRefuseUnboundedLabels(t *testing.T) {
	for name, metric := range map[string]GuestTrafficMetric{
		"platform":       {Platform: "windows", Profile: "p", Repo: "o/r"},
		"profile":        {Platform: "linux", Profile: "has space", Repo: "o/r"},
		"repo":           {Platform: "linux", Profile: "p", Repo: "not-a-repo"},
		"negative bytes": {Platform: "linux", Profile: "p", Repo: "o/r", Measured: true, ReceiveBytes: -1},
		"bytes w/o read": {Platform: "linux", Profile: "p", Repo: "o/r", ReceiveBytes: 1},
	} {
		t.Run(name, func(t *testing.T) {
			health, _ := newTestHealth(t)
			if err := health.AddGuestTraffic(metric); err == nil {
				t.Fatal("want refusal")
			}
		})
	}
	health, _ := newTestHealth(t)
	for index := 0; index < maxGuestTrafficSeries; index++ {
		repo := "o/r" + strings.Repeat("x", index%50) + string(rune('a'+index/50))
		if err := health.AddGuestTraffic(GuestTrafficMetric{Platform: "linux", Profile: "p", Repo: repo}); err != nil {
			t.Fatalf("series %d: %v", index, err)
		}
	}
	if err := health.AddGuestTraffic(GuestTrafficMetric{Platform: "macos", Profile: "p", Repo: "o/new"}); err == nil {
		t.Fatal("a series past the bound must be refused")
	}
	if err := health.AddGuestTraffic(GuestTrafficMetric{Platform: "linux", Profile: "p", Repo: "o/ra"}); err != nil {
		t.Fatalf("an existing series still accumulates past the bound: %v", err)
	}
}

type fakeTrafficReader struct {
	since     time.Time
	jobs      []adminapi.TrafficJob
	truncated bool
	err       error
}

func (r *fakeTrafficReader) GuestTraffic(_ context.Context, since time.Time) ([]adminapi.TrafficJob, bool, error) {
	r.since = since
	return r.jobs, r.truncated, r.err
}

func trafficRequest(t *testing.T, reader TrafficReader, method, query string) *httptest.ResponseRecorder {
	t.Helper()
	health, _ := newTestHealth(t)
	server, err := NewServer(health, ServerConfig{Traffic: reader})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(recorder, httptest.NewRequest(method, adminapi.TrafficPath+query, nil))
	return recorder
}

func TestTrafficEndpointServesJobsAndTotalsForTheWindow(t *testing.T) {
	rx := int64(1345000000)
	reader := &fakeTrafficReader{truncated: true, jobs: []adminapi.TrafficJob{
		{Instance: "trf-a", Repo: "o/r", Profile: "p", Measured: true, ReceiveBytes: &rx, TransmitBytes: &rx}}}
	recorder := trafficRequest(t, reader, http.MethodGet, "?sinceSeconds=86400")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	var envelope adminapi.TrafficEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Kind != adminapi.TrafficKind || !envelope.Data.Truncated || len(envelope.Data.Jobs) != 1 ||
		len(envelope.Data.Totals) != 1 || envelope.Data.Totals[0].ReceiveBytes != rx {
		t.Fatalf("envelope = %+v", envelope)
	}
	if want := envelope.GeneratedAt.Add(-24 * time.Hour); !reader.since.Equal(want) || !envelope.Data.Since.Equal(want) {
		t.Fatalf("since = %v, want %v", reader.since, want)
	}
}

// TestTrafficEndpointNeverAnswersAnUnreadableStoreAsEmpty is AGENTS.md rule 4.
func TestTrafficEndpointNeverAnswersAnUnreadableStoreAsEmpty(t *testing.T) {
	if recorder := trafficRequest(t, &fakeTrafficReader{err: errors.New("closed")}, http.MethodGet,
		"?sinceSeconds=60"); recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
	for _, query := range []string{"", "?sinceSeconds=0", "?sinceSeconds=x", "?sinceSeconds=99999999"} {
		if recorder := trafficRequest(t, &fakeTrafficReader{}, http.MethodGet, query); recorder.Code != http.StatusBadRequest {
			t.Fatalf("query %q status = %d, want 400", query, recorder.Code)
		}
	}
	if recorder := trafficRequest(t, &fakeTrafficReader{}, http.MethodPost, "?sinceSeconds=60"); recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d", recorder.Code)
	}
	if recorder := trafficRequest(t, &fakeTrafficReader{}, http.MethodGet, "?sinceSeconds=60"); recorder.Code != http.StatusOK ||
		!strings.Contains(recorder.Body.String(), `"jobs":[]`) || !strings.Contains(recorder.Body.String(), `"totals":[]`) {
		t.Fatalf("an empty window is empty lists, not null: %d %s", recorder.Code, recorder.Body.String())
	}
	if recorder := trafficRequest(t, nil, http.MethodGet, "?sinceSeconds=60"); recorder.Code != http.StatusNotFound {
		t.Fatalf("a server with no reader must not register the route, got %d", recorder.Code)
	}
}
