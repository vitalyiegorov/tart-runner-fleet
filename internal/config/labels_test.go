package config

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestCanonicalLabelDescribesTheVectorAndNothingElse(t *testing.T) {
	linux := []Profile{
		{ID: "linux-1x2", Label: "trf-linux-arm64-1x2", CPU: 1, MemoryMiB: 2048},
		{ID: "linux-2x8", Label: "trf-linux-arm64-2x8", CPU: 2, MemoryMiB: 8192},
		{ID: "linux-8x16", Label: "trf-linux-arm64-8x16", CPU: 8, MemoryMiB: 16384},
	}
	for _, profile := range linux {
		set, err := profile.labelSet(canonicalLinuxOS)
		if err != nil {
			t.Fatalf("labelSet(%s) = %v", profile.ID, err)
		}
		if set.Canonical != profile.Label {
			t.Fatalf("canonical label = %q, want %q", set.Canonical, profile.Label)
		}
		if len(set.Aliases) != 0 {
			t.Fatalf("a canonically named profile needs no alias, got %#v", set.Aliases)
		}
	}
	macOS := Profile{ID: "macos-4x7", Label: "trf-macos-arm64-4x7", CPU: 4, MemoryMiB: 7168, MaxActive: 2}
	set, err := macOS.labelSet(canonicalMacOS)
	if err != nil || set.Canonical != "trf-macos-arm64-4x7" {
		t.Fatalf("macOS canonical label = %q, %v", set.Canonical, err)
	}
}

func TestLegacyLabelBecomesAnAliasOfTheCanonicalLabel(t *testing.T) {
	profile := Profile{ID: "large", Label: "linux-large", Aliases: []string{"linux-burst", "LINUX-LARGE"},
		CPU: 4, MemoryMiB: 8192}
	set, err := profile.labelSet(canonicalLinuxOS)
	if err != nil {
		t.Fatalf("labelSet() = %v", err)
	}
	if set.Canonical != "trf-linux-arm64-4x8" {
		t.Fatalf("canonical label = %q", set.Canonical)
	}
	// The configured label leads the aliases so existing routing is unchanged,
	// and a case-different repeat is folded away rather than advertised twice.
	assertLabels(t, set.All(), "trf-linux-arm64-4x8", "linux-large", "linux-burst")
	if !set.Contains("Linux-Large") || !set.Contains("TRF-LINUX-ARM64-4X8") || set.Contains("linux-small") {
		t.Fatalf("membership = %#v", set)
	}
}

func TestConfigurationCannotNameAShapeItDoesNotProvision(t *testing.T) {
	tests := map[string]struct {
		profile  Profile
		fragment string
	}{
		"canonical label contradicts the vector": {
			profile:  Profile{ID: "liar", Label: "trf-linux-arm64-8x16", CPU: 1, MemoryMiB: 2048},
			fragment: "describes a vector it does not have",
		},
		"canonical alias contradicts the vector": {
			profile:  Profile{ID: "liar", Label: "linux-small", Aliases: []string{"trf-linux-arm64-4x8"}, CPU: 1, MemoryMiB: 2048},
			fragment: "describes a vector it does not have",
		},
		"memory is not a whole GiB": {
			profile:  Profile{ID: "ragged", Label: "linux-odd", CPU: 2, MemoryMiB: 6000},
			fragment: "not a whole GiB",
		},
		"vector is empty": {
			profile:  Profile{ID: "empty", Label: "linux-empty"},
			fragment: "positive CPU and memory vector",
		},
		"label is not a runner label": {
			profile:  Profile{ID: "bad", Label: "-leading-dash", CPU: 1, MemoryMiB: 1024},
			fragment: "not a valid GitHub runner label",
		},
		"alias is blank": {
			profile:  Profile{ID: "blank", Label: "linux-small", Aliases: []string{"  "}, CPU: 1, MemoryMiB: 1024},
			fragment: "empty runner label",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := test.profile.labelSet(canonicalLinuxOS)
			if err == nil || !strings.Contains(err.Error(), test.fragment) {
				t.Fatalf("labelSet() error = %v, want one mentioning %q", err, test.fragment)
			}
		})
	}
}

func TestTwoProfilesCannotAnswerToTheSameRunnerLabel(t *testing.T) {
	cfg := Default()
	cfg.Linux.Profiles[1].Aliases = []string{"LINUX-SMALL"}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "resolves to both profile") {
		t.Fatalf("Validate() error = %v, want an ambiguous-label rejection", err)
	}
	if sets := cfg.ProfileLabelSets(); len(sets) != 1 {
		t.Fatalf("a rejected vocabulary yields only the profiles proved so far, got %#v", sets)
	}
}

func TestAnEnabledMacOSProfileCannotNameAShapeItDoesNotProvision(t *testing.T) {
	cfg := Default()
	cfg.MacOS.Maestro.Label = "trf-macos-arm64-1x1"
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "describes a vector it does not have") {
		t.Fatalf("Validate() error = %v, want a macOS label-vector rejection", err)
	}
}

