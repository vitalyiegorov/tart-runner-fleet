package config

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// capabilityNode builds a two-platform node whose scale sets can be rewritten by
// each case. It is deliberately close to config/fleet.example.json so a case
// changes exactly one thing about a configuration that is otherwise real.
func capabilityNode() Config {
	cfg := Default()
	cfg.Linux.Profiles = []Profile{
		{ID: "linux-6x12", Label: "trf-linux-arm64-6x12", Aliases: []string{"linux-xl"},
			Resources: Resources{CPU: 6, MemoryMiB: 12288}, DiskGiB: 50},
	}
	cfg.Linux.Capacity = Resources{CPU: 6, MemoryMiB: 12288}
	cfg.Linux.MaxInstances = 1
	cfg.MacOS.Builder = Profile{ID: "macos-6x12", Label: "trf-macos-arm64-6x12", Aliases: []string{"macos-builder"},
		Resources: Resources{CPU: 6, MemoryMiB: 12288}, MaxActive: 1}
	cfg.MacOS.Maestro = Profile{ID: "macos-4x7", Label: "trf-macos-arm64-4x7", Aliases: []string{"macos-maestro"},
		Resources: Resources{CPU: 4, MemoryMiB: 7168}, MaxActive: 1}
	return cfg
}

func linuxScaleSet(requires ...string) ScaleSet {
	return ScaleSet{Profile: "linux-6x12", Name: "trf-sudoku-xl-studio", ID: 1, MaxCapacity: 2,
		Labels: []string{"self-hosted", "linux-tiered", "linux-xl"}, RequiresCapabilities: requires}
}

func macosScaleSet(requires ...string) ScaleSet {
	return ScaleSet{Profile: "macos-4x7", Name: "trf-sudoku-maestro", ID: 2, MaxCapacity: 2,
		Labels: []string{"self-hosted", "macos-maestro"}, RequiresCapabilities: requires}
}

func scopedNode(scaleSets ...ScaleSet) Config {
	cfg := capabilityNode()
	cfg.GitHub.Scopes = []GitHubScope{{Name: "sudoku", Kind: ScopeRepository,
		ConfigURL: "https://github.com/vitalyiegorov/suuudokuuu", Installation: "personal",
		Targets: []string{"vitalyiegorov/suuudokuuu"}, ScaleSets: scaleSets}}
	return cfg
}

