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
