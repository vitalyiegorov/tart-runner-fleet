package config

import (
	"bytes"
	"strings"
	"testing"
)

// runnerVersionConfig is a minimal two-platform node. Both images are declared
// so a test only has to state the one fact it is about.
func runnerVersionConfig(t *testing.T, linuxVersion, macOSVersion, floor string) string {
	t.Helper()
	declare := func(key, value string) string {
		if value == "" {
			return ""
		}
		return `"` + key + `": "` + value + `",`
	}
	return `{
  "baseVm": "linux-runner-base-go",
  ` + declare("baseImageRunnerVersion", linuxVersion) + `
  ` + declare("runnerVersionFloor", floor) + `
  "vmPrefix": "trf-linux",
  "pollSeconds": 5,
  "maxLinuxWhenMacosIdle": 2,
  "maxLinuxCpu": 6,
  "maxLinuxMemoryMb": 12288,
  "linuxReservationAgeSeconds": 300,
  "linuxProfiles": [{"id": "xl", "label": "trf-linux-arm64-6x12", "cpu": 6, "memoryMb": 12288, "diskGb": 50}],
  "minFreeDiskGb": 60,
  "githubTimeoutSeconds": 15,
  "tartControlTimeoutSeconds": 45,
  "bootTimeoutSeconds": 180,
  "macosBurst": {
    "enabled": true,
    "baseVm": "macos-tartelet-base-go",
    ` + declare("baseImageRunnerVersion", macOSVersion) + `
    "vmPrefix": "trf-macos",
    "builder": {"id": "builder", "label": "trf-macos-arm64-6x12", "cpu": 6, "memoryMb": 12288, "maxActive": 1},
    "maestro": {"id": "maestro", "label": "trf-macos-arm64-4x7", "cpu": 4, "memoryMb": 7168, "maxActive": 2}
  },
  "github": {"configUrl": "https://github.com/o/r", "owner": "o", "installationId": 1,
    "scaleSets": [{"profile": "xl", "name": "trf-xl", "id": 1, "maxCapacity": 1,
      "labels": ["self-hosted", "linux-xl"]}]},
  "targets": [{"type": "repo", "slug": "o/r", "maxActive": 1}]
}`
}

func decodeRunnerVersionConfig(t *testing.T, body string) (Config, error) {
	t.Helper()
	return Decode(strings.NewReader(body))
}

// TestRunnerVersionDeclarationsAreValidated covers the whole of what
// configuration validation is allowed to refuse: a version that is not a runner
// version. It deliberately does NOT refuse a below-floor declaration — see
// TestBelowFloorConfigurationStillLoads and ADR 0041.
func TestRunnerVersionDeclarationsAreValidated(t *testing.T) {
	for name, testCase := range map[string]struct {
		linux, macOS, floor string
		wants               string
	}{
		"both declared":            {linux: "2.336.0", macOS: "2.336.0"},
		"neither declared":         {},
		"floor overridden":         {linux: "2.336.0", macOS: "2.336.0", floor: "2.336.0"},
		"linux not a version":      {linux: "latest", wants: `linux base image "linux-runner-base-go" declares runner version "latest"`},
		"macOS not a version":      {macOS: "v2.336.0", wants: `macOS base image "macos-tartelet-base-go" declares runner version "v2.336.0"`},
		"linux two components":     {linux: "2.336", wants: `declares runner version "2.336"`},
		"linux four components":    {linux: "2.336.0.1", wants: `declares runner version "2.336.0.1"`},
		"floor not a version":      {floor: "newest", wants: `runnerVersionFloor "newest"`},
		"floor with leading zeros": {floor: "2.336.00", wants: ""},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := decodeRunnerVersionConfig(t, runnerVersionConfig(t, testCase.linux, testCase.macOS, testCase.floor))
			switch {
			case testCase.wants == "" && err != nil:
				t.Fatalf("want accepted, got %v", err)
			case testCase.wants != "" && err == nil:
				t.Fatalf("want refusal naming %q, got none", testCase.wants)
			case testCase.wants != "" && !strings.Contains(err.Error(), testCase.wants):
				t.Fatalf("want error naming %q, got %v", testCase.wants, err)
			}
		})
	}
}

