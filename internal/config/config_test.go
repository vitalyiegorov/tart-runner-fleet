package config

import (
	"strings"
	"testing"
	"time"
)

func TestDecodeLegacyConfiguration(t *testing.T) {
	raw := `{
      "baseVm":"linux-runner-base", "vmPrefix":"gha-linux-burst",
      "pollSeconds":20, "maxLinuxWhenMacosIdle":4,
      "maxLinuxCpu":8, "maxLinuxMemoryMb":16384,
      "linuxReservationAgeSeconds":300, "minFreeDiskGb":60,
      "linuxProfiles":[
        {"id":"small","label":"linux-small","cpu":1,"memoryMb":2048},
        {"id":"medium","label":"linux-medium","cpu":2,"memoryMb":4096}
      ],
      "macosBurst":{"enabled":true,"baseVm":"macos-base","vmPrefix":"gha-macos",
        "builder":{"label":"macos-builder","cpu":8,"memoryMb":12288,"maxActive":1},
        "maestro":{"label":"macos-maestro","cpu":4,"memoryMb":7168,"maxActive":2}},
      "targets":[{"type":"repo","slug":"owner/repo","maxActive":3}]
    }`

	cfg, err := Decode(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if cfg.PollInterval != 20*time.Second || cfg.ReservationAge != 5*time.Minute {
		t.Fatalf("durations = %v, %v", cfg.PollInterval, cfg.ReservationAge)
	}
	if cfg.Linux.Capacity.CPU != 8 || cfg.Linux.Capacity.MemoryMiB != 16384 || cfg.Linux.MaxInstances != 4 {
		t.Fatalf("linux capacity = %+v, max=%d", cfg.Linux.Capacity, cfg.Linux.MaxInstances)
	}
	if cfg.MacOS.Maestro.MaxActive != 2 || cfg.Targets[0].Slug != "owner/repo" {
		t.Fatalf("mac/target decode = %+v %+v", cfg.MacOS, cfg.Targets)
	}
}

func TestValidateRejectsUnsafeOrAmbiguousConfiguration(t *testing.T) {
	valid := Default()
	tests := map[string]func(*Config){
		"missing base":           func(c *Config) { c.Linux.BaseVM = "" },
		"duplicate profile":      func(c *Config) { c.Linux.Profiles = append(c.Linux.Profiles, c.Linux.Profiles[0]) },
		"profile exceeds host":   func(c *Config) { c.Linux.Profiles[0].Resources.CPU = c.Linux.Capacity.CPU + 1 },
		"invalid repository":     func(c *Config) { c.Targets[0].Slug = "bad" },
		"duplicate repository":   func(c *Config) { c.Targets = append(c.Targets, c.Targets[0]) },
		"zero timeout":           func(c *Config) { c.Timeouts.GitHub = 0 },
		"unsafe disk reserve":    func(c *Config) { c.Guards.MinFreeDiskGiB = 0 },
		"too many linux runners": func(c *Config) { c.Linux.MaxInstances = 5 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := valid.Clone()
			mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("Validate() unexpectedly succeeded")
			}
		})
	}
}

func TestDefaultIsValidAndCloneIsIndependent(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Default().Validate() = %v", err)
	}
	clone := cfg.Clone()
	clone.Targets[0].Slug = "other/repo"
	clone.Linux.Profiles[0].ID = "changed"
	if cfg.Targets[0].Slug == clone.Targets[0].Slug || cfg.Linux.Profiles[0].ID == clone.Linux.Profiles[0].ID {
		t.Fatal("Clone() shares mutable slices")
	}
}

func TestDecodeRejectsMalformedUnknownTrailingAndInvalid(t *testing.T) {
	tests := map[string]string{
		"malformed":       `{`,
		"unknown field":   `{"surprise":true}`,
		"trailing syntax": `{} {`,
		"multiple values": `{} {}`,
		"invalid config":  `{}`,
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(strings.NewReader(input)); err == nil {
				t.Fatal("Decode() unexpectedly succeeded")
			}
		})
	}
}

