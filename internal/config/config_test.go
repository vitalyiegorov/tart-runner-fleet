package config

import (
	"bytes"
	"errors"
	"math"
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
      "linuxNestedVirtualization":true,
      "linuxProfiles":[
        {"id":"small","label":"linux-small","cpu":1,"memoryMb":2048},
        {"id":"medium","label":"linux-medium","cpu":2,"memoryMb":4096,"diskGb":40}
      ],
      "macosBurst":{"enabled":true,"baseVm":"macos-base","vmPrefix":"gha-macos",
        "builder":{"label":"macos-builder","cpu":8,"memoryMb":12288,"maxActive":1},
        "maestro":{"label":"macos-maestro","cpu":4,"memoryMb":7168,"maxActive":2},
        "rootDiskOptions":"sync=none","sharedDirectoryPath":"/private/tmp/ci-shared"},
      "targets":[{"type":"repo","slug":"owner/repo","maxActive":3}]
    }`

	cfg, err := Decode(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if cfg.PollInterval != 20*time.Second || cfg.ReservationAge != 5*time.Minute {
		t.Fatalf("durations = %v, %v", cfg.PollInterval, cfg.ReservationAge)
	}
	if cfg.Timeouts.Assigned != 15*time.Minute {
		t.Fatalf("omitted assignedTimeoutSeconds default = %v, want 15m", cfg.Timeouts.Assigned)
	}
	if cfg.Linux.Capacity.CPU != 8 || cfg.Linux.Capacity.MemoryMiB != 16384 || cfg.Linux.MaxInstances != 4 {
		t.Fatalf("linux capacity = %+v, max=%d", cfg.Linux.Capacity, cfg.Linux.MaxInstances)
	}
	if cfg.Linux.Profiles[0].DiskGiB != 0 || cfg.Linux.Profiles[1].DiskGiB != 40 {
		t.Fatalf("profile disk floors = %d, %d", cfg.Linux.Profiles[0].DiskGiB, cfg.Linux.Profiles[1].DiskGiB)
	}
	if cfg.MacOS.Maestro.MaxActive != 2 || cfg.Targets[0].Slug != "owner/repo" {
		t.Fatalf("mac/target decode = %+v %+v", cfg.MacOS, cfg.Targets)
	}
	if cfg.MacOS.RootDiskOptions != "sync=none" || cfg.MacOS.SharedDirectoryPath != "/private/tmp/ci-shared" || !cfg.Linux.NestedVirtualization {
		t.Fatalf("mac performance decode = %+v", cfg.MacOS)
	}
	if cfg.MacOS.AdmissionPolicy != MacOSAdmissionShared {
		t.Fatalf("omitted macOS admission policy = %q, want %q", cfg.MacOS.AdmissionPolicy, MacOSAdmissionShared)
	}
	var encoded bytes.Buffer
	if err := Encode(&encoded, cfg); err != nil {
		t.Fatalf("Encode(legacy) = %v", err)
	}
	if strings.Contains(encoded.String(), `"admissionPolicy"`) {
		t.Fatalf("legacy Decode->Encode introduced a field older strict releases reject:\n%s", encoded.String())
	}
	wantGuards := Guards{MinFreeDiskGiB: 60, MinAvailableMemoryMiB: 1024, MaxSwapUsedMiB: 2048, MaxLoadAverage: 9, MinCPUIdlePercent: 5}
	if cfg.Guards != wantGuards {
		t.Fatalf("legacy guard defaults = %+v, want %+v", cfg.Guards, wantGuards)
	}
}

func TestDecodeAndEncodeOptInFlags(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		fragment string
		decoded  func(Config) bool
		clear    func(*Config)
	}{
		{
			name: "mixed platform admission",
			raw: `{
      "baseVm":"linux-runner-base", "vmPrefix":"gha-linux",
      "pollSeconds":20, "maxLinuxWhenMacosIdle":4,
      "maxLinuxCpu":8, "maxLinuxMemoryMb":16384,
      "linuxReservationAgeSeconds":300, "minFreeDiskGb":60,
      "linuxProfiles":[{"id":"small","label":"linux-small","cpu":1,"memoryMb":2048,"diskGb":40}],
      "macosBurst":{"enabled":false, "mixedPlatformAdmission":true},
      "targets":[{"type":"repo","slug":"owner/repo","maxActive":3}]
    }`,
			fragment: `"mixedPlatformAdmission": true`,
			decoded:  func(c Config) bool { return c.MacOS.MixedPlatformAdmission },
			clear:    func(c *Config) { c.MacOS.MixedPlatformAdmission = false },
		},
		{
			name: "pressure memory accounting",
			raw: `{
      "baseVm":"linux-runner-base", "vmPrefix":"gha-linux",
      "pollSeconds":20, "maxLinuxWhenMacosIdle":4,
      "maxLinuxCpu":8, "maxLinuxMemoryMb":16384,
      "linuxReservationAgeSeconds":300, "minFreeDiskGb":60,
      "pressureMemoryAccounting":true,
      "linuxProfiles":[{"id":"small","label":"linux-small","cpu":1,"memoryMb":2048,"diskGb":40}],
      "macosBurst":{"enabled":false},
      "targets":[{"type":"repo","slug":"owner/repo","maxActive":3}]
    }`,
			fragment: `"pressureMemoryAccounting": true`,
			decoded:  func(c Config) bool { return c.Guards.PressureMemoryAccounting },
			clear:    func(c *Config) { c.Guards.PressureMemoryAccounting = false },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := Decode(strings.NewReader(test.raw))
			if err != nil {
				t.Fatalf("Decode() error = %v", err)
			}
			if !test.decoded(cfg) {
				t.Fatalf("flag not decoded")
			}
			var encoded bytes.Buffer
			if err := Encode(&encoded, cfg); err != nil {
				t.Fatalf("Encode() = %v", err)
			}
			if !strings.Contains(encoded.String(), test.fragment) {
				t.Fatalf("Encode omitted %s:\n%s", test.fragment, encoded.String())
			}
			roundTripped, err := Decode(&encoded)
			if err != nil || !test.decoded(roundTripped) {
				t.Fatalf("round-trip = %v, %v", test.decoded(roundTripped), err)
			}
			// Default off is omitted from the encoded form so older strict
			// releases, which reject unknown fields, accept legacy configs.
			legacy := cfg
			test.clear(&legacy)
			var legacyEncoded bytes.Buffer
			if err := Encode(&legacyEncoded, legacy); err != nil {
				t.Fatalf("Encode(legacy) = %v", err)
			}
			if strings.Contains(legacyEncoded.String(), strings.Split(test.fragment, `"`)[1]) {
				t.Fatalf("default-off flag leaked into encoded form:\n%s", legacyEncoded.String())
			}
		})
	}
}