// TestBelowFloorConfigurationStillLoads is the deliberate decision of ADR 0041.
// A declaration below the floor is an operational fact that a `fleet doctor`
// check must shout about; refusing to decode it would take a node that is still
// running jobs off the air on its next restart, which is a worse outage than the
// one this whole issue exists to prevent.
func TestBelowFloorConfigurationStillLoads(t *testing.T) {
	cfg, err := decodeRunnerVersionConfig(t, runnerVersionConfig(t, "2.300.0", "2.300.0", "2.329.0"))
	if err != nil {
		t.Fatalf("want a below-floor configuration to decode, got %v", err)
	}
	for _, image := range cfg.RunnerImages() {
		if image.Compliant() {
			t.Fatalf("want %s judged below floor, got compliant", image.Platform)
		}
	}
}

func TestRunnerImagesJudgeEachDeclaredImage(t *testing.T) {
	for name, testCase := range map[string]struct {
		linux, macOS, floor string
		wantReasons         []string
	}{
		"both at the floor": {linux: "2.329.0", macOS: "2.329.0"},
		"both above it":     {linux: "2.336.0", macOS: "2.336.0"},
		"patch below": {linux: "2.328.9", macOS: "2.336.0",
			wantReasons: []string{`linux base image "linux-runner-base-go" carries actions/runner 2.328.9, below the 2.329.0 floor`}},
		"minor below": {linux: "2.336.0", macOS: "2.100.0",
			wantReasons: []string{`macOS base image "macos-tartelet-base-go" carries actions/runner 2.100.0, below the 2.329.0 floor`}},
		"raised floor leaves both behind": {linux: "2.335.1", macOS: "2.335.1", floor: "2.336.0",
			wantReasons: []string{
				`linux base image "linux-runner-base-go" carries actions/runner 2.335.1, below the 2.336.0 floor`,
				`macOS base image "macos-tartelet-base-go" carries actions/runner 2.335.1, below the 2.336.0 floor`}},
		"undeclared is not compliant": {macOS: "2.336.0",
			wantReasons: []string{`linux base image "linux-runner-base-go" declares no runner version, ` +
				`so its brownout compliance cannot be judged`}},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := decodeRunnerVersionConfig(t, runnerVersionConfig(t, testCase.linux, testCase.macOS, testCase.floor))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			reasons := []string{}
			for _, image := range cfg.RunnerImages() {
				if reason := image.Reason(); reason != "" {
					reasons = append(reasons, reason)
				}
			}
			if len(reasons) != len(testCase.wantReasons) {
				t.Fatalf("want %d reasons %q, got %d: %q", len(testCase.wantReasons), testCase.wantReasons, len(reasons), reasons)
			}
			for index, want := range testCase.wantReasons {
				if !strings.HasPrefix(reasons[index], want) {
					t.Fatalf("reason %d: want it to open with %q, got %q", index, want, reasons[index])
				}
			}
		})
	}
}

