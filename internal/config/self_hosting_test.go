package config

import (
	"os"
	"os/exec"
	"path/filepath"
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
	applicationCaps := map[string]int{}
	for _, target := range cfg.Targets {
		if target.Slug == selfRepository {
			selfCap = target.MaxActive
			continue
		}
		applicationCaps[target.Slug] = target.MaxActive
	}
	if selfCap < 3 {
		t.Fatalf("%s must allow the three-job CI fanout, cap=%d", selfRepository, selfCap)
	}
	for repository, cap := range applicationCaps {
		if cap != cfg.Linux.MaxInstances {
			t.Errorf("%s application cap=%d, want fleet capacity=%d", repository, cap, cfg.Linux.MaxInstances)
		}
	}

	// Profiles are looked up by canonical resource label, never by an id or a
	// size adjective: the label is derived from the vector (ADR 0032), so this
	// assertion cannot be satisfied by renaming anything.
	profiles := make(map[string]Profile, len(cfg.Linux.Profiles))
	for _, profile := range cfg.Linux.Profiles {
		profiles[profile.Label] = profile.normalized()
	}
	medium, mediumOK := profiles["trf-linux-arm64-2x4"]
	large, largeOK := profiles["trf-linux-arm64-4x8"]
	if !mediumOK || !largeOK {
		t.Fatal("self-hosted CI requires the 2x4 and 4x8 Linux variants")
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
		"tart-runner-fleet-bootstrap-darwin-arm64.cdx.json",
		"tart-runner-fleet-bootstrap-linux-arm64.cdx.json",
		"tart-runner-fleet-bootstrap-darwin-arm64",
		"tart-runner-fleet-bootstrap-linux-arm64",
		"com.vitalyiegorov.tart-runner-fleet.authority.plist",
		"com.vitalyiegorov.tart-runner-fleet.canary.plist",
		"com.vitalyiegorov.tart-runner-fleet.shadow.plist",
		"render-launchd.sh",
	} {
		if !strings.Contains(string(buildScript), required) {
			t.Errorf("release artifacts must include the secret-safe guest bootstrap helper %q", required)
		}
	}

	mainRelease, err := os.ReadFile("../../.github/workflows/main-release.yml") // #nosec G304 -- fixed repository fixture.
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"tart-runner-fleet-bootstrap-darwin-arm64",
		"tart-runner-fleet-bootstrap-linux-arm64",
		"tart-runner-fleet-bootstrap-darwin-arm64.cdx.json",
		"tart-runner-fleet-bootstrap-linux-arm64.cdx.json",
		"com.vitalyiegorov.tart-runner-fleet.authority.plist",
		"com.vitalyiegorov.tart-runner-fleet.canary.plist",
		"com.vitalyiegorov.tart-runner-fleet.shadow.plist",
		"render-launchd.sh",
	} {
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
			t.Errorf("launchd template must contain %q so the daemon can resolve tart on Intel and Apple Silicon hosts", required)
		}
	}
}