// TestCapabilityDeclarationsAreValidated covers both the declaring and the
// requiring side, in the scoped model and in the legacy flat list, because a
// capability the image does not carry is exactly as wrong in either.
func TestCapabilityDeclarationsAreValidated(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr []string
	}{
		{name: "absent on both sides is a no-op", mutate: func(cfg *Config) {
			*cfg = scopedNode(linuxScaleSet())
		}},
		{name: "linux requirement declared by the linux base image", mutate: func(cfg *Config) {
			*cfg = scopedNode(linuxScaleSet("redroid-android"))
			cfg.Linux.BaseImageCapabilities = []string{"container-runtime", "redroid-android"}
		}},
		{name: "linux requirement the linux base image does not declare", mutate: func(cfg *Config) {
			*cfg = scopedNode(linuxScaleSet("redroid-android"))
			cfg.Linux.BaseImageCapabilities = []string{"container-runtime"}
		}, wantErr: []string{"sudoku", "trf-sudoku-xl-studio", "redroid-android", "linux-xl", "linux-runner-base"}},
		{name: "macOS requirement is compared against the macOS base image", mutate: func(cfg *Config) {
			*cfg = scopedNode(macosScaleSet("maestro-cli"))
			cfg.Linux.BaseImageCapabilities = []string{"maestro-cli"}
		}, wantErr: []string{"sudoku", "trf-sudoku-maestro", "maestro-cli", "macos-maestro", "macos-tartelet-base"}},
		{name: "macOS requirement declared by the macOS base image", mutate: func(cfg *Config) {
			*cfg = scopedNode(macosScaleSet("maestro-cli"))
			cfg.MacOS.BaseImageCapabilities = []string{"jvm", "maestro-cli"}
		}},
		{name: "legacy flat list is covered too", mutate: func(cfg *Config) {
			cfg.GitHub.ScaleSets = []ScaleSet{linuxScaleSet("redroid-android")}
		}, wantErr: []string{"github.scaleSets", "trf-sudoku-xl-studio", "redroid-android", "linux-xl"}},
		{name: "a requirement whose profile this node does not declare", mutate: func(cfg *Config) {
			set := linuxScaleSet("redroid-android")
			set.Profile = "linux-nowhere"
			*cfg = scopedNode(set)
			cfg.Linux.BaseImageCapabilities = []string{"redroid-android"}
		}, wantErr: []string{"linux-nowhere", "trf-sudoku-xl-studio"}},
		{name: "a macOS requirement on a node with macOS disabled", mutate: func(cfg *Config) {
			*cfg = scopedNode(macosScaleSet("maestro-cli"))
			cfg.MacOS.Enabled = false
		}, wantErr: []string{"macos-4x7", "trf-sudoku-maestro"}},
		{name: "declared capability with invalid syntax", mutate: func(cfg *Config) {
			cfg.Linux.BaseImageCapabilities = []string{"Redroid_Android"}
		}, wantErr: []string{"linux base image", "Redroid_Android"}},
		{name: "declared capability that is empty", mutate: func(cfg *Config) {
			cfg.Linux.BaseImageCapabilities = []string{""}
		}, wantErr: []string{"linux base image"}},
		{name: "declared capability listed twice", mutate: func(cfg *Config) {
			cfg.Linux.BaseImageCapabilities = []string{"redroid-android", "redroid-android"}
		}, wantErr: []string{"linux base image", "redroid-android"}},
		{name: "declared macOS capability with invalid syntax", mutate: func(cfg *Config) {
			cfg.MacOS.BaseImageCapabilities = []string{"-xcode"}
		}, wantErr: []string{"macOS base image", "-xcode"}},
		{name: "required capability with invalid syntax", mutate: func(cfg *Config) {
			*cfg = scopedNode(linuxScaleSet("redroid android"))
		}, wantErr: []string{"trf-sudoku-xl-studio", "redroid android"}},
		{name: "required capability listed twice", mutate: func(cfg *Config) {
			*cfg = scopedNode(linuxScaleSet("redroid-android", "redroid-android"))
			cfg.Linux.BaseImageCapabilities = []string{"redroid-android"}
		}, wantErr: []string{"trf-sudoku-xl-studio", "redroid-android"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := capabilityNode()
			test.mutate(&cfg)
			err := cfg.Validate()
			if len(test.wantErr) == 0 {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Validate() = nil, want a capability failure")
			}
			for _, fragment := range test.wantErr {
				if !strings.Contains(err.Error(), fragment) {
					t.Errorf("Validate() = %q, missing %q", err, fragment)
				}
			}
		})
	}
}