// TestRunnerImagesSkipsAnImageTheNodeNeverBoots keeps the check honest on an
// observe-only or Linux-only node: an image that boots no profile is not a
// compliance hole, so it is not reported at all.
func TestRunnerImagesSkipsAnImageTheNodeNeverBoots(t *testing.T) {
	body := strings.Replace(runnerVersionConfig(t, "2.336.0", "", ""), `"enabled": true`, `"enabled": false`, 1)
	cfg, err := decodeRunnerVersionConfig(t, body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	images := cfg.RunnerImages()
	if len(images) != 1 || images[0].Platform != capabilityPlatformLinux {
		t.Fatalf("want only the linux image, got %+v", images)
	}
	if floor := images[0].Floor; floor != DefaultRunnerVersionFloor {
		t.Fatalf("want the shipped floor %q, got %q", DefaultRunnerVersionFloor, floor)
	}
}

// TestRunnerVersionRoundTrips proves Encode/Decode carry the declaration, and
// that a node declaring nothing still encodes byte-for-byte what it always did.
func TestRunnerVersionRoundTrips(t *testing.T) {
	cfg, err := decodeRunnerVersionConfig(t, runnerVersionConfig(t, "2.336.0", "2.335.1", "2.330.0"))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var encoded bytes.Buffer
	if err := Encode(&encoded, cfg); err != nil {
		t.Fatalf("encode: %v", err)
	}
	again, err := Decode(bytes.NewReader(encoded.Bytes()))
	if err != nil {
		t.Fatalf("decode again: %v", err)
	}
	if again.Linux.BaseImageRunnerVersion != "2.336.0" || again.MacOS.BaseImageRunnerVersion != "2.335.1" ||
		again.RunnerVersionFloor != "2.330.0" {
		t.Fatalf("round trip lost the declaration: %+v", again)
	}
}

func TestUndeclaredRunnerVersionEncodesNoKey(t *testing.T) {
	cfg, err := decodeRunnerVersionConfig(t, runnerVersionConfig(t, "", "", ""))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var encoded bytes.Buffer
	if err := Encode(&encoded, cfg); err != nil {
		t.Fatalf("encode: %v", err)
	}
	for _, key := range []string{"baseImageRunnerVersion", "runnerVersionFloor"} {
		if bytes.Contains(encoded.Bytes(), []byte(key)) {
			t.Fatalf("want no %q key for a node that declares none, got %s", key, encoded.String())
		}
	}
}

// TestRunnerImageDeclarationSurvivesClone guards the same aliasing bug the
// capability slices are cloned for.
func TestRunnerImageDeclarationSurvivesClone(t *testing.T) {
	cfg, err := decodeRunnerVersionConfig(t, runnerVersionConfig(t, "2.336.0", "2.336.0", ""))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	clone := cfg.Clone()
	if clone.Linux.BaseImageRunnerVersion != cfg.Linux.BaseImageRunnerVersion ||
		clone.MacOS.BaseImageRunnerVersion != cfg.MacOS.BaseImageRunnerVersion ||
		clone.RunnerVersionFloor != cfg.RunnerVersionFloor {
		t.Fatalf("clone lost the declaration: %+v", clone)
	}
}

// TestCompareRunnerVersionsOrdersReleases pins the ordering the floor rule rests
// on, including the numeric case a string comparison gets wrong: 2.9.0 is older
// than 2.10.0, and `actions/runner` has already crossed that boundary twice.
func TestCompareRunnerVersionsOrdersReleases(t *testing.T) {
	for name, testCase := range map[string]struct {
		left, right string
		want        int
	}{
		"equal":                 {"2.336.0", "2.336.0", 0},
		"patch older":           {"2.336.0", "2.336.1", -1},
		"patch newer":           {"2.336.1", "2.336.0", 1},
		"minor older":           {"2.329.0", "2.336.0", -1},
		"minor is not lexical":  {"2.9.0", "2.10.0", -1},
		"major newer":           {"3.0.0", "2.336.0", 1},
		"leading zeros are not": {"2.336.00", "2.336.0", 0},
		// The grammar admits neither of the next two, so they can only mean the
		// grammar and the comparison have drifted apart. Both order the value
		// LAST, which fails the floor rather than silently clearing it.
		"unparsable component": {"2.x.0", "2.336.0", -1},
		"short":                {"2.336", "2.336.0", -1},
		"long":                 {"2.336.0.1", "2.336.0", 1},
	} {
		t.Run(name, func(t *testing.T) {
			if got := compareRunnerVersions(testCase.left, testCase.right); got != testCase.want {
				t.Fatalf("compare(%q, %q) = %d, want %d", testCase.left, testCase.right, got, testCase.want)
			}
		})
	}
}
