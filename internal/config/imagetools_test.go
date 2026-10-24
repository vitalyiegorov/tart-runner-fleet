package config

import (
	"bytes"
	"strings"
	"testing"
)

func linuxToolScaleSet(name string, tools ...string) ScaleSet {
	set := linuxScaleSet()
	set.Name, set.RequiresTools = name, tools
	return set
}

func macosToolScaleSet(name string, tools ...string) ScaleSet {
	set := macosScaleSet()
	set.Name, set.RequiresTools = name, tools
	return set
}

// TestToolDeclarationsAreValidated covers what decoding may refuse: a tool
// entry this package cannot compare exactly, and a requirement no image can
// answer for. It deliberately does NOT refuse a requirement the image lacks —
// see TestUncoveredToolRequirementStillLoads and ADR 0079.
func TestToolDeclarationsAreValidated(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr []string
	}{
		{name: "absent on both sides is a no-op", mutate: func(cfg *Config) { *cfg = scopedNode(linuxScaleSet()) }},
		{name: "exact versions on both sides", mutate: func(cfg *Config) {
			*cfg = scopedNode(linuxToolScaleSet("ota", "node@22.22.2", "pnpm@12.14.0"))
			cfg.Linux.BaseImageTools = []string{"node@22.22.2", "node@24.22.1", "pnpm@12.14.0"}
			cfg.MacOS.BaseImageTools = []string{"maestro@2.10.0", "python@3.12"}
		}},
		{name: "a declared tool with no version", mutate: func(cfg *Config) {
			cfg.Linux.BaseImageTools = []string{"node"}
		}, wantErr: []string{"linux base image", `"node"`}},
		{name: "a declared tool with a version range", mutate: func(cfg *Config) {
			cfg.MacOS.BaseImageTools = []string{"node@22.x"}
		}, wantErr: []string{"macOS base image", `"node@22.x"`}},
		{name: "a declared tool with a v prefix", mutate: func(cfg *Config) {
			cfg.Linux.BaseImageTools = []string{"node@v22.22.2"}
		}, wantErr: []string{`"node@v22.22.2"`}},
		{name: "a declared tool with an uppercase name", mutate: func(cfg *Config) {
			cfg.Linux.BaseImageTools = []string{"Node@22.22.2"}
		}, wantErr: []string{`"Node@22.22.2"`}},
		{name: "a declared tool listed twice", mutate: func(cfg *Config) {
			cfg.Linux.BaseImageTools = []string{"node@22.22.2", "node@22.22.2"}
		}, wantErr: []string{"linux base image", "twice"}},
		{name: "a required tool with invalid syntax", mutate: func(cfg *Config) {
			*cfg = scopedNode(linuxToolScaleSet("ota", "pnpm 12.14.0"))
		}, wantErr: []string{`scale set "ota"`, `"pnpm 12.14.0"`}},
		{name: "a required tool listed twice", mutate: func(cfg *Config) {
			*cfg = scopedNode(linuxToolScaleSet("ota", "pnpm@12.14.0", "pnpm@12.14.0"))
		}, wantErr: []string{`scale set "ota"`, "twice"}},
		{name: "a requirement whose profile this node does not declare", mutate: func(cfg *Config) {
			set := linuxToolScaleSet("ota", "node@22.22.2")
			set.Profile = "linux-nowhere"
			*cfg = scopedNode(set)
		}, wantErr: []string{"linux-nowhere", `"ota"`, "no base image answers"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := capabilityNode()
			test.mutate(&cfg)
			assertRefusalNames(t, cfg.Validate(), test.wantErr)
		})
	}
}

// assertRefusalNames passes a nil error when no fragment is wanted, and
// otherwise requires a refusal naming every fragment.
func assertRefusalNames(t *testing.T, err error, fragments []string) {
	t.Helper()
	if len(fragments) == 0 && err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
	if len(fragments) > 0 && err == nil {
		t.Fatalf("Validate() = nil, want a refusal naming %q", fragments)
	}
	for _, fragment := range fragments {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("Validate() = %q, missing %q", err, fragment)
		}
	}
}

// TestUncoveredToolRequirementStillLoads is the half validation must not do. A
// guest that lacks a pinned tool still runs the job — it downloads the tool
// first — so refusing the file would take a serving node off the air on its
// next restart over a bandwidth cost. `fleet doctor` is the loud channel.
func TestUncoveredToolRequirementStillLoads(t *testing.T) {
	cfg := scopedNode(macosToolScaleSet("maestro", "maestro@2.10.0"))
	cfg.MacOS.BaseImageTools = []string{"maestro@2.6.1"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want an uncovered requirement to load", err)
	}
}

