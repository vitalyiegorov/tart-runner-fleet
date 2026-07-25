package telemetry

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
)

// A drain that cannot progress is a first-class operator fact: the 2026-07-25
// phantom retried for 206 minutes while the only published signal was
// "retrying 1". The bounded failure aggregate must reach both the versioned
// status document and the metrics endpoint, carrying the closed failure code and
// the worst attempt count, and it must reject anything outside that vocabulary.
func TestOperationFailuresReachStatusAndMetrics(t *testing.T) {
	health, clock := newTestHealth(t)
	health.RecordTick(true)
	for _, name := range []string{"github", "host", "tart"} {
		if err := health.RecordObservation(name, ObservationFresh); err != nil {
			t.Fatal(err)
		}
	}
	_ = health.SetMode(ModeMacOS)
	_ = health.SetQueue("macos-maestro", 1, clock.Now())
	_ = health.SetInstances("macos-maestro", 1, 4, 7168)
	health.SetOperations(1, 0)
	_ = health.SetHostPressure(HostPressureMetric{AvailableMemoryMiB: 10240, FreeDiskGiB: 203, CPUIdlePercent: 54,
		LoadAverage: 3, AdmissionAllowed: true, AdmissionReason: "capacity available"})
	if err := health.SetOperationFailures([]OperationFailure{
		{Kind: "deregister", Code: "deregister:runner_busy", Count: 1, Attempts: 397},
	}); err != nil {
		t.Fatal(err)
	}

	snapshot := health.Snapshot()
	if len(snapshot.OperationFailures) != 1 || snapshot.OperationFailures[0].Attempts != 397 {
		t.Fatalf("snapshot failures=%#v", snapshot.OperationFailures)
	}
	snapshot.OperationFailures[0] = OperationFailure{}
	if health.Snapshot().OperationFailures[0].Code != "deregister:runner_busy" {
		t.Fatal("snapshot aliases the failure aggregate")
	}

	server, err := NewServer(health, ServerConfig{ControllerVersion: "v1", ControllerMode: "authority"})
	if err != nil {
		t.Fatal(err)
	}
	statusResponse := request(t, server, http.MethodGet, adminapi.StatusPath)
	defer statusResponse.Body.Close()
	var status adminapi.StatusEnvelope
	if err := json.NewDecoder(statusResponse.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	failures := status.Data.Operations.Failures
	if len(failures) != 1 || failures[0].Kind != "deregister" || failures[0].Code != "deregister:runner_busy" ||
		failures[0].Count != 1 || failures[0].Attempts != 397 {
		t.Fatalf("status failures=%#v", failures)
	}

	metricsResponse := request(t, server, http.MethodGet, "/metrics")
	defer metricsResponse.Body.Close()
	body, err := io.ReadAll(metricsResponse.Body)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`fleet_operation_failures{kind="deregister",code="deregister:runner_busy"} 1`,
		`fleet_operation_failure_attempts{kind="deregister",code="deregister:runner_busy"} 397`,
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("metrics missing %q", want)
		}
	}
}

// The aggregate is published verbatim, so it must be validated at the boundary:
// unbounded codes, unknown kinds, and negative counters can never enter.
func TestOperationFailuresRejectUnboundedInput(t *testing.T) {
	health, _ := newTestHealth(t)
	secret := "ghs_do-not-leak"
	for _, failures := range [][]OperationFailure{
		{{Kind: "deregister", Code: secret, Count: 1, Attempts: 1}},
		{{Kind: secret, Code: "deregister:runner_busy", Count: 1, Attempts: 1}},
		{{Kind: "deregister", Code: "deregister:runner_busy", Count: -1, Attempts: 1}},
		{{Kind: "deregister", Code: "deregister:runner_busy", Count: 1, Attempts: -1}},
		{{Kind: "", Code: "", Count: 1, Attempts: 1}},
	} {
		err := health.SetOperationFailures(failures)
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Fatalf("unsafe failure aggregate accepted: %v", err)
		}
	}
	if err := health.SetOperationFailures(nil); err != nil {
		t.Fatalf("an empty aggregate is the healthy case: %v", err)
	}
	if got := health.Snapshot().OperationFailures; len(got) != 0 {
		t.Fatalf("failures=%#v", got)
	}
}

// Metric label cardinality is bounded by the closed vocabulary, so an aggregate
// larger than that vocabulary can only be a producer regression.
func TestOperationFailuresRejectUnboundedCardinality(t *testing.T) {
	health, _ := newTestHealth(t)
	failures := make([]OperationFailure, maxOperationFailures+1)
	for i := range failures {
		failures[i] = OperationFailure{Kind: "deregister", Code: "deregister:runner_busy", Count: 1, Attempts: 1}
	}
	if err := health.SetOperationFailures(failures); err == nil {
		t.Fatal("unbounded failure cardinality accepted")
	}
}
