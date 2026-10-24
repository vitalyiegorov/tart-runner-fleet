package daemon

import (
	"strings"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/telemetry"
)

// TestImageToolsCarryTheConfiguredVerdictToTelemetry is the producer half of
// the tool-drift check (ADR 0079): the daemon transports what internal/config
// decided, and the rows it builds must be ones telemetry accepts — a refused set
// publishes nothing, which doctor reports as "not reported", never as covered.
func TestImageToolsCarryTheConfiguredVerdictToTelemetry(t *testing.T) {
	cfg := config.Default()
	cfg.Linux.BaseImageTools = []string{"node@22.22.2"}
	cfg.MacOS.BaseImageTools = []string{"maestro@2.6.1"}
	cfg.GitHub.ScaleSets = []config.ScaleSet{
		{Profile: "large", Name: "ota", ID: 1, MaxCapacity: 1, RequiresTools: []string{"node@22.22.2"}},
		{Profile: "maestro", Name: "maestro", ID: 2, MaxCapacity: 1, RequiresTools: []string{"maestro@2.10.0"}},
	}
	images := imageTools(cfg)
	if len(images) != 2 || images[0].Platform != "linux" || images[0].Reason != "" ||
		strings.Join(images[0].Declared, ",") != "node@22.22.2" {
		t.Fatalf("linux row = %#v", images)
	}
	if images[1].Platform != "macOS" || strings.Join(images[1].Missing, ",") != "maestro@2.10.0" ||
		!strings.Contains(images[1].Reason, "maestro@2.10.0") {
		t.Fatalf("macOS row must carry the drift verdict: %#v", images[1])
	}
	health, err := telemetry.NewHealth(wallClock{}, telemetry.HealthConfig{})
	if err != nil {
		t.Fatalf("new health: %v", err)
	}
	if err := health.SetImageTools(images); err != nil {
		t.Fatalf("the daemon's own rows must be publishable: %v", err)
	}
	if published := health.Snapshot(); !published.ImageToolsPublished || len(published.ImageTools) != 2 {
		t.Fatalf("published = %+v", published.ImageTools)
	}
}