func TestDisabledMacOSContributesNoLabels(t *testing.T) {
	cfg := Default()
	cfg.MacOS.Enabled = false
	sets := cfg.ProfileLabelSets()
	if len(sets) != len(cfg.Linux.Profiles) {
		t.Fatalf("label sets = %#v, want only the Linux profiles", sets)
	}
	if _, exists := sets["builder"]; exists {
		t.Fatal("a disabled macOS burst still published labels")
	}
}

func TestAdvertiseKeepsConfiguredLabelsAndAddsEveryName(t *testing.T) {
	set := LabelSet{Canonical: "trf-linux-arm64-4x8", Aliases: []string{"linux-large", "linux-burst"}}
	assertLabels(t, set.Advertise([]string{"self-hosted", "linux-tiered", "LINUX-LARGE", ""}),
		"self-hosted", "linux-tiered", "LINUX-LARGE", "trf-linux-arm64-4x8", "linux-burst")
	assertLabels(t, LabelSet{}.Advertise(nil))
}

func TestExampleConfigLabelsAreDerivedFromTheirVectors(t *testing.T) {
	cfg := decodeExampleConfig(t)
	sets := cfg.ProfileLabelSets()
	if len(sets) != len(cfg.Linux.Profiles)+2 {
		t.Fatalf("label sets = %#v", sets)
	}
	for id, set := range sets {
		if !strings.HasPrefix(set.Canonical, canonicalLabelPrefix+"-") {
			t.Fatalf("profile %s canonical label = %q", id, set.Canonical)
		}
	}
	// Every retired role or tier name still resolves, so a consumer workflow
	// keeps routing across the migration without an edit.
	legacy := map[string]string{
		"linux-small": "linux-1x2", "linux-medium": "linux-2x4", "linux-large": "linux-4x8",
		"linux-xl": "linux-6x12", "macos-builder": "macos-6x12", "macos-maestro": "macos-4x7",
	}
	for alias, id := range legacy {
		set, exists := sets[id]
		if !exists || !set.Contains(alias) {
			t.Fatalf("alias %q no longer resolves to profile %s: %#v", alias, id, set)
		}
	}
}

func TestAliasesSurviveDecodeEncodeAndClone(t *testing.T) {
	cfg := decodeExampleConfig(t)
	var encoded bytes.Buffer
	if err := Encode(&encoded, cfg); err != nil {
		t.Fatalf("Encode() = %v", err)
	}
	if !strings.Contains(encoded.String(), `"aliases"`) {
		t.Fatalf("encoded configuration dropped profile aliases:\n%s", encoded.String())
	}
	round, err := Decode(bytes.NewReader(encoded.Bytes()))
	if err != nil {
		t.Fatalf("Decode(Encode()) = %v", err)
	}
	if len(round.Linux.Profiles[0].Aliases) != 1 || round.MacOS.Builder.Aliases[0] != "macos-builder" {
		t.Fatalf("round-tripped aliases = %#v / %#v", round.Linux.Profiles[0].Aliases, round.MacOS.Builder.Aliases)
	}
	clone := round.Clone()
	clone.Linux.Profiles[0].Aliases[0] = "mutated"
	clone.MacOS.Builder.Aliases[0] = "mutated"
	clone.MacOS.Maestro.Aliases[0] = "mutated"
	if round.Linux.Profiles[0].Aliases[0] == "mutated" || round.MacOS.Builder.Aliases[0] == "mutated" ||
		round.MacOS.Maestro.Aliases[0] == "mutated" {
		t.Fatal("Clone() shares alias storage with its source")
	}
}

func TestScaleSetMayNotAdvertiseAnotherVariantsCanonicalLabel(t *testing.T) {
	cfg := multiScopeAuthorityConfig()
	cfg.GitHub.Scopes[0].ScaleSets[0].Labels = append(cfg.GitHub.Scopes[0].ScaleSets[0].Labels, "trf-linux-arm64-4x8")
	err := cfg.ValidateAuthority()
	if err == nil || !strings.Contains(err.Error(), "canonical label of profile") {
		t.Fatalf("ValidateAuthority() error = %v, want a cross-variant routing rejection", err)
	}
}

func TestScopeExposesOnlyTheVariantsItLists(t *testing.T) {
	cfg := multiScopeAuthorityConfig()
	// Two of the five profiles are enough for this scope; the others cost
	// nothing here and remain available to scopes that ask for them.
	cfg.GitHub.Scopes[0].ScaleSets = cfg.GitHub.Scopes[0].ScaleSets[:2]
	if err := cfg.ValidateAuthority(); err != nil {
		t.Fatalf("ValidateAuthority() = %v, want a partial variant matrix to be accepted", err)
	}
	cfg.GitHub.Scopes[0].ScaleSets[0].Labels = []string{"self-hosted", "trf-linux-arm64-1x2"}
	if err := cfg.ValidateAuthority(); err != nil {
		t.Fatalf("ValidateAuthority() = %v, want a canonical route label to be accepted", err)
	}
}

func decodeExampleConfig(t *testing.T) Config {
	t.Helper()
	file, err := os.Open("../../config/fleet.example.json") // #nosec G304 -- fixed repository fixture.
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	cfg, err := Decode(file)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func assertLabels(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("labels = %#v, want %#v", got, want)
	}
	for index, label := range want {
		if got[index] != label {
			t.Fatalf("labels = %#v, want %#v", got, want)
		}
	}
}
