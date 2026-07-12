package app

import (
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

func TestBuildSchedulerConfigAndBindings(t *testing.T) {
	cfg := config.Default()
	cfg.Targets = []config.Target{{Type: "repo", Slug: "o/r", MaxActive: 3}}
	cfg.GitHub.ScaleSets = []config.ScaleSet{{Profile: "small", ID: 1, MaxCapacity: 4}, {Profile: "builder", ID: 2, MaxCapacity: 1}}
	schedulerConfig := BuildSchedulerConfig(cfg)
	if schedulerConfig.LinuxCapacity != (domain.Resources{CPU: 8, MemoryMB: 16384, Slots: 4}) || schedulerConfig.RepoCaps["o/r"] != 3 {
		t.Fatalf("scheduler config = %#v", schedulerConfig)
	}
	if schedulerConfig.Profiles["small"].Route != "linux-small" || schedulerConfig.Profiles["builder"].Platform != domain.PlatformMacOS {
		t.Fatalf("profiles = %#v", schedulerConfig.Profiles)
	}
	bindings, err := BuildBindings(cfg, schedulerConfig)
	if err != nil || len(bindings) != 2 || bindings[0].ScaleSetID != 1 || bindings[1].Profile.ID != "builder" {
		t.Fatalf("bindings = %#v, %v", bindings, err)
	}
}

func TestBuildBindingsRejectsUnknownAndInvalidScaleSet(t *testing.T) {
	cfg := config.Default()
	schedulerConfig := BuildSchedulerConfig(cfg)
	for _, scaleSet := range []config.ScaleSet{{Profile: "missing", ID: 1}, {Profile: "small", ID: 0}} {
		cfg.GitHub.ScaleSets = []config.ScaleSet{scaleSet}
		if _, err := BuildBindings(cfg, schedulerConfig); err == nil {
			t.Fatalf("scale set %#v accepted", scaleSet)
		}
	}
}

func TestBuildSchedulerConfigWithoutMacOS(t *testing.T) {
	cfg := config.Default()
	cfg.MacOS = config.MacOS{}
	got := BuildSchedulerConfig(cfg)
	if _, ok := got.Profiles["builder"]; ok || len(got.Profiles) != 3 {
		t.Fatalf("profiles = %#v", got.Profiles)
	}
}