func TestDecodeAndEncodePreserveExplicitAssignedTimeout(t *testing.T) {
	raw := `{
      "baseVm":"linux-runner-base", "vmPrefix":"gha-linux",
      "pollSeconds":20, "maxLinuxWhenMacosIdle":4,
      "maxLinuxCpu":8, "maxLinuxMemoryMb":16384,
      "linuxReservationAgeSeconds":300, "minFreeDiskGb":60,
      "assignedTimeoutSeconds":600,
      "linuxProfiles":[{"id":"small","label":"linux-small","cpu":1,"memoryMb":2048,"diskGb":40}],
      "macosBurst":{"enabled":false},
      "targets":[{"type":"repo","slug":"owner/repo","maxActive":3}]
    }`
	cfg, err := Decode(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if cfg.Timeouts.Assigned != 10*time.Minute {
		t.Fatalf("explicit assignedTimeoutSeconds = %v, want 10m", cfg.Timeouts.Assigned)
	}
	var encoded bytes.Buffer
	if err := Encode(&encoded, cfg); err != nil {
		t.Fatalf("Encode() = %v", err)
	}
	if !strings.Contains(encoded.String(), `"assignedTimeoutSeconds": 600`) {
		t.Fatalf("Encode omitted assignedTimeoutSeconds:\n%s", encoded.String())
	}
	roundTripped, err := Decode(&encoded)
	if err != nil || roundTripped.Timeouts.Assigned != 10*time.Minute {
		t.Fatalf("round-trip assigned timeout = %v, %v", roundTripped.Timeouts.Assigned, err)
	}
}

func TestValidateRejectsUnsafeOrAmbiguousConfiguration(t *testing.T) {
	valid := Default()
	tests := map[string]func(*Config){
		"missing base":            func(c *Config) { c.Linux.BaseVM = "" },
		"duplicate profile":       func(c *Config) { c.Linux.Profiles = append(c.Linux.Profiles, c.Linux.Profiles[0]) },
		"profile exceeds host":    func(c *Config) { c.Linux.Profiles[0].Resources.CPU = c.Linux.Capacity.CPU + 1 },
		"negative profile disk":   func(c *Config) { c.Linux.Profiles[0].DiskGiB = -1 },
		"invalid repository":      func(c *Config) { c.Targets[0].Slug = "bad" },
		"duplicate repository":    func(c *Config) { c.Targets = append(c.Targets, c.Targets[0]) },
		"zero timeout":            func(c *Config) { c.Timeouts.GitHub = 0 },
		"unsafe disk reserve":     func(c *Config) { c.Guards.MinFreeDiskGiB = 0 },
		"negative memory reserve": func(c *Config) { c.Guards.MinAvailableMemoryMiB = -1 },
		"negative swap ceiling":   func(c *Config) { c.Guards.MaxSwapUsedMiB = -1 },
		"negative load ceiling":   func(c *Config) { c.Guards.MaxLoadAverage = -1 },
		"invalid cpu idle floor":  func(c *Config) { c.Guards.MinCPUIdlePercent = 101 },
		"invalid load number":     func(c *Config) { c.Guards.MaxLoadAverage = math.NaN() },
		"too many linux runners":  func(c *Config) { c.Linux.MaxInstances = 5 },
		"invalid root disk mode":  func(c *Config) { c.MacOS.RootDiskOptions = "sync=unsafe" },
		"relative shared path":    func(c *Config) { c.MacOS.SharedDirectoryPath = "relative/cache" },
		"invalid mac admission":   func(c *Config) { c.MacOS.AdmissionPolicy = "invalid" },
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

func TestAuthorityRequiresExplicitLinuxDiskFloors(t *testing.T) {
	cfg := multiScopeAuthorityConfig()
	cfg.Linux.Profiles[0].DiskGiB = 0
	if err := cfg.ValidateAuthority(); err == nil || !strings.Contains(err.Error(), "disk floor") {
		t.Fatalf("ValidateAuthority() error = %v", err)
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
		"zero poll":                 func(c *Config) { c.PollInterval = 0 },
		"zero reservation":          func(c *Config) { c.ReservationAge = 0 },
		"zero tart timeout":         func(c *Config) { c.Timeouts.Tart = 0 },
		"zero boot timeout":         func(c *Config) { c.Timeouts.Boot = 0 },
		"assigned timeout too low":  func(c *Config) { c.Timeouts.Assigned = 59 * time.Second },
		"assigned timeout too high": func(c *Config) { c.Timeouts.Assigned = time.Hour + time.Second },
		"missing prefix":            func(c *Config) { c.Linux.VMPrefix = "" },
		"zero instance count":       func(c *Config) { c.Linux.MaxInstances = 0 },
		"zero capacity cpu":         func(c *Config) { c.Linux.Capacity.CPU = 0 },
		"zero capacity memory":      func(c *Config) { c.Linux.Capacity.MemoryMiB = 0 },
		"profile no id":             func(c *Config) { c.Linux.Profiles[0].ID = "" },
		"profile no label":          func(c *Config) { c.Linux.Profiles[0].Label = "" },
		"profile zero cpu":          func(c *Config) { c.Linux.Profiles[0].Resources.CPU = 0; c.Linux.Profiles[0].CPU = 0 },
		"profile zero memory":       func(c *Config) { c.Linux.Profiles[0].Resources.MemoryMiB = 0; c.Linux.Profiles[0].MemoryMiB = 0 },
		"profile memory too big":    func(c *Config) { c.Linux.Profiles[0].Resources.MemoryMiB = c.Linux.Capacity.MemoryMiB + 1 },
		"target wrong type":         func(c *Config) { c.Targets[0].Type = "org" },
		"target zero max":           func(c *Config) { c.Targets[0].MaxActive = 0 },
		"mac nested virtualization (Linux-only feature)": func(c *Config) { c.MacOS.NestedVirtualization = true },
		"mac no base":     func(c *Config) { c.MacOS.BaseVM = "" },
		"mac no prefix":   func(c *Config) { c.MacOS.VMPrefix = "" },
		"mac no label":    func(c *Config) { c.MacOS.Builder.Label = "" },
		"mac zero cpu":    func(c *Config) { c.MacOS.Builder.Resources.CPU = 0; c.MacOS.Builder.CPU = 0 },
		"mac zero memory": func(c *Config) { c.MacOS.Builder.Resources.MemoryMiB = 0; c.MacOS.Builder.MemoryMiB = 0 },
		"mac zero active": func(c *Config) { c.MacOS.Builder.MaxActive = 0 },
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
		CanonicalJobInventory: true,
		KeychainService:       "fleet", KeychainAccount: "app", ScaleSets: []ScaleSet{
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
	legacy := valid.Clone()
	legacy.GitHub.CanonicalJobInventory = false
	for index := range legacy.GitHub.ScaleSets {
		legacy.GitHub.ScaleSets[index].MaxCapacity++
	}
	if err := legacy.ValidateAuthority(); err != nil {
		t.Fatalf("legacy lookahead rejected: %v", err)
	}
	legacy.GitHub.ScaleSets[0].MaxCapacity--
	if err := legacy.ValidateAuthority(); err == nil || !strings.Contains(err.Error(), "queue lookahead") {
		t.Fatalf("legacy authority accepted capacity without lookahead: %v", err)
	}
}

func TestValidateAuthorityAcceptsMultipleInstallationsAndRegistrationScopes(t *testing.T) {
	valid := multiScopeAuthorityConfig()

	if err := valid.ValidateAuthority(); err != nil {
		t.Fatalf("ValidateAuthority() = %v", err)
	}

	clone := valid.Clone()
	clone.GitHub.Installations[0].Name = "changed"
	clone.GitHub.Scopes[0].Targets[0] = "other/repo"
	clone.GitHub.Scopes[0].ScaleSets[0].Labels[0] = "changed"
	if valid.GitHub.Installations[0].Name == "changed" ||
		valid.GitHub.Scopes[0].Targets[0] == "other/repo" ||
		valid.GitHub.Scopes[0].ScaleSets[0].Labels[0] == "changed" {
		t.Fatal("Clone() aliases multi-scope GitHub configuration")
	}
}

func TestValidateAuthorityAcceptsPrivateKeyFileAndGivesItSafePrecedence(t *testing.T) {
	cfg := multiScopeAuthorityConfig()
	cfg.GitHub.App.PrivateKeyFile = "/Users/runner/.config/tart-runner-fleet/app.pem"
	cfg.GitHub.App.KeychainService = ""
	cfg.GitHub.App.KeychainAccount = ""
	if err := cfg.ValidateAuthority(); err != nil {
		t.Fatalf("ValidateAuthority(file only) = %v", err)
	}

	// A configured file is authoritative, so stale Keychain metadata cannot
	// force an unattended process back into an interactive Keychain prompt.
	cfg.GitHub.App.KeychainService = "stale-service"
	if err := cfg.ValidateAuthority(); err != nil {
		t.Fatalf("ValidateAuthority(file precedence) = %v", err)
	}

	cfg.GitHub.App.PrivateKeyFile = ""
	if err := cfg.ValidateAuthority(); err == nil {
		t.Fatal("ValidateAuthority accepted an incomplete Keychain reference without a file")
	}

	cfg = multiScopeAuthorityConfig()
	cfg.GitHub.App.PrivateKeyFile = "/Users/runner/.config/tart-runner-fleet/app.pem"
	var encoded bytes.Buffer
	if err := Encode(&encoded, cfg); err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(strings.NewReader(encoded.String()))
	if err != nil || decoded.GitHub.App.PrivateKeyFile != cfg.GitHub.App.PrivateKeyFile {
		t.Fatalf("privateKeyFile round trip = %q, %v", decoded.GitHub.App.PrivateKeyFile, err)
	}
}

func TestDecodeMultiScopeGitHubConfiguration(t *testing.T) {
	raw := `{
      "baseVm":"linux-runner-base", "vmPrefix":"gha-linux-burst",
      "pollSeconds":20, "maxLinuxWhenMacosIdle":4,
      "maxLinuxCpu":8, "maxLinuxMemoryMb":16384,
      "linuxReservationAgeSeconds":300, "minFreeDiskGb":60,
      "linuxProfiles":[{"id":"small","label":"linux-small","cpu":1,"memoryMb":2048,"diskGb":50}],
      "macosBurst":{"enabled":false},
      "github":{
        "sessionOwner":"fleet-macmini",
		"canonicalJobInventory":true,
        "app":{"clientId":"Iv1.test","keychainService":"fleet","keychainAccount":"app"},
        "installations":[{"name":"personal","installationId":146307296}],
        "scopes":[{
          "name":"knee-doctor","kind":"repository",
          "configUrl":"https://github.com/vitalyiegorov/knee-doctor",
          "installation":"personal","targets":["vitalyiegorov/knee-doctor"],
          "scaleSets":[{"profile":"small","name":"fleet-knee-linux-small","maxCapacity":3,
            "labels":["self-hosted","linux-tiered","linux-small"]}]
        }]
      },
      "targets":[{"type":"repo","slug":"vitalyiegorov/knee-doctor","maxActive":3}]
    }`

	cfg, err := Decode(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if cfg.GitHub.App.ClientID != "Iv1.test" || cfg.GitHub.Installations[0].InstallationID != 146307296 ||
		cfg.GitHub.Scopes[0].ScaleSets[0].Name != "fleet-knee-linux-small" {
		t.Fatalf("multi-scope GitHub decode = %+v", cfg.GitHub)
	}
	if err := cfg.ValidateAuthority(); err != nil {
		t.Fatalf("ValidateAuthority() = %v", err)
	}
}

func TestEncodeDeterministicallyRoundTripsMultiScopeConfiguration(t *testing.T) {
	cfg := multiScopeAuthorityConfig()
	cfg.MacOS.AdmissionPolicy = MacOSAdmissionExclusive
	cfg.MacOS.RootDiskOptions = "sync=none"
	cfg.MacOS.SharedDirectoryPath = "/private/tmp/ci-shared"
	cfg.GitHub.Scopes[0].ScaleSets[0].ID = 101
	cfg.GitHub.Scopes[1].ScaleSets[4].ID = 205
	var first, second bytes.Buffer
	if err := Encode(&first, cfg); err != nil {
		t.Fatalf("Encode(first) = %v", err)
	}
	if err := Encode(&second, cfg); err != nil {
		t.Fatalf("Encode(second) = %v", err)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatal("Encode() is not deterministic")
	}
	roundTrip, err := Decode(bytes.NewReader(first.Bytes()))
	if err != nil {
		t.Fatalf("Decode(Encode()) = %v", err)
	}
	if err := roundTrip.ValidateAuthority(); err != nil {
		t.Fatalf("round-trip ValidateAuthority() = %v", err)
	}
	if roundTrip.MacOS.RootDiskOptions != cfg.MacOS.RootDiskOptions || roundTrip.MacOS.SharedDirectoryPath != cfg.MacOS.SharedDirectoryPath {
		t.Fatalf("round trip lost mac performance options: %+v", roundTrip.MacOS)
	}
	if roundTrip.MacOS.AdmissionPolicy != MacOSAdmissionExclusive || !strings.Contains(first.String(), `"admissionPolicy": "macos-exclusive"`) {
		t.Fatalf("round trip lost mac admission policy: %+v\n%s", roundTrip.MacOS, first.String())
	}
	if roundTrip.GitHub.Scopes[0].ScaleSets[0].ID != 101 || roundTrip.GitHub.Scopes[1].ScaleSets[4].ID != 205 ||
		roundTrip.Linux.Profiles[0].Resources != cfg.Linux.Profiles[0].Resources || roundTrip.Timeouts != cfg.Timeouts ||
		roundTrip.Guards != cfg.Guards {
		t.Fatalf("round trip lost data: %+v", roundTrip)
	}
	if strings.Contains(first.String(), "privateKey") || strings.Contains(first.String(), "encodedJITConfig") {
		t.Fatal("Encode() emitted credential material")
	}
}

func TestEncodeRejectsInvalidConfigurationAndWriterFailure(t *testing.T) {
	invalid := Default()
	invalid.PollInterval = 0
	if err := Encode(&bytes.Buffer{}, invalid); err == nil {
		t.Fatal("Encode(invalid) unexpectedly succeeded")
	}
	if err := Encode(errorWriter{}, Default()); err == nil {
		t.Fatal("Encode(errorWriter) unexpectedly succeeded")
	}
	if err := Encode(nil, Default()); err == nil {
		t.Fatal("Encode(nil) unexpectedly succeeded")
	}
}

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestEncodeRejectsSubsecondDurations(t *testing.T) {
	tests := map[string]func(*Config){
		"poll":        func(c *Config) { c.PollInterval += time.Nanosecond },
		"reservation": func(c *Config) { c.ReservationAge += time.Nanosecond },
		"github":      func(c *Config) { c.Timeouts.GitHub += time.Nanosecond },
		"tart":        func(c *Config) { c.Timeouts.Tart += time.Nanosecond },
		"boot":        func(c *Config) { c.Timeouts.Boot += time.Nanosecond },
		"assigned":    func(c *Config) { c.Timeouts.Assigned += time.Nanosecond },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := Default()
			mutate(&cfg)
			if err := Encode(&bytes.Buffer{}, cfg); err == nil {
				t.Fatal("Encode(subsecond) unexpectedly succeeded")
			}
		})
	}
}

func TestValidateAuthorityRejectsAmbiguousOrUnsafeMultiScopeConfiguration(t *testing.T) {
	tests := map[string]func(*Config){
		"mixed legacy and multi-scope": func(c *Config) { c.GitHub.ConfigURL = "https://github.com/legacy/repo" },
		"missing session owner":        func(c *Config) { c.GitHub.SessionOwner = "" },
		"missing app client":           func(c *Config) { c.GitHub.App.ClientID = "" },
		"missing keychain service":     func(c *Config) { c.GitHub.App.KeychainService = "" },
		"missing installations":        func(c *Config) { c.GitHub.Installations = nil },
		"missing scopes":               func(c *Config) { c.GitHub.Scopes = nil },
		"bad installation id":          func(c *Config) { c.GitHub.Installations[0].InstallationID = 0 },
		"duplicate installation name": func(c *Config) {
			c.GitHub.Installations = append(c.GitHub.Installations, c.GitHub.Installations[0])
		},
		"duplicate installation id": func(c *Config) {
			other := c.GitHub.Installations[0]
			other.Name = "personal-alias"
			c.GitHub.Installations = append(c.GitHub.Installations, other)
		},
		"unknown installation": func(c *Config) { c.GitHub.Scopes[0].Installation = "missing" },
		"missing scope name":   func(c *Config) { c.GitHub.Scopes[0].Name = "" },
		"duplicate scope name": func(c *Config) { c.GitHub.Scopes[1].Name = c.GitHub.Scopes[0].Name },
		"duplicate scope url": func(c *Config) {
			c.GitHub.Scopes[1].ConfigURL = c.GitHub.Scopes[0].ConfigURL
			c.GitHub.Scopes[1].Kind = ScopeRepository
		},
		"bad scope url":           func(c *Config) { c.GitHub.Scopes[0].ConfigURL = "://" },
		"wrong repository path":   func(c *Config) { c.GitHub.Scopes[0].ConfigURL = "https://github.com/vitalyiegorov" },
		"repository runner group": func(c *Config) { c.GitHub.Scopes[0].RunnerGroup = "custom" },
		"wrong organization path": func(c *Config) { c.GitHub.Scopes[1].ConfigURL = "https://github.com/budgie-at/budgie" },
		"unsupported scope kind":  func(c *Config) { c.GitHub.Scopes[0].Kind = "enterprise" },
		"repository target count": func(c *Config) {
			c.GitHub.Scopes[0].Targets = append(c.GitHub.Scopes[0].Targets, "budgie-at/budgie")
		},
		"repo target mismatch": func(c *Config) { c.GitHub.Scopes[0].ConfigURL = "https://github.com/vitalyiegorov/other" },
		"org target mismatch":  func(c *Config) { c.GitHub.Scopes[1].ConfigURL = "https://github.com/other" },
		"duplicate target ownership": func(c *Config) {
			c.GitHub.Scopes[1].Targets = append(c.GitHub.Scopes[1].Targets, c.GitHub.Scopes[0].Targets[0])
		},
		"scope without targets":  func(c *Config) { c.GitHub.Scopes[0].Targets = nil },
		"uncovered target":       func(c *Config) { c.GitHub.Scopes = c.GitHub.Scopes[1:] },
		"unknown target":         func(c *Config) { c.GitHub.Scopes[0].Targets[0] = "vitalyiegorov/unknown" },
		"unknown profile":        func(c *Config) { c.GitHub.Scopes[0].ScaleSets[0].Profile = "unknown" },
		"missing profile":        func(c *Config) { c.GitHub.Scopes[0].ScaleSets = c.GitHub.Scopes[0].ScaleSets[:4] },
		"duplicate profile":      func(c *Config) { c.GitHub.Scopes[0].ScaleSets[1].Profile = "small" },
		"missing scale set name": func(c *Config) { c.GitHub.Scopes[0].ScaleSets[0].Name = "" },
		"duplicate scale set name": func(c *Config) {
			c.GitHub.Scopes[0].ScaleSets[1].Name = c.GitHub.Scopes[0].ScaleSets[0].Name
		},
		"negative scale set id": func(c *Config) { c.GitHub.Scopes[0].ScaleSets[0].ID = -1 },
		"negative capacity":     func(c *Config) { c.GitHub.Scopes[0].ScaleSets[0].MaxCapacity = -1 },
		"empty label":           func(c *Config) { c.GitHub.Scopes[0].ScaleSets[0].Labels[0] = "" },
		"duplicate label": func(c *Config) {
			c.GitHub.Scopes[0].ScaleSets[0].Labels = []string{"self-hosted", "linux-small", "linux-small"}
		},
		"missing self-hosted label":   func(c *Config) { c.GitHub.Scopes[0].ScaleSets[0].Labels = []string{"linux-small"} },
		"missing profile route label": func(c *Config) { c.GitHub.Scopes[0].ScaleSets[0].Labels = []string{"self-hosted", "linux-tiered"} },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := multiScopeAuthorityConfig()
			mutate(&cfg)
			if err := cfg.ValidateAuthority(); err == nil {
				t.Fatal("ValidateAuthority() unexpectedly succeeded")
			}
		})
	}
}

func TestValidateAuthorityRequiresTruthfulScaleSetCapacity(t *testing.T) {
	cfg := multiScopeAuthorityConfig()
	for index := range cfg.GitHub.Scopes[1].ScaleSets {
		set := &cfg.GitHub.Scopes[1].ScaleSets[index]
		if set.Profile == "builder" {
			set.MaxCapacity = cfg.MacOS.Builder.MaxActive + 1
		}
	}

	err := cfg.ValidateAuthority()
	if err == nil || !strings.Contains(err.Error(), "truthful capacity") {
		t.Fatalf("ValidateAuthority() error = %v, want inflated capacity rejection", err)
	}
	cfg.GitHub.Scopes[1].ScaleSets[3].MaxCapacity = 0
	if err := cfg.ValidateAuthority(); err == nil || !strings.Contains(err.Error(), "invalid scale set") {
		t.Fatalf("zero maxCapacity was accepted: %v", err)
	}
}

func TestCanonicalInventoryActivationPreservesDeployableLegacyCapacity(t *testing.T) {
	cfg := multiScopeAuthorityConfig()
	cfg.GitHub.CanonicalJobInventory = false
	for scopeIndex := range cfg.GitHub.Scopes {
		for setIndex := range cfg.GitHub.Scopes[scopeIndex].ScaleSets {
			cfg.GitHub.Scopes[scopeIndex].ScaleSets[setIndex].MaxCapacity++
		}
	}
	if err := cfg.ValidateAuthority(); err != nil {
		t.Fatalf("legacy lookahead rejected before explicit activation: %v", err)
	}
	cfg.GitHub.Scopes[0].ScaleSets[0].MaxCapacity--
	if err := cfg.ValidateAuthority(); err == nil || !strings.Contains(err.Error(), "queue lookahead") {
		t.Fatalf("dormant canonical inventory accepted capacity without lookahead: %v", err)
	}
	cfg.GitHub.Scopes[0].ScaleSets[0].MaxCapacity++
	cfg.GitHub.CanonicalJobInventory = true
	if err := cfg.ValidateAuthority(); err == nil || !strings.Contains(err.Error(), "truthful capacity") {
		t.Fatalf("canonical inventory accepted inflated capacity: %v", err)
	}
}

func TestTruthfulScaleSetCapacityUsesScopedRuntimeCapacity(t *testing.T) {
	cfg := multiScopeAuthorityConfig()
	cfg.Targets[0].MaxActive = 1
	for index := range cfg.GitHub.Scopes[0].ScaleSets {
		cfg.GitHub.Scopes[0].ScaleSets[index].MaxCapacity = 1
	}
	if err := cfg.ValidateAuthority(); err != nil {
		t.Fatalf("one-runner truthful capacity was rejected: %v", err)
	}
}

func TestPathPartsRejectsRoot(t *testing.T) {
	if parts := pathParts("///"); parts != nil {
		t.Fatalf("pathParts(root) = %v", parts)
	}
}

func multiScopeAuthorityConfig() Config {
	cfg := Default()
	cfg.Targets = []Target{
		{Type: "repo", Slug: "vitalyiegorov/knee-doctor", MaxActive: 4},
		{Type: "repo", Slug: "budgie-at/budgie", MaxActive: 4},
	}
	cfg.GitHub = GitHub{
		SessionOwner: "fleet-macmini", CanonicalJobInventory: true,
		App: GitHubApp{ClientID: "Iv1.test", KeychainService: "fleet", KeychainAccount: "app"},
		Installations: []GitHubInstallation{
			{Name: "personal", InstallationID: 146307296},
			{Name: "budgie", InstallationID: 146307362},
		},
		Scopes: []GitHubScope{
			{Name: "knee-doctor", Kind: ScopeRepository, ConfigURL: "https://github.com/vitalyiegorov/knee-doctor", Installation: "personal", Targets: []string{"vitalyiegorov/knee-doctor"}, ScaleSets: profileScaleSets("knee")},
			{Name: "budgie-at", Kind: ScopeOrganization, ConfigURL: "https://github.com/budgie-at", Installation: "budgie", Targets: []string{"budgie-at/budgie"}, ScaleSets: profileScaleSets("budgie")},
		},
	}
	return cfg
}

func profileScaleSets(scope string) []ScaleSet {
	return []ScaleSet{
		{Profile: "small", Name: "fleet-" + scope + "-linux-small", MaxCapacity: 4, Labels: []string{"self-hosted", "linux-tiered", "linux-small"}},
		{Profile: "medium", Name: "fleet-" + scope + "-linux-medium", MaxCapacity: 4, Labels: []string{"self-hosted", "linux-tiered", "linux-medium"}},
		{Profile: "large", Name: "fleet-" + scope + "-linux-large", MaxCapacity: 2, Labels: []string{"self-hosted", "linux-tiered", "linux-large"}},
		{Profile: "builder", Name: "fleet-" + scope + "-macos-builder", MaxCapacity: 1, Labels: []string{"self-hosted", "macOS", "ARM64", "macos-builder"}},
		{Profile: "maestro", Name: "fleet-" + scope + "-macos-maestro", MaxCapacity: 2, Labels: []string{"self-hosted", "macOS", "ARM64", "macos-maestro"}},
	}
}
