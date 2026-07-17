package app

import (
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

func TestBuildSchedulerConfigAndBindings(t *testing.T) {
	cfg := config.Default()
	cfg.Targets = []config.Target{{Type: "repo", Slug: "o/r", MaxActive: 3, SchedulingClass: domain.SchedulingControlPlane}}
	cfg.GitHub.ScaleSets = []config.ScaleSet{{Profile: "small", ID: 1, MaxCapacity: 4}, {Profile: "builder", ID: 2, MaxCapacity: 1}}
	schedulerConfig := BuildSchedulerConfig(cfg)
	if schedulerConfig.LinuxCapacity != (domain.Resources{CPU: 8, MemoryMB: 16384, Slots: 4}) || schedulerConfig.RepoCaps["o/r"] != 3 {
		t.Fatalf("scheduler config = %#v", schedulerConfig)
	}
	if schedulerConfig.RepoSchedulingClasses["o/r"] != domain.SchedulingControlPlane {
		t.Fatalf("scheduling classes = %#v", schedulerConfig.RepoSchedulingClasses)
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
	cfg.Targets = []config.Target{{Type: "repo", Slug: "o/default", MaxActive: 1}}
	got := BuildSchedulerConfig(cfg)
	if _, ok := got.Profiles["builder"]; ok || len(got.Profiles) != 3 {
		t.Fatalf("profiles = %#v", got.Profiles)
	}
	if got.RepoSchedulingClasses["o/default"] != domain.SchedulingStandard {
		t.Fatalf("default scheduling class = %q", got.RepoSchedulingClasses["o/default"])
	}
}

func TestBuildSchedulerConfigResolvesAutomaticTargetCapacity(t *testing.T) {
	cfg := config.Default()
	cfg.Linux.MaxInstances = 3
	cfg.Targets = []config.Target{{Type: "repo", Slug: "o/auto", AutoMaxActive: true}}
	got := BuildSchedulerConfig(cfg)
	if got.RepoCaps["o/auto"] != 3 {
		t.Fatalf("automatic repository cap = %d, want fleet slots 3", got.RepoCaps["o/auto"])
	}
}

func TestBuildBindingsUseScopedDurableIdentityAndTargets(t *testing.T) {
	cfg := config.Default()
	cfg.GitHub = config.GitHub{App: config.GitHubApp{ClientID: "client", KeychainService: "service", KeychainAccount: "account"},
		SessionOwner: "host", Installations: []config.GitHubInstallation{{Name: "personal", InstallationID: 7}},
		Scopes: []config.GitHubScope{
			{Name: "one", Kind: config.ScopeRepository, ConfigURL: "https://github.com/o/r1", Installation: "personal", Targets: []string{"o/r1"}, ScaleSets: []config.ScaleSet{{Profile: "small", Name: "one-small", ID: 11, MaxCapacity: 1}}},
			{Name: "two", Kind: config.ScopeRepository, ConfigURL: "https://github.com/o/r2", Installation: "personal", Targets: []string{"o/r2"}, ScaleSets: []config.ScaleSet{{Profile: "small", Name: "two-small", ID: 11, MaxCapacity: 1}}},
		}}
	bindings, err := BuildBindings(cfg, BuildSchedulerConfig(cfg))
	if err != nil || len(bindings) != 2 {
		t.Fatalf("bindings = %#v, %v", bindings, err)
	}
	if bindings[0].ScaleSetID != 11 || bindings[0].StoreKey <= 0 || bindings[0].StoreKey == bindings[1].StoreKey || bindings[0].Scope != "one" {
		t.Fatalf("scoped identities = %#v", bindings)
	}
	if !bindings[0].accepts("o/r1") || bindings[0].accepts("o/r2") {
		t.Fatalf("target filter = %#v", bindings[0])
	}
}

func TestBuildBindingsRejectsInvalidScopedScaleSet(t *testing.T) {
	cfg := config.Default()
	cfg.GitHub.ScaleSets = nil
	cfg.GitHub.Scopes = []config.GitHubScope{{Name: "scope", ScaleSets: []config.ScaleSet{{Profile: "missing", ID: 1}}}}
	if _, err := BuildBindings(cfg, BuildSchedulerConfig(cfg)); err == nil {
		t.Fatal("scoped scale set with unknown profile accepted")
	}
	cfg.GitHub.Scopes[0].ScaleSets[0] = config.ScaleSet{Profile: "small", ID: 0}
	if _, err := BuildBindings(cfg, BuildSchedulerConfig(cfg)); err == nil {
		t.Fatal("scoped scale set without provisioned ID accepted")
	}
}