func TestValidateEveryInvariant(t *testing.T) {
	valid := Default()
	tests := map[string]func(*Config){
		"zero poll":              func(c *Config) { c.PollInterval = 0 },
		"zero reservation":       func(c *Config) { c.ReservationAge = 0 },
		"zero tart timeout":      func(c *Config) { c.Timeouts.Tart = 0 },
		"zero boot timeout":      func(c *Config) { c.Timeouts.Boot = 0 },
		"missing prefix":         func(c *Config) { c.Linux.VMPrefix = "" },
		"zero instance count":    func(c *Config) { c.Linux.MaxInstances = 0 },
		"zero capacity cpu":      func(c *Config) { c.Linux.Capacity.CPU = 0 },
		"zero capacity memory":   func(c *Config) { c.Linux.Capacity.MemoryMiB = 0 },
		"profile no id":          func(c *Config) { c.Linux.Profiles[0].ID = "" },
		"profile no label":       func(c *Config) { c.Linux.Profiles[0].Label = "" },
		"profile zero cpu":       func(c *Config) { c.Linux.Profiles[0].Resources.CPU = 0; c.Linux.Profiles[0].CPU = 0 },
		"profile zero memory":    func(c *Config) { c.Linux.Profiles[0].Resources.MemoryMiB = 0; c.Linux.Profiles[0].MemoryMiB = 0 },
		"profile memory too big": func(c *Config) { c.Linux.Profiles[0].Resources.MemoryMiB = c.Linux.Capacity.MemoryMiB + 1 },
		"target wrong type":      func(c *Config) { c.Targets[0].Type = "org" },
		"target zero max":        func(c *Config) { c.Targets[0].MaxActive = 0 },
		"mac no base":            func(c *Config) { c.MacOS.BaseVM = "" },
		"mac no prefix":          func(c *Config) { c.MacOS.VMPrefix = "" },
		"mac no label":           func(c *Config) { c.MacOS.Builder.Label = "" },
		"mac zero cpu":           func(c *Config) { c.MacOS.Builder.Resources.CPU = 0; c.MacOS.Builder.CPU = 0 },
		"mac zero memory":        func(c *Config) { c.MacOS.Builder.Resources.MemoryMiB = 0; c.MacOS.Builder.MemoryMiB = 0 },
		"mac zero active":        func(c *Config) { c.MacOS.Builder.MaxActive = 0 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := valid.Clone()
			mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("Validate() unexpectedly succeeded")
			}
		})
	}
}

func TestMacOSCanBeDisabledWithoutMacConfiguration(t *testing.T) {
	cfg := Default()
	cfg.MacOS = MacOS{}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
}

func TestValidateAuthority(t *testing.T) {
	valid := Default()
	valid.GitHub = GitHub{ConfigURL: "https://github.com/owner", Owner: "owner", ClientID: "client", InstallationID: 42,
		KeychainService: "fleet", KeychainAccount: "app", ScaleSets: []ScaleSet{
			{Profile: "small", ID: 1, MaxCapacity: 4}, {Profile: "medium", ID: 2, MaxCapacity: 4}, {Profile: "large", ID: 3, MaxCapacity: 2},
			{Profile: "builder", ID: 4, MaxCapacity: 1}, {Profile: "maestro", ID: 5, MaxCapacity: 2},
		}}
	if err := valid.ValidateAuthority(); err != nil {
		t.Fatalf("ValidateAuthority() = %v", err)
	}
	tests := map[string]func(*Config){
		"base invalid":      func(c *Config) { c.PollInterval = 0 },
		"missing app":       func(c *Config) { c.GitHub.ClientID = "" },
		"unknown profile":   func(c *Config) { c.GitHub.ScaleSets[0].Profile = "unknown" },
		"bad id":            func(c *Config) { c.GitHub.ScaleSets[0].ID = 0 },
		"bad capacity":      func(c *Config) { c.GitHub.ScaleSets[0].MaxCapacity = -1 },
		"duplicate profile": func(c *Config) { c.GitHub.ScaleSets[1].Profile = "small" },
		"missing scale set": func(c *Config) { c.GitHub.ScaleSets = c.GitHub.ScaleSets[:4] },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := valid.Clone()
			mutate(&cfg)
			if err := cfg.ValidateAuthority(); err == nil {
				t.Fatal("ValidateAuthority() unexpectedly succeeded")
			}
		})
	}
	clone := valid.Clone()
	clone.GitHub.ScaleSets[0].ID = 99
	if valid.GitHub.ScaleSets[0].ID == 99 {
		t.Fatal("Clone aliases scale sets")
	}
}
