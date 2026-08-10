package adminapi

import (
	"encoding/json"
	"strings"
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

// TestEffectiveOccupancySupportsFleetV1Handoff pins the compatibility rule
// EffectiveOccupancy documents. An older daemon never measured occupancy, so it
// omits the field; a CLI that rendered that absence as a failing check would
// report an incident on every fleet that has not been upgraded yet. Absence is
// an unspecified check — OK with no reasons — while a daemon that did measure is
// reported verbatim, including the reasons that name the starving hold.
func TestEffectiveOccupancySupportsFleetV1Handoff(t *testing.T) {
	got := (Status{}).EffectiveOccupancy()
	if !got.OK || got.Reasons == nil || len(got.Reasons) != 0 {
		t.Fatalf("omitted occupancy check = %#v, want an empty non-nil reason list", got)
	}
	want := Check{Reasons: []string{"instance trf-xl-05bbe1c83f21fcd6 of profile xl has held 6 cpu"}}
	if got := (Status{OccupancyCheck: &want}).EffectiveOccupancy(); got.OK || len(got.Reasons) != 1 ||
		got.Reasons[0] != want.Reasons[0] {
		t.Fatalf("reported occupancy check = %#v", got)
	}
}

// TestOccupancyRowsTravelAsAnAdditiveField proves the new per-instance rows are
// additive: a fleet holding nothing omits them entirely, so an older fleetctl
// parses byte-identically to what it always saw, while a fleet holding a vector
// publishes every field an operator judges the hold by.
func TestOccupancyRowsTravelAsAnAdditiveField(t *testing.T) {
	var empty Status
	encoded, err := json.Marshal(empty)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "occupancy") {
		t.Fatalf("a fleet holding nothing named occupancy: %s", encoded)
	}
	held := Status{Occupancy: []Occupancy{{Instance: "trf-xl-05bbe1c83f21fcd6", Profile: "xl",
		Repo: "rnw-community/rnw-community", CPU: 6, MemoryMiB: 12_288, AgeSeconds: 4500, BudgetSeconds: 2700,
		Warned: true, OverBudget: true, StarvesQueuedDemand: true}}}
	encoded, err = json.Marshal(held)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{`"instance":"trf-xl-05bbe1c83f21fcd6"`, `"profile":"xl"`,
		`"repo":"rnw-community/rnw-community"`, `"cpu":6`, `"memoryMiB":12288`, `"ageSeconds":4500`,
		`"budgetSeconds":2700`, `"warned":true`, `"overBudget":true`, `"starvesQueuedDemand":true`} {
		if !strings.Contains(string(encoded), fragment) {
			t.Fatalf("occupancy row missing %s: %s", fragment, encoded)
		}
	}
}

// TestEffectiveProgressSupportsFleetV1Handoff pins the same rule for the drain
// progress check (ADR 0039). An older daemon could not see a stalled drain at
// all — which is the condition issue #233 ran in production unnamed — so its
// absence is an unspecified check rather than a measured pass, and a new CLI
// against it must not start failing every host that has not been upgraded.
func TestEffectiveProgressSupportsFleetV1Handoff(t *testing.T) {
	got := (Status{}).EffectiveProgress()
	if !got.OK || got.Reasons == nil || len(got.Reasons) != 0 {
		t.Fatalf("EffectiveProgress() = %#v, want an unspecified pass with a non-nil reason slice", got)
	}
	want := Check{Reasons: []string{"operation event-drain-x (deregister) has failed 67 times at stop"}}
	if got := (Status{ProgressCheck: &want}).EffectiveProgress(); got.OK || len(got.Reasons) != 1 ||
		got.Reasons[0] != want.Reasons[0] {
		t.Fatalf("a measured progress check was not passed through: %#v", got)
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