// TestImageToolsNamesEveryVersionTheImageDoesNotCarry is the incident of issue
// #414: the macOS image carried Maestro 2.6.1, the consumer pinned 2.10.0, and
// every maestro shard downloaded 315 MB over the home uplink while every fleet
// surface read healthy. A newer version does not cover an older pin either —
// the consumer's action asks for the exact version it names.
func TestImageToolsNamesEveryVersionTheImageDoesNotCarry(t *testing.T) {
	cfg := scopedNode(
		linuxToolScaleSet("ota", "node@22.22.2", "pnpm@12.14.0"),
		linuxToolScaleSet("lint", "node@22.19.0"),
		macosToolScaleSet("maestro", "maestro@2.10.0", "node@22.22.2"),
		macosToolScaleSet("maestro-legacy", "maestro@2.10.0"),
	)
	cfg.Linux.BaseImageTools = []string{"pnpm@12.14.0", "node@22.22.2", "node@24.22.1"}
	cfg.MacOS.BaseImageTools = []string{"node@22.22.2", "maestro@2.11.0"}
	images := cfg.ImageTools()
	if len(images) != 2 {
		t.Fatalf("ImageTools() = %+v, want one row per booted image", images)
	}
	linux, macOS := images[0], images[1]
	if linux.Platform != "linux" || linux.VM != "linux-runner-base" || macOS.Platform != "macOS" ||
		macOS.VM != "macos-tartelet-base" {
		t.Fatalf("rows are not linux then macOS: %+v", images)
	}
	if got := strings.Join(linux.Declared, ","); got != "node@22.22.2,node@24.22.1,pnpm@12.14.0" {
		t.Errorf("linux declared = %s, want it sorted", got)
	}
	if got := missingTools(linux); got != "node@22.19.0" {
		t.Errorf("linux missing = %s", got)
	}
	if got := missingTools(macOS); got != "maestro@2.10.0" {
		t.Errorf("macOS missing = %s", got)
	}
	if got := strings.Join(macOS.Missing[0].RequiredBy, ","); got != "maestro,maestro-legacy" {
		t.Errorf("maestro@2.10.0 required by %s, want both sets", got)
	}
	reason := macOS.Reason()
	for _, fragment := range []string{`macOS base image "macos-tartelet-base"`, "maestro@2.10.0",
		"maestro, maestro-legacy", "downloads"} {
		if !strings.Contains(reason, fragment) {
			t.Errorf("Reason() = %q, missing %q", reason, fragment)
		}
	}
	if linux.Covered() || macOS.Covered() {
		t.Error("an image missing a required version must not be covered")
	}
}

func missingTools(image ImageTools) string {
	tools := make([]string, 0, len(image.Missing))
	for _, gap := range image.Missing {
		tools = append(tools, gap.Tool)
	}
	return strings.Join(tools, ",")
}

// TestImageToolsWithNothingRequiredIsCovered keeps absence a pass where it is
// provable from the file: no scale set requires a tool, so no job is expected
// to download one, whatever the image declares.
func TestImageToolsWithNothingRequiredIsCovered(t *testing.T) {
	cfg := scopedNode(linuxScaleSet(), macosScaleSet())
	cfg.MacOS.BaseImageTools = []string{"maestro@2.6.1"}
	for _, image := range cfg.ImageTools() {
		if !image.Covered() || image.Reason() != "" || len(image.Missing) != 0 {
			t.Errorf("%s image = %+v, want covered", image.Platform, image)
		}
	}
	required := scopedNode(linuxToolScaleSet("ota", "node@22.22.2"))
	if image := required.ImageTools()[0]; image.Covered() || missingTools(image) != "node@22.22.2" {
		t.Errorf("an image declaring nothing against a requirement = %+v, want the requirement missing", image)
	}
}

// TestImageToolsReportsOnlyBootedImages mirrors RunnerImages: an image nothing
// is routed to cannot cost a download.
func TestImageToolsReportsOnlyBootedImages(t *testing.T) {
	cfg := scopedNode(linuxToolScaleSet("ota", "node@22.22.2"))
	cfg.MacOS.Enabled = false
	images := cfg.ImageTools()
	if len(images) != 1 || images[0].Platform != "linux" {
		t.Fatalf("ImageTools() = %+v, want only the linux row", images)
	}
	cfg.Linux.Profiles = nil
	cfg.GitHub.Scopes = nil
	cfg.MacOS.Enabled = true
	images = cfg.ImageTools()
	if len(images) != 1 || images[0].Platform != "macOS" {
		t.Fatalf("a node with no Linux profile = %+v, want only the macOS row", images)
	}
}

// TestToolDeclarationsRoundTrip proves the three keys survive an encode and
// decode, and that Clone does not alias them.
func TestToolDeclarationsRoundTrip(t *testing.T) {
	cfg := scopedNode(linuxToolScaleSet("ota", "node@22.22.2"), macosToolScaleSet("maestro", "maestro@2.10.0"))
	cfg.Linux.BaseImageTools = []string{"node@22.22.2"}
	cfg.MacOS.BaseImageTools = []string{"maestro@2.10.0"}
	var encoded bytes.Buffer
	if err := Encode(&encoded, cfg); err != nil {
		t.Fatalf("Encode() = %v", err)
	}
	for _, fragment := range []string{`"baseImageTools"`, `"requiresTools"`} {
		if !strings.Contains(encoded.String(), fragment) {
			t.Fatalf("Encode() dropped %s:\n%s", fragment, encoded.String())
		}
	}
	decoded, err := Decode(bytes.NewReader(encoded.Bytes()))
	if err != nil {
		t.Fatalf("Decode(Encode()) = %v", err)
	}
	if strings.Join(decoded.Linux.BaseImageTools, ",") != "node@22.22.2" ||
		strings.Join(decoded.MacOS.BaseImageTools, ",") != "maestro@2.10.0" ||
		strings.Join(decoded.GitHub.Scopes[0].ScaleSets[1].RequiresTools, ",") != "maestro@2.10.0" {
		t.Fatalf("round trip lost a declaration: %+v %+v", decoded.Linux.BaseImageTools, decoded.MacOS.BaseImageTools)
	}
	clone := decoded.Clone()
	clone.Linux.BaseImageTools[0] = "mutated"
	clone.MacOS.BaseImageTools[0] = "mutated"
	clone.GitHub.Scopes[0].ScaleSets[0].RequiresTools[0] = "mutated"
	if decoded.Linux.BaseImageTools[0] == "mutated" || decoded.MacOS.BaseImageTools[0] == "mutated" ||
		decoded.GitHub.Scopes[0].ScaleSets[0].RequiresTools[0] == "mutated" {
		t.Error("Clone() aliased a tool slice")
	}
}