// TestCapabilityDeclarationsRoundTrip proves the two new keys survive an encode
// and decode unchanged, which is what lets `fleet scale-sets provision` rewrite a
// file that uses them without dropping the declaration.
func TestCapabilityDeclarationsRoundTrip(t *testing.T) {
	cfg := scopedNode(linuxScaleSet("redroid-android"), macosScaleSet("maestro-cli"))
	cfg.Linux.BaseImageCapabilities = []string{"container-runtime", "redroid-android"}
	cfg.MacOS.BaseImageCapabilities = []string{"jvm", "maestro-cli"}
	var encoded bytes.Buffer
	if err := Encode(&encoded, cfg); err != nil {
		t.Fatalf("Encode() = %v", err)
	}
	for _, fragment := range []string{`"baseImageCapabilities"`, `"requiresCapabilities"`, "redroid-android", "maestro-cli"} {
		if !strings.Contains(encoded.String(), fragment) {
			t.Fatalf("Encode() dropped %s:\n%s", fragment, encoded.String())
		}
	}
	decoded, err := Decode(bytes.NewReader(encoded.Bytes()))
	if err != nil {
		t.Fatalf("Decode(Encode()) = %v", err)
	}
	if strings.Join(decoded.Linux.BaseImageCapabilities, ",") != "container-runtime,redroid-android" {
		t.Errorf("linux capabilities = %v", decoded.Linux.BaseImageCapabilities)
	}
	if strings.Join(decoded.MacOS.BaseImageCapabilities, ",") != "jvm,maestro-cli" {
		t.Errorf("macOS capabilities = %v", decoded.MacOS.BaseImageCapabilities)
	}
	if strings.Join(decoded.GitHub.Scopes[0].ScaleSets[0].RequiresCapabilities, ",") != "redroid-android" {
		t.Errorf("required capabilities = %v", decoded.GitHub.Scopes[0].ScaleSets[0].RequiresCapabilities)
	}
	clone := decoded.Clone()
	clone.Linux.BaseImageCapabilities[0] = "mutated"
	clone.GitHub.Scopes[0].ScaleSets[0].RequiresCapabilities[0] = "mutated"
	if decoded.Linux.BaseImageCapabilities[0] == "mutated" || decoded.GitHub.Scopes[0].ScaleSets[0].RequiresCapabilities[0] == "mutated" {
		t.Error("Clone() aliased a capability slice")
	}
}

// TestUnusedCapabilityFieldsEncodeByteForByte is the compatibility gate the issue
// asks for: a configuration that does not use the feature must encode to exactly
// the bytes it encoded to before the feature existed, so a release older than
// this one still decodes it with DisallowUnknownFields. The golden file was
// produced by the encoder as it stood before this change.
func TestUnusedCapabilityFieldsEncodeByteForByte(t *testing.T) {
	file, err := os.Open("../../config/fleet.example.json") // #nosec G304 -- fixed repository fixture.
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	cfg, err := Decode(file)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Linux.BaseImageCapabilities != nil || cfg.MacOS.BaseImageCapabilities != nil {
		t.Fatal("the example configuration must not declare capabilities")
	}
	var encoded bytes.Buffer
	if err := Encode(&encoded, cfg); err != nil {
		t.Fatalf("Encode() = %v", err)
	}
	want, err := os.ReadFile("testdata/fleet.example.encoded.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded.Bytes(), want) {
		t.Fatalf("encoding a capability-free configuration changed:\n--- got\n%s\n--- want\n%s", encoded.String(), want)
	}
}

// TestProfileRequiredCapabilitiesUnionEveryScaleSet is what the daemon threads
// to the guest. An instance records its profile, not its scale set, so the guest
// must satisfy every requirement routed to that profile.
func TestProfileRequiredCapabilitiesUnionEveryScaleSet(t *testing.T) {
	cfg := capabilityNode()
	cfg.Linux.BaseImageCapabilities = []string{"container-runtime", "jdk", "redroid-android"}
	cfg.MacOS.BaseImageCapabilities = []string{"maestro-cli"}
	cfg.GitHub.ScaleSets = []ScaleSet{named(linuxScaleSet("redroid-android", "container-runtime"), "flat")}
	cfg.GitHub.Scopes = []GitHubScope{
		{Name: "sudoku", ScaleSets: []ScaleSet{named(linuxScaleSet("jdk", "container-runtime"), "scoped")}},
		{Name: "budgie", ScaleSets: []ScaleSet{named(macosScaleSet("maestro-cli"), "mac"), named(macosScaleSet(), "none")}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	required := cfg.ProfileRequiredCapabilities()
	if strings.Join(required["linux-6x12"], ",") != "container-runtime,jdk,redroid-android" {
		t.Errorf("linux-6x12 requires %v", required["linux-6x12"])
	}
	if strings.Join(required["macos-4x7"], ",") != "maestro-cli" {
		t.Errorf("macos-4x7 requires %v", required["macos-4x7"])
	}
	if len(required) != 2 {
		t.Errorf("a profile no scale set requires anything of must be absent: %v", required)
	}
}