func TestVersionedLaunchdModesRenderWithoutAdHocPlistEdits(t *testing.T) {
	const prefix = "../../launchd/com.vitalyiegorov.tart-runner-fleet"
	templates := map[string][]string{
		"observe":   {"--mode=observe", "com.vitalyiegorov.tart-runner-fleet", "<key>ExitTimeOut</key>", "<integer>30</integer>", "<key>AbandonProcessGroup</key>", "<true/>"},
		"shadow":    {"--mode=shadow", "com.vitalyiegorov.tart-runner-fleet.shadow", "fleet-shadow.db", "fleet-shadow.sock", "<key>ExitTimeOut</key>", "<integer>30</integer>", "<key>AbandonProcessGroup</key>", "<true/>"},
		"canary":    {"--mode=canary", "com.vitalyiegorov.tart-runner-fleet.canary", "--canary-scope=__CANARY_SCOPE__", "--canary-profile=__CANARY_PROFILE__", "fleet-canary.db", "fleet-canary.sock", "<key>ExitTimeOut</key>", "<integer>30</integer>", "<key>AbandonProcessGroup</key>", "<true/>"},
		"authority": {"--mode=authority", "com.vitalyiegorov.tart-runner-fleet.authority", "<key>ExitTimeOut</key>", "<integer>30</integer>", "<key>AbandonProcessGroup</key>", "<true/>"},
	}
	for mode, required := range templates {
		required = append(required, "--config=__STATE_DIR__/fleet.json")
		path := prefix + "." + mode + ".plist"
		if mode == "observe" {
			path = prefix + ".plist"
		}
		data, err := os.ReadFile(path) // #nosec G304 -- fixed repository fixture assembled from a closed mode set.
		if err != nil {
			t.Fatalf("read %s launchd template: %v", mode, err)
		}
		for _, token := range required {
			if !strings.Contains(string(data), token) {
				t.Errorf("%s launchd template is missing %q", mode, token)
			}
		}
	}

	rendered := filepath.Join(t.TempDir(), "fleet-canary.plist")
	command := exec.Command("../../launchd/render-launchd.sh", "canary", "/opt/tart runner fleet/v1", "/tmp/fleet state", rendered, "fleet-repo", "small") // #nosec G204 -- fixed test arguments.
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("render launchd canary: %v: %s", err, output)
	}
	data, err := os.ReadFile(rendered) // #nosec G304 -- test-owned path.
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	// Match complete ProgramArguments elements, never bare path fragments. ADR
	// 0019 merged `fleetd` and `fleetctl` into a name that is a strict prefix of
	// both, so `Contains(text, ".../v1/fleet")` is also satisfied by a plist that
	// launches `.../v1/fleetd` and can no longer prove the daemon is invoked.
	for _, required := range []string{
		"<string>/opt/tart runner fleet/v1/fleet</string>",
		"<string>--config=/tmp/fleet state/fleet.json</string>",
		"<string>--mode=canary</string>",
		"<string>--canary-scope=fleet-repo</string>",
		"<string>--canary-profile=small</string>",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("rendered canary is missing %q", required)
		}
	}
	if strings.Contains(text, "__") {
		t.Errorf("rendered canary retains a template placeholder")
	}
	if info, err := os.Stat(rendered); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("rendered launchd permissions = %v, %v", info, err)
	}

	bad := exec.Command("../../launchd/render-launchd.sh", "authority", "/tmp/<unsafe>", "/tmp/state", rendered) // #nosec G204 -- fixed injection regression.
	if err := bad.Run(); err == nil {
		t.Fatal("launchd renderer accepted an XML metacharacter in a path")
	}

	operations, err := os.ReadFile("../../docs/OPERATIONS.md") // #nosec G304 -- fixed repository fixture.
	if err != nil {
		t.Fatal(err)
	}
	operationsText := string(operations)
	for _, required := range []string{
		"render-launchd.sh authority",
		"launchctl bootout gui/\"$(id -u)\"/com.github.linux-burst-manager",
		"launchctl bootstrap gui/\"$(id -u)\" \"$AUTHORITY_PLIST\"",
		"launchctl bootout gui/\"$(id -u)\"/com.vitalyiegorov.tart-runner-fleet.authority",
		"launchctl bootstrap gui/\"$(id -u)\" \"$INCUMBENT_PLIST\"",
	} {
		if !strings.Contains(operationsText, required) {
			t.Errorf("operations handoff is missing exact command %q", required)
		}
	}
}

func TestAuthorityCanaryIsManualDedicatedAndBounded(t *testing.T) {
	workflow, err := os.ReadFile("../../.github/workflows/fleet-canary.yml") // #nosec G304 -- fixed repository fixture.
	if err != nil {
		t.Fatal(err)
	}
	text := string(workflow)
	for _, required := range []string{
		"workflow_dispatch:",
		"cancel-in-progress: false",
		"[self-hosted, linux-tiered, linux-small, tart-fleet-canary]",
		"timeout-minutes: 5",
		`test "$RUNNER_ENVIRONMENT" = self-hosted`,
	} {
		if !strings.Contains(text, required) {
			t.Errorf("authority canary is missing %q", required)
		}
	}
	for _, forbidden := range []string{"pull_request:", "push:", "schedule:"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("authority canary must remain manual, found %q", forbidden)
		}
	}
}
