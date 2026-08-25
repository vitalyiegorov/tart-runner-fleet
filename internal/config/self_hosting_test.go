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

// TestExampleConfigIsPortableOnlyOnceItsHostBudgetIsRemoved pins the reason the
// observe smoke test cannot use `config/fleet.example.json` verbatim.
//
// `hostBudget` is a claim about ONE machine — the example states the live Mac
// mini's ten cores and 23552 MiB — and `app.budgetExceedsHost` fails the host
// observation closed when the machine cannot honour it (issue #136). An
// unavailable host observation blocks every tick's plan, so the daemon never
// records a successful tick and never becomes ready. That is exactly right for a
// node whose operator claims capacity it does not have, and exactly wrong for a
// two-core CI guest booting the shipped example: it cost issue #138 a red gate.
//
// The two halves are asserted together so they cannot drift. If the example
// stops declaring a budget the smoke script's removal becomes a silent no-op and
// its comment becomes a lie; if the script stops removing it, CI breaks again on
// any machine smaller than node A.
func TestExampleConfigIsPortableOnlyOnceItsHostBudgetIsRemoved(t *testing.T) {
	file, err := os.Open("../../config/fleet.example.json") // #nosec G304 -- fixed repository fixture.
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	cfg, err := Decode(file)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HostBudget.CPU <= 0 || cfg.HostBudget.MemoryMiB <= 0 {
		t.Fatalf("the example declares node A's envelope explicitly; hostBudget = %+v", cfg.HostBudget)
	}

	smoke, err := os.ReadFile("../../scripts/observe-smoke.sh") // #nosec G304 -- fixed repository fixture.
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"config/fleet.example.json",
		`config.pop("hostBudget", None)`,
	} {
		if !strings.Contains(string(smoke), required) {
			t.Errorf("the observe smoke test must derive its configuration from the example: missing %q", required)
		}
	}
	// A timeout that prints nothing is a gate nobody can act on. The smoke test
	// must surface the daemon's own account of why it never became ready.
	for _, required := range []string{"status --output json", "doctor --output json", "readyz"} {
		if !strings.Contains(string(smoke), required) {
			t.Errorf("the observe smoke test must be self-diagnosing on timeout: missing %q", required)
		}
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
		"tart-runner-fleet-bootstrap-linux-amd64.cdx.json",
		"tart-runner-fleet-bootstrap-darwin-arm64",
		"tart-runner-fleet-bootstrap-linux-arm64",
		"tart-runner-fleet-bootstrap-linux-amd64",
		"com.vitalyiegorov.tart-runner-fleet.authority.plist",
		"com.vitalyiegorov.tart-runner-fleet.canary.plist",
		"com.vitalyiegorov.tart-runner-fleet.shadow.plist",
		"render-launchd.sh",
	} {
		if !strings.Contains(string(buildScript), required) {
			t.Errorf("release artifacts must include the secret-safe guest bootstrap helper %q", required)
		}
	}
	// ADR 0034 gives the fleet two node types, so a release that cannot be
	// installed on the second one is not a release. The Linux archive is built
	// from the same double-build compare and listed in the same SHA256SUMS as
	// the darwin one, and it carries the systemd units and the renderer that
	// boot it.
	for _, required := range []string{
		"GOOS=linux GOARCH=amd64",
		"fleet-linux-amd64",
		"fleet-linux-amd64.cdx.json",
		"BUILDINFO-linux-amd64.txt",
		"archive linux-amd64",
		"tart-runner-fleet-$version-$goos_arch.tar.gz",
		"tart-runner-fleet-authority.service",
		"tart-runner-fleet-updater.service",
		"tart-runner-fleet-updater.timer",
		"tart-runner-fleet-updater-handoff.service",
		"render-systemd.sh",
	} {
		if !strings.Contains(string(buildScript), required) {
			t.Errorf("release must publish the linux/amd64 node's generation: missing %q", required)
		}
	}
	// Issue #272: the geekom image build (docs/LINUX_BASE_IMAGE.md) installs the
	// guest bootstrap helper from a released, checksummed
	// `tart-runner-fleet-bootstrap-linux-amd64` asset, so the matrix that builds
	// the other two bootstrap variants must build this one too, from the same
	// double-build compare and in both archives' shared bootstrap set.
	for _, required := range []string{
		"tart-runner-fleet-bootstrap-linux-amd64",
		"tart-runner-fleet-bootstrap-linux-amd64.cdx.json",
	} {
		if !strings.Contains(string(buildScript), required) {
			t.Errorf("release must build and archive the geekom guest bootstrap helper: missing %q", required)
		}
	}

	mainRelease, err := os.ReadFile("../../.github/workflows/main-release.yml") // #nosec G304 -- fixed repository fixture.
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"tart-runner-fleet-bootstrap-darwin-arm64",
		"tart-runner-fleet-bootstrap-linux-arm64",
		"tart-runner-fleet-bootstrap-linux-amd64",
		"tart-runner-fleet-bootstrap-darwin-arm64.cdx.json",
		"tart-runner-fleet-bootstrap-linux-arm64.cdx.json",
		"tart-runner-fleet-bootstrap-linux-amd64.cdx.json",
		"com.vitalyiegorov.tart-runner-fleet.authority.plist",
		"com.vitalyiegorov.tart-runner-fleet.canary.plist",
		"com.vitalyiegorov.tart-runner-fleet.shadow.plist",
		"render-launchd.sh",
	} {
		if !strings.Contains(string(mainRelease), required) {
			t.Errorf("main release verification must require bootstrap asset %q", required)
		}
	}
	// The publish step must verify the second node's archive with the same
	// checksum manifest, member listing, and file-presence checks as the first,
	// and upload it: a release missing one node type is not publishable.
	for _, required := range []string{
		`LINUX_ARCHIVE="tart-runner-fleet-${VERSION}-linux-amd64.tar.gz"`,
		"fleet-linux-amd64",
		"BUILDINFO-linux-amd64.txt",
		"EXPECTED_LINUX_CONTENTS",
		`ACTUAL_LINUX_CONTENTS="$(tar -tzf "dist/$LINUX_ARCHIVE" | LC_ALL=C sort)"`,
		`test "$ACTUAL_LINUX_CONTENTS" = "$EXPECTED_LINUX_CONTENTS"`,
		`"dist/$LINUX_ARCHIVE"`,
		"dist/render-systemd.sh",
		"dist/tart-runner-fleet-authority.service",
		"dist/tart-runner-fleet-updater.timer",
	} {
		if !strings.Contains(string(mainRelease), required) {
			t.Errorf("main release must verify and publish the linux/amd64 generation: missing %q", required)
		}
	}
	if !strings.Contains(string(ci), "./scripts/observe-smoke.sh") {
		t.Error("CI must prove the daemon reaches the observe steady state on the node it builds on")
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

// A `systemd --user` manager holds no privileges of its own, so each directive
// below defeats rootless podman one of two ways: NoNewPrivileges, and
// RestrictSUIDSGID which implies it per systemd.exec(5), leave the execve of
// `/usr/bin/newuidmap` without its `cap_setuid=ep` file capability, and the
// three namespacing ones run the unit inside a user namespace mapping this uid
// alone, where there is no subuid left to map. A controller unit carrying any
// of them is a Linux node that can never start a container guest, so none of
// them may come back to one (#277).
var rootlessPodmanHostileSandbox = []string{
	"NoNewPrivileges=", "PrivateTmp=", "ProtectSystem=",
	"ProtectKernelTunables=", "RestrictSUIDSGID=",
}

func assertNoRootlessPodmanHostileSandbox(t *testing.T, name, body string) {
	t.Helper()
	for _, directive := range rootlessPodmanHostileSandbox {
		if strings.Contains(body, directive) {
			t.Errorf("%s sets %s, which starves the controller's rootless podman of CAP_SETUID", name, directive)
		}
	}
}

// TestVersionedSystemdModesRenderWithoutAdHocUnitEdits is the Linux twin of the
// launchd assertion above. ADR 0034's second node boots from `systemd --user`,
// and issue #138 requires its three services — the controller, the five-minute
// updater with its timer, and the updater handoff — to come from the release
// being installed rather than from a hand-written file.
func TestVersionedSystemdModesRenderWithoutAdHocUnitEdits(t *testing.T) {
	templates := map[string][]string{
		"tart-runner-fleet.service":                 {`"--mode=observe"`, "/fleetd.sock", "Restart=on-failure", "KillMode=process", "TimeoutStopSec=30", "UMask=0077"},
		"tart-runner-fleet-shadow.service":          {`"--mode=shadow"`, "fleet-shadow.db", "fleet-shadow.sock", "Restart=on-failure", "KillMode=process"},
		"tart-runner-fleet-canary.service":          {`"--mode=canary"`, `"--canary-scope=__CANARY_SCOPE__"`, `"--canary-profile=__CANARY_PROFILE__"`, "fleet-canary.db", "fleet-canary.sock"},
		"tart-runner-fleet-authority.service":       {`"--mode=authority"`, "Restart=on-failure", "KillMode=process", "TimeoutStopSec=30"},
		"tart-runner-fleet-updater.service":         {"update apply-latest", "automatic-release-update", "Type=oneshot"},
		"tart-runner-fleet-updater.timer":           {"OnUnitActiveSec=300", "OnActiveSec=0", "Unit=tart-runner-fleet-updater.service"},
		"tart-runner-fleet-updater-handoff.service": {"update finish-updater-handoff", "automatic-updater-handoff", "Restart=on-failure", "RestartSec=10"},
	}
	for name, required := range templates {
		data, err := os.ReadFile(filepath.Join("../../systemd", name)) // #nosec G304 -- fixed repository fixture assembled from a closed unit set.
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		body := string(data)
		required = append(required, "[Unit]")
		if strings.HasSuffix(name, ".service") {
			// The host probe reads /proc, so a unit that hid it would turn every
			// admission decision on the node into an unavailable observation.
			required = append(required, `"__RELEASE_DIR__/fleet"`)
			if strings.Contains(body, "ProcSubset=") || strings.Contains(body, "ProtectProc=") {
				t.Errorf("%s hides /proc from the host probe", name)
			}
			if strings.Contains(name, "updater") {
				// The release transaction forks no guest runtime, so it keeps the
				// sandbox the controller cannot afford.
				required = append(required, "NoNewPrivileges=yes")
			} else {
				assertNoRootlessPodmanHostileSandbox(t, name, body)
			}
		}
		for _, token := range required {
			if !strings.Contains(body, token) {
				t.Errorf("%s systemd template is missing %q", name, token)
			}
		}
	}

	rendered := t.TempDir()
	command := exec.Command("../../systemd/render-systemd.sh", "canary", // #nosec G204 -- fixed test arguments.
		"/opt/tart runner fleet/releases/v1", "/tmp/fleet state", rendered, "fleet-repo", "small")
	command.Env = append(os.Environ(), "FLEET_UNITS_DIR=/home/fleet/.config/systemd/user")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("render systemd canary: %v: %s", err, output)
	}
	for _, name := range []string{"tart-runner-fleet-canary.service", "tart-runner-fleet-updater.service",
		"tart-runner-fleet-updater.timer", "tart-runner-fleet-updater-handoff.service"} {
		info, err := os.Stat(filepath.Join(rendered, name))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("rendered %s permissions = %v, %v", name, info, err)
		}
	}
	// Every argument is quoted because a release directory may contain spaces
	// and systemd splits an unquoted ExecStart on whitespace. A bare path
	// fragment cannot prove that, so match whole quoted arguments.
	unit, err := os.ReadFile(filepath.Join(rendered, "tart-runner-fleet-canary.service")) // #nosec G304 -- test-owned path.
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		`"/opt/tart runner fleet/releases/v1/fleet"`,
		`"--config=/tmp/fleet state/fleet.json"`,
		`"--mode=canary"`,
		`"--canary-scope=fleet-repo"`,
		`"--canary-profile=small"`,
	} {
		if !strings.Contains(string(unit), required) {
			t.Errorf("rendered canary unit is missing %q", required)
		}
	}
	if strings.Contains(string(unit), "__") {
		t.Error("rendered canary unit retains a template placeholder")
	}
	// The rendered file is what the manager executes, so the sandbox rule is
	// asserted on it and not only on the template it came from.
	assertNoRootlessPodmanHostileSandbox(t, "rendered canary unit", string(unit))

	// The immutable root is derived from the release path, so the updater is
	// pointed at the tree the release actually lives in.
	updater, err := os.ReadFile(filepath.Join(rendered, "tart-runner-fleet-updater.service")) // #nosec G304 -- test-owned path.
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		`"--root" "/opt/tart runner fleet"`,
		`"--launch-agents-dir" "/home/fleet/.config/systemd/user"`,
		`"--endpoint" "unix:///tmp/fleet state/fleet-canary.sock"`,
		`"--mode" "canary"`,
	} {
		if !strings.Contains(string(updater), required) {
			t.Errorf("rendered updater unit is missing %q", required)
		}
	}

	refused := [][]string{
		// A systemd specifier and a variable reference both expand inside
		// ExecStart, so a path carrying either would escape the quoting.
		{"authority", "/opt/%h/releases/v1", "/tmp/state", rendered},
		{"authority", "/opt/$HOME/releases/v1", "/tmp/state", rendered},
		{"authority", `/opt/"quoted"/releases/v1`, "/tmp/state", rendered},
		// A release directory outside <root>/releases/<version> would silently
		// point the updater at the wrong tree.
		{"authority", "/opt/fleet/v1", "/tmp/state", rendered},
		{"observe", "/opt/fleet/releases/v1", "/tmp/state", rendered, "extra", "arguments"},
		{"nonsense", "/opt/fleet/releases/v1", "/tmp/state", rendered},
	}
	for _, args := range refused {
		if err := exec.Command("../../systemd/render-systemd.sh", args...).Run(); err == nil { // #nosec G204 -- fixed injection regressions.
			t.Errorf("systemd renderer accepted %v", args)
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
