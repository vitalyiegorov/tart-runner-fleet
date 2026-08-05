package app

import (
	"strings"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// TestValidateBindingsCatchesConfigsThatValidateButCrashTheDaemon pins the
// incident: a config whose scale set references a nonexistent profile ("xl") or
// carries a non-positive durable ID passes config.Validate yet would crash-loop
// the authority daemon at BuildBindings. ValidateBindings must reject it, with a
// distinct, actionable message per cause naming the offending scale set.
func TestValidateBindingsCatchesConfigsThatValidateButCrashTheDaemon(t *testing.T) {
	for _, tc := range []struct {
		name     string
		scaleSet config.ScaleSet
		want     string
	}{
		{"unknown profile", config.ScaleSet{Profile: "xl", Name: "fleet-repo-xl", ID: 1, MaxCapacity: 1}, "unknown profile"},
		{"non-positive id", config.ScaleSet{Profile: "small", Name: "fleet-repo-small", ID: 0, MaxCapacity: 1}, "non-positive durable ID"},
	} {
		cfg := config.Default()
		cfg.GitHub.ScaleSets = []config.ScaleSet{tc.scaleSet}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("%s: config.Validate rejected the config, so the gap cannot be demonstrated: %v", tc.name, err)
		}
		err := ValidateBindings(cfg)
		if err == nil {
			t.Fatalf("%s: ValidateBindings accepted a config that would crash the daemon", tc.name)
		}
		if !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), tc.scaleSet.Name) {
			t.Fatalf("%s: error %q missing %q or scale set name %q", tc.name, err, tc.want, tc.scaleSet.Name)
		}
	}
	if err := ValidateBindings(config.Default()); err != nil {
		t.Fatalf("ValidateBindings rejected the default config: %v", err)
	}
}

