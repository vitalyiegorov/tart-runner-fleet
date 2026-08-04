package config

import (
	"bytes"
	"strings"
	"testing"
)

// budgetConfig is a Default() configuration whose scale sets expose exactly the
// named profiles. Exposure is what the budget check reads, so every table below
// states it explicitly rather than inheriting it.
func budgetConfig(exposed ...string) Config {
	cfg := Default()
	cfg.GitHub.ScaleSets = nil
	for _, profile := range exposed {
		cfg.GitHub.ScaleSets = append(cfg.GitHub.ScaleSets, ScaleSet{Profile: profile, ID: 1, MaxCapacity: 8})
	}
	return cfg
}

func TestHostBudgetDecodesEncodesAndRoundTrips(t *testing.T) {
	raw := `{
      "baseVm":"linux-runner-base", "vmPrefix":"gha-linux",
      "pollSeconds":20, "maxLinuxWhenMacosIdle":4,
      "maxLinuxCpu":8, "maxLinuxMemoryMb":16384,
      "linuxReservationAgeSeconds":300, "minFreeDiskGb":60,
      "hostBudget":{"cpu":4,"memoryMb":10240},
      "linuxProfiles":[{"id":"small","label":"linux-small","cpu":1,"memoryMb":2048}],
      "macosBurst":{"enabled":false},
      "targets":[{"type":"repo","slug":"owner/repo","maxActive":3}]
    }`

	cfg, err := Decode(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if cfg.HostBudget != (Resources{CPU: 4, MemoryMiB: 10240}) {
		t.Fatalf("decoded host budget = %+v, want 4 CPU / 10240 MiB", cfg.HostBudget)
	}
	var encoded bytes.Buffer
	if err := Encode(&encoded, cfg); err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	if !strings.Contains(encoded.String(), `"hostBudget"`) {
		t.Fatalf("Encode() dropped a configured host budget:\n%s", encoded.String())
	}
	round, err := Decode(strings.NewReader(encoded.String()))
	if err != nil {
		t.Fatalf("Decode(Encode()) error = %v", err)
	}
	if round.HostBudget != cfg.HostBudget {
		t.Fatalf("round-tripped host budget = %+v, want %+v", round.HostBudget, cfg.HostBudget)
	}
}

// TestOmittedHostBudgetStaysOffTheWire keeps zero migration honest: a file that
// never mentioned the setting must not grow a field an older strict release
// (DisallowUnknownFields) would refuse to decode.
func TestOmittedHostBudgetStaysOffTheWire(t *testing.T) {
	cfg := Default()
	if cfg.HostBudget != (Resources{}) {
		t.Fatalf("Default() host budget = %+v, want the physical envelope (unset)", cfg.HostBudget)
	}
	var encoded bytes.Buffer
	if err := Encode(&encoded, cfg); err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	if strings.Contains(encoded.String(), "hostBudget") {
		t.Fatalf("Encode() introduced hostBudget into a configuration that has none:\n%s", encoded.String())
	}
}

func TestValidateRejectsNonsensicalHostBudget(t *testing.T) {
	tests := map[string]struct {
		budget Resources
		want   string
	}{
		"cpu only":        {budget: Resources{CPU: 4}, want: "host budget"},
		"memory only":     {budget: Resources{MemoryMiB: 10240}, want: "host budget"},
		"negative cpu":    {budget: Resources{CPU: -1, MemoryMiB: 10240}, want: "host budget"},
		"negative memory": {budget: Resources{CPU: 4, MemoryMiB: -1}, want: "host budget"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := budgetConfig()
			cfg.HostBudget = test.budget
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want one mentioning %q", err, test.want)
			}
		})
	}
}

// TestValidateRejectsAnExposedProfileThatCanNeverFitTheBudget is the ADR 0032
// interaction: a shape GitHub can route to but the node can never admit is
// permanent starvation, not backpressure. A profile the node does not expose is
// not checked, which is what lets a budgeted node keep the mandatory macOS
// builder in its file while serving maestro only.
func TestValidateRejectsAnExposedProfileThatCanNeverFitTheBudget(t *testing.T) {
	tests := map[string]struct {
		exposed []string
		budget  Resources
		wantErr string
	}{
		"exposed macOS builder exceeds budget cpu": {
			exposed: []string{"builder"}, budget: Resources{CPU: 4, MemoryMiB: 10240}, wantErr: "builder"},
		"exposed linux profile exceeds budget memory": {
			exposed: []string{"large"}, budget: Resources{CPU: 8, MemoryMiB: 4096}, wantErr: "large"},
		"exposed maestro fits the budget": {
			exposed: []string{"maestro"}, budget: Resources{CPU: 4, MemoryMiB: 10240}},
		"unexposed builder is not checked": {
			exposed: []string{"maestro", "small"}, budget: Resources{CPU: 4, MemoryMiB: 10240}},
		"no budget checks nothing": {
			exposed: []string{"builder"}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := budgetConfig(test.exposed...)
			cfg.HostBudget = test.budget
			err := cfg.Validate()
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Validate() error = %v, want one naming profile %q", err, test.wantErr)
			}
		})
	}
}

// TestScopedScaleSetsExposeProfilesToTheBudgetCheck proves exposure is read from
// the multi-scope model too, not only the legacy flat list.
func TestScopedScaleSetsExposeProfilesToTheBudgetCheck(t *testing.T) {
	cfg := budgetConfig()
	cfg.GitHub.Scopes = []GitHubScope{{Name: "org", ScaleSets: []ScaleSet{{Profile: "builder", ID: 7, MaxCapacity: 8}}}}
	cfg.HostBudget = Resources{CPU: 4, MemoryMiB: 10240}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "builder") {
		t.Fatalf("Validate() error = %v, want one naming the scoped profile that cannot fit", err)
	}
}

// TestHostBudgetBoundsTruthfulScaleSetCapacity keeps ADR 0015's promise on a
// budgeted node: advertising two maestro slots on a 4-vCPU budget that can hold
// one is advertising capacity the node can never serve.
func TestHostBudgetBoundsTruthfulScaleSetCapacity(t *testing.T) {
	cfg := budgetConfig()
	cfg.HostBudget = Resources{CPU: 4, MemoryMiB: 10240}
	capacities := cfg.authorityProfileCapacities()
	if capacities["maestro"] != 1 {
		t.Fatalf("maestro authority capacity = %d, want 1 under a 4 CPU / 10240 MiB budget", capacities["maestro"])
	}
	if capacities["small"] != 4 {
		t.Fatalf("small authority capacity = %d, want the 4-slot ceiling", capacities["small"])
	}
	unbudgeted := budgetConfig().authorityProfileCapacities()
	if unbudgeted["maestro"] != 2 {
		t.Fatalf("unbudgeted maestro authority capacity = %d, want its MaxActive 2", unbudgeted["maestro"])
	}
}
