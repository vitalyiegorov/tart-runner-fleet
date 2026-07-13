package config

import (
	"os"
	"strings"
	"testing"
)

const selfRepository = "vitalyiegorov/tart-runner-fleet"

func TestExampleConfigCanBuildItsSuccessor(t *testing.T) {
	file, err := os.Open("../../config/fleet.example.json") // #nosec G304 -- fixed repository fixture.
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	cfg, err := Decode(file)
	if err != nil {
		t.Fatal(err)
	}

	selfCap := 0
	for _, target := range cfg.Targets {
		if target.Slug == selfRepository {
			selfCap = target.MaxActive
			break
		}
	}
	if selfCap < 3 {
		t.Fatalf("%s must allow the three-job CI fanout, cap=%d", selfRepository, selfCap)
	}

	profiles := make(map[string]Profile, len(cfg.Linux.Profiles))
	for _, profile := range cfg.Linux.Profiles {
		profiles[profile.ID] = profile.normalized()
	}
	medium, mediumOK := profiles["medium"]
	large, largeOK := profiles["large"]
	if !mediumOK || !largeOK {
		t.Fatal("self-hosted CI requires medium and large Linux profiles")
	}
	cpu := 2*medium.Resources.CPU + large.Resources.CPU
	memory := 2*medium.Resources.MemoryMiB + large.Resources.MemoryMiB
	if cpu != cfg.Linux.Capacity.CPU || memory != cfg.Linux.Capacity.MemoryMiB {
		t.Fatalf("CI fanout must exactly fill the Linux envelope: fanout=%dCPU/%dMiB capacity=%dCPU/%dMiB",
			cpu, memory, cfg.Linux.Capacity.CPU, cfg.Linux.Capacity.MemoryMiB)
	}
}

func TestSuccessfulMainCIBuildPublishesItsVerifiedArtifact(t *testing.T) {
	workflow, err := os.ReadFile("../../.github/workflows/main-release.yml") // #nosec G304 -- fixed repository fixture.
	if err != nil {
		t.Fatal(err)
	}
	text := string(workflow)
	for _, required := range []string{
		"workflow_run:",
		"workflows: [CI]",
		"types: [completed]",
		"branches: [main]",
		"github.event.workflow_run.conclusion == 'success'",
		"github.event.workflow_run.event == 'push'",
		"actions: read",
		"contents: write",
		"run-id: ${{ github.event.workflow_run.id }}",
		"github-token: ${{ github.token }}",
		"gh release create",
		"--target \"$HEAD_SHA\"",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("main release workflow must contain %q", required)
		}
	}
	if strings.Contains(text, "--prerelease") {
		t.Error("successful main builds must publish production releases, not prereleases")
	}

	ci, err := os.ReadFile("../../.github/workflows/ci.yml") // #nosec G304 -- fixed repository fixture.
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ci), `v0.1.${GITHUB_RUN_NUMBER}+main.${SHORT_SHA}`) {
		t.Error("main CI must embed the immutable production release version in its verified binaries")
	}

	manualRelease, err := os.ReadFile("../../.github/workflows/release.yml") // #nosec G304 -- fixed repository fixture.
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manualRelease), `!v*.*.*\+main.*`) {
		t.Error("the manual tag workflow must ignore automatically generated main release tags")
	}

	buildScript, err := os.ReadFile("../../scripts/build-release.sh") // #nosec G304 -- fixed repository fixture.
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(buildScript), "RELEASE_VERSION") {
		t.Error("release artifacts must carry a machine-readable version manifest")
	}
	for _, required := range []string{
		"./cmd/tart-runner-fleet-bootstrap",
		"tart-runner-fleet-bootstrap.cdx.json",
		"tart-runner-fleet-bootstrap",
	} {
		if !strings.Contains(string(buildScript), required) {
			t.Errorf("release artifacts must include the secret-safe guest bootstrap helper %q", required)
		}
	}

	mainRelease, err := os.ReadFile("../../.github/workflows/main-release.yml") // #nosec G304 -- fixed repository fixture.
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"tart-runner-fleet-bootstrap", "tart-runner-fleet-bootstrap.cdx.json"} {
		if !strings.Contains(string(mainRelease), required) {
			t.Errorf("main release verification must require bootstrap asset %q", required)
		}
	}
}

func TestLaunchdTemplateCanResolveRequiredOperatorTools(t *testing.T) {
	plist, err := os.ReadFile("../../launchd/com.vitalyiegorov.tart-runner-fleet.plist") // #nosec G304 -- fixed repository fixture.
	if err != nil {
		t.Fatal(err)
	}
	text := string(plist)
	for _, required := range []string{
		"<key>EnvironmentVariables</key>",
		"<key>PATH</key>",
		"/opt/homebrew/bin",
		"/usr/local/bin",
		"/usr/bin:/bin:/usr/sbin:/sbin",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("launchd template must contain %q so fleetd can resolve tart on Intel and Apple Silicon hosts", required)
		}
	}
}