func TestBuildSchedulerConfigAndBindings(t *testing.T) {
	cfg := config.Default()
	cfg.MacOS.AdmissionPolicy = config.MacOSAdmissionExclusive
	cfg.MacOS.MixedPlatformAdmission = true
	cfg.Targets = []config.Target{{Type: "repo", Slug: "o/r", MaxActive: 3, SchedulingClass: domain.SchedulingControlPlane}}
	cfg.GitHub.ScaleSets = []config.ScaleSet{
		{Profile: "small", ID: 1, MaxCapacity: 4, Labels: []string{"self-hosted", "linux-tiered", "linux-small"}},
		{Profile: "builder", ID: 2, MaxCapacity: 1, Labels: []string{"self-hosted", "macOS", "ARM64", "macos-builder"}},
	}
	schedulerConfig := BuildSchedulerConfig(cfg)
	if schedulerConfig.LinuxCapacity != (domain.Resources{CPU: 8, MemoryMB: 16384, Slots: 4}) || schedulerConfig.RepoCaps["o/r"] != 3 {
		t.Fatalf("scheduler config = %#v", schedulerConfig)
	}
	if schedulerConfig.RepoSchedulingClasses["o/r"] != domain.SchedulingControlPlane {
		t.Fatalf("scheduling classes = %#v", schedulerConfig.RepoSchedulingClasses)
	}
	if !schedulerConfig.MacOSExclusive {
		t.Fatal("macos-exclusive admission policy was not mapped to the scheduler")
	}
	if !schedulerConfig.MixedPlatformAdmission {
		t.Fatal("mixed-platform admission flag was not mapped to the scheduler")
	}
	if schedulerConfig.Profiles["small"].Route != "linux-small" || schedulerConfig.Profiles["builder"].Platform != domain.PlatformMacOS {
		t.Fatalf("profiles = %#v", schedulerConfig.Profiles)
	}
	bindings, err := BuildBindings(cfg, schedulerConfig)
	if err != nil || len(bindings) != 2 || bindings[0].ScaleSetID != 1 || bindings[1].Profile.ID != "builder" ||
		len(bindings[0].ScaleSetLabels) != 4 || bindings[0].ScaleSetLabels[2] != "linux-small" ||
		bindings[0].ScaleSetLabels[3] != "trf-linux-arm64-1x2" || bindings[1].ScaleSetLabels[4] != "trf-macos-arm64-8x12" {
		t.Fatalf("bindings = %#v, %v", bindings, err)
	}
	cfg.GitHub.ScaleSets[0].Labels[2] = "mutated"
	if bindings[0].ScaleSetLabels[2] != "linux-small" {
		t.Fatalf("binding labels alias configuration: %#v", bindings[0].ScaleSetLabels)
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

func TestBuildBindingsUseScopedDurableIdentityAndTargets(t *testing.T) {
	cfg := config.Default()
	cfg.GitHub = config.GitHub{App: config.GitHubApp{ClientID: "client", KeychainService: "service", KeychainAccount: "account"},
		SessionOwner: "host", Installations: []config.GitHubInstallation{{Name: "personal", InstallationID: 7}},
		Scopes: []config.GitHubScope{
			{Name: "one", Kind: config.ScopeRepository, ConfigURL: "https://github.com/o/r1", Installation: "personal", Targets: []string{"o/r1"}, ScaleSets: []config.ScaleSet{{Profile: "small", Name: "one-small", ID: 11, MaxCapacity: 1, Labels: []string{"self-hosted", "linux-small"}}}},
			{Name: "two", Kind: config.ScopeRepository, ConfigURL: "https://github.com/o/r2", Installation: "personal", Targets: []string{"o/r2"}, ScaleSets: []config.ScaleSet{{Profile: "small", Name: "two-small", ID: 11, MaxCapacity: 1, Labels: []string{"self-hosted", "linux-small"}}}},
		}}
	bindings, err := BuildBindings(cfg, BuildSchedulerConfig(cfg))
	if err != nil || len(bindings) != 2 {
		t.Fatalf("bindings = %#v, %v", bindings, err)
	}
	// The canonical resource label is attached beside the configured legacy
	// label, so a binding matches jobs that ask for either name (ADR 0032).
	if bindings[0].ScaleSetID != 11 || bindings[0].StoreKey <= 0 || bindings[0].StoreKey == bindings[1].StoreKey || bindings[0].Scope != "one" ||
		len(bindings[0].ScaleSetLabels) != 4 || bindings[0].ScaleSetLabels[1] != "linux-small" ||
		bindings[0].ScaleSetLabels[2] != "trf-linux-arm64-1x2" || bindings[0].ScaleSetLabels[3] != "one-small" {
		t.Fatalf("scoped identities = %#v", bindings)
	}
	if !bindings[0].accepts("o/r1") || !bindings[0].accepts("O/R1") || bindings[0].accepts("o/r2") {
		t.Fatalf("target filter = %#v", bindings[0])
	}
}

// TestBindingRoutesCanonicalAndLegacyLabelsToTheSameScaleSet proves the
// consumer-visible half of ADR 0032: a workflow may ask for a shape by its
// resource label or by the role name it used before, and both reach the one
// scale set, while a shape that scale set does not serve still does not match.
func TestBindingRoutesCanonicalAndLegacyLabelsToTheSameScaleSet(t *testing.T) {
	cfg := config.Default()
	cfg.GitHub.ScaleSets = []config.ScaleSet{{Profile: "small", Name: "repo-small", ID: 1, MaxCapacity: 4,
		Labels: []string{"self-hosted", "linux-small"}}}
	bindings, err := BuildBindings(cfg, BuildSchedulerConfig(cfg))
	if err != nil || len(bindings) != 1 {
		t.Fatalf("bindings = %#v, %v", bindings, err)
	}
	for _, requested := range [][]string{{"self-hosted", "linux-small"}, {"self-hosted", "trf-linux-arm64-1x2"}} {
		if !bindings[0].matchesRESTLabels(requested) {
			t.Fatalf("binding refused %v", requested)
		}
	}
	if bindings[0].matchesRESTLabels([]string{"self-hosted", "trf-linux-arm64-4x8"}) {
		t.Fatal("binding accepted a job asking for a shape it does not serve")
	}
}

func TestEffectiveScaleSetLabelsDoesNotDuplicateConfiguredName(t *testing.T) {
	got := effectiveScaleSetLabels(config.ScaleSet{Name: "Fleet-Small", Labels: []string{"self-hosted", "fleet-small"}}, config.LabelSet{})
	if len(got) != 2 || got[1] != "fleet-small" {
		t.Fatalf("effective labels = %#v", got)
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

// TestBuildBindingsCarriesTheSharedLabelDeclaration proves the one fact the REST
// inventory lane cannot observe reaches it. A node is not aware of another node,
// so "a peer advertises these labels in this scope" is configuration, and a
// binding that lost it would silently claim the peer's queue (issue #153).
func TestBuildBindingsCarriesTheSharedLabelDeclaration(t *testing.T) {
	scoped := config.Default()
	scoped.GitHub = config.GitHub{App: config.GitHubApp{ClientID: "client", KeychainService: "service", KeychainAccount: "account"},
		SessionOwner: "host", CanonicalJobInventory: true,
		Installations: []config.GitHubInstallation{{Name: "personal", InstallationID: 7}},
		Scopes: []config.GitHubScope{{Name: "sudoku", Kind: config.ScopeRepository,
			ConfigURL: "https://github.com/o/r", Installation: "personal", Targets: []string{"o/r"},
			ScaleSets: []config.ScaleSet{
				{Profile: "maestro", Name: "shared", ID: 21, MaxCapacity: 2, SharedLabels: true,
					Labels: []string{"self-hosted", "macos-maestro"}},
				{Profile: "builder", Name: "sole", ID: 22, MaxCapacity: 1,
					Labels: []string{"self-hosted", "macos-builder"}},
			}}}}
	bindings, err := BuildBindings(scoped, BuildSchedulerConfig(scoped))
	if err != nil || len(bindings) != 2 || !bindings[0].SharedLabels || bindings[1].SharedLabels {
		t.Fatalf("scoped shared-label declaration = %#v, %v", bindings, err)
	}

	legacy := config.Default()
	legacy.GitHub.ScaleSets = []config.ScaleSet{{Profile: "small", Name: "repo-small", ID: 1, MaxCapacity: 4,
		SharedLabels: true, Labels: []string{"self-hosted", "linux-small"}}}
	bindings, err = BuildBindings(legacy, BuildSchedulerConfig(legacy))
	if err != nil || len(bindings) != 1 || !bindings[0].SharedLabels {
		t.Fatalf("legacy shared-label declaration = %#v, %v", bindings, err)
	}
}
