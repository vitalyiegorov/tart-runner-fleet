package daemon

import (
	"strings"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/telemetry"
)

// TestRunnerImagesCarryTheConfiguredVerdict is the producer half of the floor
// check: the daemon transports what internal/config decided and never forms a
// second opinion, so the metric, the doctor finding and the configuration file
// cannot disagree about which image GitHub is about to refuse.
func TestRunnerImagesCarryTheConfiguredVerdict(t *testing.T) {
	cfg := config.Default()
	cfg.Linux.BaseVM = "linux-runner-base-go"
	cfg.Linux.BaseImageRunnerVersion = "2.335.1"
	cfg.MacOS.Enabled = true
	cfg.MacOS.BaseVM = "macos-tartelet-base-go"
	cfg.MacOS.BaseImageRunnerVersion = "2.336.0"
	cfg.RunnerVersionFloor = "2.336.0"

	images := runnerImages(cfg)
	if len(images) != 2 {
		t.Fatalf("want one row per image this node boots, got %#v", images)
	}
	if images[0].Platform != "linux" || images[0].Version != "2.335.1" || images[0].Floor != "2.336.0" {
		t.Fatalf("linux row = %#v", images[0])
	}
	if !strings.Contains(images[0].Reason, "below the 2.336.0 floor") {
		t.Fatalf("want the linux row to carry the floor verdict, got %q", images[0].Reason)
	}
	if images[1].Platform != "macOS" || images[1].Reason != "" {
		t.Fatalf("a current image carries no reason: %#v", images[1])
	}
}

// TestRunnerImagesReachTelemetry proves the DTO the daemon builds is one the
// telemetry setter accepts. A refused set publishes nothing, and nothing renders
// as "no image is behind" — the exact silence issue #206 was filed about — so
// the two shapes agreeing is load-bearing rather than incidental.
func TestRunnerImagesReachTelemetry(t *testing.T) {
	cfg := config.Default()
	cfg.Linux.BaseVM = "linux-runner-base-go"
	cfg.MacOS.Enabled = true
	cfg.MacOS.BaseVM = "macos-tartelet-base-go"

	health, err := telemetry.NewHealth(wallClock{}, telemetry.HealthConfig{})
	if err != nil {
		t.Fatalf("new health: %v", err)
	}
	if err := health.SetRunnerImages(runnerImages(cfg)); err != nil {
		t.Fatalf("the daemon's own rows must be publishable: %v", err)
	}
	result := health.RunnerVersions()
	if result.OK || len(result.Reasons) != 2 {
		t.Fatalf("a node that declares no version for either image is not compliant: %+v", result)
	}
}

// TestRunnerImagesSkipTheImageANodeNeverBoots keeps the check readable on the
// Linux-only nodes: an image nothing is routed to is not a compliance hole.
func TestRunnerImagesSkipTheImageANodeNeverBoots(t *testing.T) {
	cfg := config.Default()
	cfg.Linux.BaseVM = "linux-runner-base-go"
	cfg.Linux.BaseImageRunnerVersion = "2.336.0"
	cfg.MacOS.Enabled = false

	images := runnerImages(cfg)
	if len(images) != 1 || images[0].Platform != "linux" {
		t.Fatalf("want only the linux row, got %#v", images)
	}
	if images[0].Floor != config.DefaultRunnerVersionFloor || images[0].Reason != "" {
		t.Fatalf("want a compliant row judged against the shipped floor, got %#v", images[0])
	}
}
