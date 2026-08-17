package telemetry

import (
	"strings"
	"testing"
)

func runnerVersionHealth(t *testing.T) *Health {
	t.Helper()
	health, _ := newTestHealth(t)
	return health
}

func TestRunnerVersionsPassesWhenEveryImageIsCompliant(t *testing.T) {
	health := runnerVersionHealth(t)
	if err := health.SetRunnerImages([]RunnerImageMetric{
		{Platform: "linux", VM: "linux-runner-base-go", Version: "2.336.0", Floor: "2.329.0"},
		{Platform: "macOS", VM: "macos-tartelet-base-go", Version: "2.336.0", Floor: "2.329.0"},
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if result := health.RunnerVersions(); !result.OK || len(result.Reasons) != 0 {
		t.Fatalf("want a passing check with no reasons, got %+v", result)
	}
}

// TestRunnerVersionsFailsOnTheReasonTheProducerStated keeps the floor rule in
// one place: telemetry carries the verdict internal/config computed and never
// re-derives it, so the metric, the doctor finding and the configuration cannot
// disagree about which image is behind.
func TestRunnerVersionsFailsOnTheReasonTheProducerStated(t *testing.T) {
	const reason = `linux base image "linux-runner-base-go" carries actions/runner 2.335.1, below the 2.336.0 floor`
	health := runnerVersionHealth(t)
	if err := health.SetRunnerImages([]RunnerImageMetric{
		{Platform: "linux", VM: "linux-runner-base-go", Version: "2.335.1", Floor: "2.336.0", Reason: reason},
		{Platform: "macOS", VM: "macos-tartelet-base-go", Version: "2.336.0", Floor: "2.336.0"},
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	result := health.RunnerVersions()
	if result.OK || len(result.Reasons) != 1 || result.Reasons[0] != reason {
		t.Fatalf("want exactly the producer's reason, got %+v", result)
	}
}

// TestRunnerVersionsIsEmptyBeforeAnythingIsPublished is the handoff half: a
// daemon that has not yet recorded an image must not report a fleet whose images
// are all behind, and must not report one that is fine either.
func TestRunnerVersionsIsEmptyBeforeAnythingIsPublished(t *testing.T) {
	health := runnerVersionHealth(t)
	result := health.RunnerVersions()
	if !result.OK || len(result.Reasons) != 0 {
		t.Fatalf("want a vacuous pass before anything is set, got %+v", result)
	}
	if images := health.Snapshot().RunnerImages; len(images) != 0 {
		t.Fatalf("want no rows before anything is set, got %+v", images)
	}
}

func TestSetRunnerImagesReplacesTheWholeSet(t *testing.T) {
	health := runnerVersionHealth(t)
	if err := health.SetRunnerImages([]RunnerImageMetric{
		{Platform: "linux", VM: "old", Version: "2.300.0", Floor: "2.329.0", Reason: "behind"},
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := health.SetRunnerImages([]RunnerImageMetric{
		{Platform: "linux", VM: "new", Version: "2.336.0", Floor: "2.329.0"},
	}); err != nil {
		t.Fatalf("set again: %v", err)
	}
	snapshot := health.Snapshot()
	if len(snapshot.RunnerImages) != 1 || snapshot.RunnerImages[0].VM != "new" {
		t.Fatalf("want the second set to replace the first, got %+v", snapshot.RunnerImages)
	}
	if !health.RunnerVersions().OK {
		t.Fatalf("want the rebuilt image to clear the finding")
	}
}

// TestSetRunnerImagesRejectsAnUnreadableRow refuses rather than truncates, for
// the reason SetOccupancy does: a mangled row that silently became "no rows"
// would read as "every image is compliant", which is the exact failure this
// check exists to remove.
func TestSetRunnerImagesRejectsAnUnreadableRow(t *testing.T) {
	for name, images := range map[string][]RunnerImageMetric{
		"unknown platform": {{Platform: "windows", VM: "vm", Version: "2.336.0", Floor: "2.329.0"}},
		"unbounded vm":     {{Platform: "linux", VM: strings.Repeat("a", 200), Version: "2.336.0", Floor: "2.329.0"}},
		"empty floor":      {{Platform: "linux", VM: "vm", Version: "2.336.0"}},
		"duplicate platform": {
			{Platform: "linux", VM: "one", Version: "2.336.0", Floor: "2.329.0"},
			{Platform: "linux", VM: "two", Version: "2.336.0", Floor: "2.329.0"}},
		"unbounded reason": {{Platform: "linux", VM: "vm", Version: "2.336.0", Floor: "2.329.0",
			Reason: strings.Repeat("a", 4096)}},
		"unreadable version": {{Platform: "linux", VM: "vm", Version: "2.336.0 or so", Floor: "2.329.0"}},
	} {
		t.Run(name, func(t *testing.T) {
			health := runnerVersionHealth(t)
			if err := health.SetRunnerImages(images); err == nil {
				t.Fatalf("want a refusal, got none")
			}
			if rows := health.Snapshot().RunnerImages; len(rows) != 0 {
				t.Fatalf("want a refused set to publish nothing, got %+v", rows)
			}
		})
	}
}

// TestRunnerImagesReachTheStatusDocumentAndTheMetrics is the observability half:
// the acceptance criterion of issue #206 is that the version in service can be
// read without SSH-ing into a guest, so it must survive the projection into the
// versioned DTO and the bounded metrics endpoint.
func TestRunnerImagesReachTheStatusDocumentAndTheMetrics(t *testing.T) {
	const reason = `linux base image "linux-runner-base-go" carries actions/runner 2.335.1, below the 2.336.0 floor`
	health := runnerVersionHealth(t)
	if err := health.SetRunnerImages([]RunnerImageMetric{
		{Platform: "linux", VM: "linux-runner-base-go", Version: "2.335.1", Floor: "2.336.0", Reason: reason},
		{Platform: "macOS", VM: "macos-tartelet-base-go", Version: "2.336.0", Floor: "2.336.0"},
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	envelope := statusEnvelope(health.Snapshot(), "v", "authority", HealthResult{OK: true}, HealthResult{OK: true},
		HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true},
		HealthResult{OK: true}, health.RunnerVersions())
	rows := envelope.Data.RunnerImages
	if len(rows) != 2 || rows[0].Version != "2.335.1" || !rows[0].BelowFloor || rows[0].Reason != reason {
		t.Fatalf("the behind image must reach the document intact: %#v", rows)
	}
	if rows[1].BelowFloor || rows[1].Reason != "" {
		t.Fatalf("a current image is not below the floor: %#v", rows[1])
	}
	if check := envelope.Data.EffectiveRunnerVersionCheck(); check.OK || len(check.Reasons) != 1 {
		t.Fatalf("the published check must carry the finding: %+v", check)
	}

	// The verdict is the metric; the version strings are deliberately not labels.
	metrics := renderTestMetrics(t, health)
	for _, want := range []string{
		`fleet_runner_image_below_floor{platform="linux"} 1`,
		`fleet_runner_image_below_floor{platform="macOS"} 0`,
	} {
		if !strings.Contains(metrics, want) {
			t.Fatalf("metrics missing %q:\n%s", want, metrics)
		}
	}
	if strings.Contains(metrics, "2.335.1") {
		t.Fatalf("a version must never become a label; it churns the series on every rebuild:\n%s", metrics)
	}
}

// TestRunnerImagesAreAbsentUntilPublished keeps the handoff shape: a daemon that
// recorded nothing emits exactly the document and the metrics older clients saw.
func TestRunnerImagesAreAbsentUntilPublished(t *testing.T) {
	health := runnerVersionHealth(t)
	envelope := statusEnvelope(health.Snapshot(), "v", "authority", HealthResult{OK: true}, HealthResult{OK: true},
		HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true},
		HealthResult{OK: true}, health.RunnerVersions())
	if envelope.Data.RunnerImages != nil {
		t.Fatalf("nil must stay nil; got %#v", envelope.Data.RunnerImages)
	}
	if strings.Contains(renderTestMetrics(t, health), "fleet_runner_image_below_floor") {
		t.Fatal("a daemon with nothing recorded must emit no runner-image series")
	}
}
