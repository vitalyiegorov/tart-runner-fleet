package telemetry

import (
	"strings"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
)

func imageToolsEnvelopeHealth(t *testing.T, images []ImageToolsMetric) *Health {
	t.Helper()
	health, _ := newTestHealth(t)
	if err := health.SetImageTools(images); err != nil {
		t.Fatalf("SetImageTools() = %v", err)
	}
	return health
}

func imageToolsStatus(health *Health) adminapi.StatusEnvelope {
	return statusEnvelope(health.Snapshot(), "v", "authority", HealthResult{OK: true}, HealthResult{OK: true},
		HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true},
		HealthResult{OK: true}, health.RunnerVersions(), health.GuestConsole())
}

// TestImageToolDriftReachesTheStatusDocumentAndTheMetrics is issue #414's
// acceptance: a guest image that lacks a version its scale sets pin must be a
// named, alertable finding rather than 315 MB per shard nobody sees.
func TestImageToolDriftReachesTheStatusDocumentAndTheMetrics(t *testing.T) {
	const reason = `macOS base image "macos-tartelet-base" does not carry maestro@2.10.0, pinned by scale sets maestro`
	health := imageToolsEnvelopeHealth(t, []ImageToolsMetric{
		{Platform: "linux", VM: "linux-runner-base", Declared: []string{"node@22.22.2"}},
		{Platform: "macOS", VM: "macos-tartelet-base", Declared: []string{"maestro@2.6.1"},
			Missing: []string{"maestro@2.10.0"}, Reason: reason},
	})
	envelope := imageToolsStatus(health)
	rows := envelope.Data.ImageTools
	if len(rows) != 2 || rows[0].Drift || strings.Join(rows[0].Declared, ",") != "node@22.22.2" {
		t.Fatalf("the covered image must reach the document intact: %#v", rows)
	}
	if !rows[1].Drift || strings.Join(rows[1].Missing, ",") != "maestro@2.10.0" || rows[1].Reason != reason {
		t.Fatalf("the drifted image must reach the document intact: %#v", rows[1])
	}
	check := envelope.Data.EffectiveImageToolCheck()
	if check.OK || len(check.Reasons) != 1 || check.Reasons[0] != reason {
		t.Fatalf("the published check must fail on the stated reason: %+v", check)
	}
	metrics := renderTestMetrics(t, health)
	for _, want := range []string{
		`fleet_image_tools_missing{platform="linux"} 0`,
		`fleet_image_tools_missing{platform="macOS"} 1`,
	} {
		if !strings.Contains(metrics, want) {
			t.Fatalf("metrics missing %q:\n%s", want, metrics)
		}
	}
	if strings.Contains(metrics, "maestro@") {
		t.Fatalf("a tool version must never become a label:\n%s", metrics)
	}
}

// TestImageToolsPublishedEmptyIsAMeasuredPass separates a node that published
// and has nothing to report from a daemon that never looked: the first carries
// a check, the second carries none (AGENTS.md rule 4).
func TestImageToolsPublishedEmptyIsAMeasuredPass(t *testing.T) {
	health := imageToolsEnvelopeHealth(t, nil)
	envelope := imageToolsStatus(health)
	if envelope.Data.ImageTools != nil {
		t.Fatalf("no image must publish no row, got %#v", envelope.Data.ImageTools)
	}
	if envelope.Data.ImageToolCheck == nil || !envelope.Data.ImageToolCheck.OK {
		t.Fatalf("a published empty set is a measured pass: %+v", envelope.Data.ImageToolCheck)
	}
}

// TestImageToolsAreAbsentUntilPublished is the handoff half: nothing recorded
// emits exactly the document and metrics older clients saw, with no check.
func TestImageToolsAreAbsentUntilPublished(t *testing.T) {
	health, _ := newTestHealth(t)
	envelope := imageToolsStatus(health)
	if envelope.Data.ImageTools != nil || envelope.Data.ImageToolCheck != nil {
		t.Fatalf("an unpublished set must not be rendered as a pass: %#v %#v",
			envelope.Data.ImageTools, envelope.Data.ImageToolCheck)
	}
	if strings.Contains(renderTestMetrics(t, health), "fleet_image_tools_missing") {
		t.Fatal("a daemon with nothing recorded must emit no image-tools series")
	}
}

// TestSetImageToolsReplacesTheWholeSet keeps a rebuilt image from being reported
// as drifted forever.
func TestSetImageToolsReplacesTheWholeSet(t *testing.T) {
	health := imageToolsEnvelopeHealth(t, []ImageToolsMetric{
		{Platform: "macOS", VM: "old", Missing: []string{"maestro@2.10.0"}, Reason: "drift"}})
	if err := health.SetImageTools([]ImageToolsMetric{{Platform: "macOS", VM: "new"}}); err != nil {
		t.Fatalf("SetImageTools() = %v", err)
	}
	images := health.Snapshot().ImageTools
	if len(images) != 1 || images[0].VM != "new" || imageToolsResult(health.Snapshot()) == nil ||
		!imageToolsResult(health.Snapshot()).OK {
		t.Fatalf("want the second set to replace the first, got %+v", images)
	}
}

// TestSetImageToolsRejectsAnUnreadableRow refuses rather than truncates, so a
// mangled row is never published as a shorter, plausible one.
func TestSetImageToolsRejectsAnUnreadableRow(t *testing.T) {
	for name, images := range map[string][]ImageToolsMetric{
		"unknown platform":    {{Platform: "windows", VM: "vm"}},
		"duplicate platform":  {{Platform: "linux", VM: "a"}, {Platform: "linux", VM: "b"}},
		"unbounded vm":        {{Platform: "linux", VM: "has space"}},
		"unbounded declared":  {{Platform: "linux", VM: "vm", Declared: []string{"node 22"}}},
		"unbounded missing":   {{Platform: "linux", VM: "vm", Missing: []string{""}, Reason: "x"}},
		"missing w/o reason":  {{Platform: "linux", VM: "vm", Missing: []string{"node@22.22.2"}}},
		"reason w/o missing":  {{Platform: "linux", VM: "vm", Reason: "x"}},
		"overlong reason":     {{Platform: "linux", VM: "vm", Missing: []string{"node@22.22.2"}, Reason: strings.Repeat("x", maxImageToolsReason+1)}},
		"too many tools":      {{Platform: "linux", VM: "vm", Declared: manyTools()}},
		"too many gaps":       {{Platform: "linux", VM: "vm", Missing: manyTools(), Reason: "x"}},
		"second row unusable": {{Platform: "linux", VM: "vm"}, {Platform: "macOS", VM: ""}},
	} {
		t.Run(name, func(t *testing.T) {
			health, _ := newTestHealth(t)
			if err := health.SetImageTools(images); err == nil {
				t.Fatal("want refusal")
			}
			if imageToolsResult(health.Snapshot()) != nil {
				t.Fatal("a refused set must leave nothing published")
			}
		})
	}
}

func manyTools() []string {
	tools := make([]string, 0, maxImageTools+1)
	for index := 0; index <= maxImageTools; index++ {
		tools = append(tools, "tool@"+strings.Repeat("1", index%5+1))
	}
	return tools
}
