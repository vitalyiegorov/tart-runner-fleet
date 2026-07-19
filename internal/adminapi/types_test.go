package adminapi

import (
	"encoding/json"
	"testing"
	"time"
)

func TestStatusEnvelopeJSONContract(t *testing.T) {
	now := time.Date(2026, 7, 12, 20, 0, 0, 0, time.UTC)
	status := StatusEnvelope{APIVersion: APIVersion, Kind: "Status", GeneratedAt: now, Revision: 7, Data: Status{
		ControllerVersion: "v1", ControllerMode: "observe", HostMode: "idle",
		Live: Check{OK: true}, Ready: Check{Reasons: []string{"tick_missing"}}, QueueSLO: &Check{OK: true},
		HostPressure: HostPressure{AvailableMemoryMiB: 8192, FreeDiskGiB: 200, AdmissionAllowed: true, AdmissionReason: "capacity available"},
		Queues:       []Queue{}, Instances: []Instance{}, Observations: []Observation{}, Operations: OperationSummary{},
	}, Warnings: []Warning{}}
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"apiVersion", "kind", "generatedAt", "revision", "data", "warnings"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("missing %s: %s", key, encoded)
		}
	}
}

func TestEffectiveQueueSLOSupportsFleetV1Handoff(t *testing.T) {
	if got := (Status{}).EffectiveQueueSLO(); !got.OK || len(got.Reasons) != 0 {
		t.Fatalf("missing queue SLO = %#v", got)
	}
	want := Check{Reasons: []string{"queue_incident"}}
	if got := (Status{QueueSLO: &want}).EffectiveQueueSLO(); got.OK || len(got.Reasons) != 1 {
		t.Fatalf("reported queue SLO = %#v", got)
	}
}
