package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/autoupdate"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/provision"
)

type fakeClient struct {
	status                        adminapi.StatusEnvelope
	live                          adminapi.Check
	ready                         adminapi.Check
	metrics                       string
	err                           error
	liveErr, readyErr, metricsErr error
	discharge                     adminapi.DischargeResult
	dischargeErr                  error
	discharged                    *adminapi.DischargeRequest
}

func (f fakeClient) Status(context.Context) (adminapi.StatusEnvelope, error) {
	return f.status, f.err
}
func (f fakeClient) Probe(_ context.Context, ready bool) (adminapi.Check, error) {
	if f.err != nil {
		return adminapi.Check{}, f.err
	}
	if ready {
		return f.ready, f.readyErr
	}
	return f.live, f.liveErr
}
func (f fakeClient) Discharge(_ context.Context, request adminapi.DischargeRequest) (adminapi.DischargeResult, error) {
	if f.discharged != nil {
		*f.discharged = request
	}
	return f.discharge, f.dischargeErr
}
func (f fakeClient) Metrics(context.Context) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.metrics, f.metricsErr
}

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) { return 0, errors.New("broken output") }

type fakeUpdateCommand struct {
	calls       []string
	launchPrint string
}

func (c *fakeUpdateCommand) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := name + " " + strings.Join(args, " ")
	c.calls = append(c.calls, call)
	if name == "gh" {
		return []byte(`{"tag_name":"v2","draft":false,"prerelease":false}`), nil
	}
	if strings.Contains(call, "status --require-ready") {
		return []byte(`{"data":{"controllerVersion":"v2","controllerMode":"authority","ready":{"ok":true}}}`), nil
	}
	if strings.Contains(call, "launchctl print") {
		return []byte(c.launchPrint), nil
	}
	return nil, nil
}

func makeUpdateRelease(t *testing.T, root string) string {
	t.Helper()
	dir := filepath.Join(root, "releases", "v2")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	template := []byte(`<plist><dict><string>__RELEASE_DIR__/fleet</string><string>--mode=authority</string><string>__STATE_DIR__/fleet.json</string></dict></plist>`)
	files := map[string][]byte{"RELEASE_VERSION": []byte("v2\n"), "fleet": []byte("fleet"),
		"com.vitalyiegorov.tart-runner-fleet.authority.plist": template}
	var sums strings.Builder
	for _, name := range []string{"RELEASE_VERSION", "fleet", "com.vitalyiegorov.tart-runner-fleet.authority.plist"} {
		body := files[name]
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o700); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(body)
		sums.WriteString(hex.EncodeToString(digest[:]) + "  " + name + "\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(sums.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestGuardedAutomaticUpdateAdoptionAndIdempotentLatest(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	agents := filepath.Join(root, "LaunchAgents")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(agents, 0o700); err != nil {
		t.Fatal(err)
	}
	release := makeUpdateRelease(t, root)
	configPath := filepath.Join(state, "fleet.json")
	if err := os.WriteFile(configPath, []byte(`{"valid":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	canonical := filepath.Join(agents, autoupdate.CanonicalPlist)
	if err := os.WriteFile(canonical, []byte(`<string>`+release+`/fleet</string><string>--mode=authority</string>`), 0o600); err != nil {
		t.Fatal(err)
	}
	command := &fakeUpdateCommand{}
	deps := dependencies{command: command}
	common := []string{"--root", root, "--state-dir", state, "--launch-agents-dir", agents, "--config", configPath,
		"--endpoint", "unix:///state/fleetd.sock", "--domain", "gui/501", "--repo", "owner/repo", "--mode", "authority"}
	args := append([]string{"update", "adopt"}, common...)
	args = append(args, "--release-dir", release, "--confirm", "adopt-current-generation")
	var stdout, stderr bytes.Buffer
	if code := executeWith(context.Background(), args, &stdout, &stderr, deps); code != exitSuccess {
		t.Fatalf("adopt code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	command.launchPrint = "program = " + release + "/fleet"
	args = append([]string{"update", "finish-updater-handoff"}, common...)
	args = append(args, "--release-dir", release, "--confirm", "automatic-updater-handoff")
	stdout.Reset()
	stderr.Reset()
	if code := executeWith(context.Background(), args, &stdout, &stderr, deps); code != exitSuccess || !strings.Contains(stdout.String(), "v2") {
		t.Fatalf("handoff code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	args = append([]string{"update", "apply-latest"}, common...)
	args = append(args, "--confirm", "automatic-release-update")
	stdout.Reset()
	stderr.Reset()
	if code := executeWith(context.Background(), args, &stdout, &stderr, deps); code != exitSuccess || !strings.Contains(stdout.String(), "v2") {
		t.Fatalf("latest code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(agents, autoupdate.UpdaterPlist)); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := executeWith(context.Background(), []string{"update", "apply-latest"}, &stdout, &stderr, deps); code != exitUnsafe {
		t.Fatalf("unguarded update code=%d", code)
	}
}

func TestUpdateCommandFailureModes(t *testing.T) {
	if body, err := (execCommand{}).Run(context.Background(), "/usr/bin/printf", "ok"); err != nil || string(body) != "ok" {
		t.Fatalf("exec body=%q err=%v", body, err)
	}
	for _, args := range [][]string{{"update"}, {"update", "other"}, {"update", "adopt", "--bad"}} {
		var stdout, stderr bytes.Buffer
		if code := executeWith(context.Background(), args, &stdout, &stderr, dependencies{}); code != exitUsage {
			t.Fatalf("args=%v code=%d", args, code)
		}
	}

	root := t.TempDir()
	state := filepath.Join(root, "state")
	agents := filepath.Join(root, "agents")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(agents, 0o700); err != nil {
		t.Fatal(err)
	}
	command := &fakeUpdateCommand{}
	deps := dependencies{command: command}
	base := []string{"--root", root, "--state-dir", state, "--launch-agents-dir", agents, "--repo", "owner/repo",
		"--domain", "gui/501", "--config", filepath.Join(state, "fleet.json"), "--endpoint", "unix:///state/fleetd.sock"}
	tests := []struct {
		name string
		args []string
		code int
	}{
		{name: "invalid host", args: []string{"update", "apply-latest", "--repo", "bad", "--confirm", "automatic-release-update"}, code: exitFailure},
		{name: "adopt missing release", args: append(append([]string{"update", "adopt"}, base...), "--confirm", "adopt-current-generation"), code: exitUsage},
		{name: "adopt failure", args: append(append([]string{"update", "adopt"}, base...), "--release-dir", filepath.Join(root, "releases", "missing"), "--confirm", "adopt-current-generation"), code: exitFailure},
		{name: "handoff missing release", args: append(append([]string{"update", "finish-updater-handoff"}, base...), "--confirm", "automatic-updater-handoff"), code: exitUsage},
		{name: "handoff wrong confirmation", args: append(append([]string{"update", "finish-updater-handoff"}, base...), "--release-dir", filepath.Join(root, "releases", "v2"), "--confirm", "automatic-release-update"), code: exitUnsafe},
		{name: "latest rejects release dir", args: append(append([]string{"update", "apply-latest"}, base...), "--release-dir", "/x", "--confirm", "automatic-release-update"), code: exitUsage},
		{name: "latest source failure", args: append(append([]string{"update", "apply-latest"}, base...), "--confirm", "automatic-release-update"), code: exitFailure},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := executeWith(context.Background(), test.args, &stdout, &stderr, deps); code != test.code {
				t.Fatalf("code=%d want=%d stdout=%q stderr=%q", code, test.code, stdout.String(), stderr.String())
			}
		})
	}

	release := makeUpdateRelease(t, root)
	installed := autoupdate.Generation{Version: "v1", Mode: "authority", ReleaseDir: filepath.Join(root, "releases", "v1"),
		ConfigPath: filepath.Join(state, "fleet.json"), Endpoint: "unix:///state/fleetd.sock"}
	body, _ := json.Marshal(installed)
	if err := os.WriteFile(filepath.Join(state, autoupdate.InstalledGenerationFile), body, 0o600); err != nil {
		t.Fatal(err)
	}
	handoffArgs := append(append([]string{"update", "finish-updater-handoff"}, base...), "--release-dir", release, "--confirm", "automatic-updater-handoff")
	var stdout, stderr bytes.Buffer
	if code := executeWith(context.Background(), handoffArgs, &stdout, &stderr, deps); code != exitFailure || !strings.Contains(stderr.String(), "finish automatic updater handoff") {
		t.Fatalf("handoff failure code=%d stderr=%q", code, stderr.String())
	}
	args := append(append([]string{"update", "apply-latest"}, base...), "--confirm", "automatic-release-update")
	stdout.Reset()
	stderr.Reset()
	if code := executeWith(context.Background(), args, &stdout, &stderr, deps); code != exitFailure || !strings.Contains(stderr.String(), "apply production release") {
		t.Fatalf("apply code=%d stderr=%q", code, stderr.String())
	}
}

func TestProductionUpdateReadinessBudgetCoversScaleSetInitialization(t *testing.T) {
	if budget := time.Duration(updateReadyAttempts) * updateReadyDelay; budget < 5*time.Minute {
		t.Fatalf("production update readiness budget=%s want at least 5m", budget)
	}
}

func healthyStatus() adminapi.StatusEnvelope {
	now := time.Date(2026, 7, 12, 20, 0, 0, 0, time.UTC)
	return adminapi.StatusEnvelope{APIVersion: adminapi.APIVersion, Kind: "Status", GeneratedAt: now, Revision: 9,
		Warnings: []adminapi.Warning{}, Data: adminapi.Status{ControllerVersion: "v1", ControllerMode: "shadow", HostMode: "linux",
			LastSuccessfulTick: now.Add(-2 * time.Second), Live: adminapi.Check{OK: true, Reasons: []string{}},
			Ready:        adminapi.Check{OK: true, Reasons: []string{}},
			HostPressure: adminapi.HostPressure{AvailableMemoryMiB: 8192, FreeDiskGiB: 200, CPUIdlePercent: 60, LoadAverage: 3, AdmissionAllowed: true, AdmissionReason: "capacity available"},
			Queues:       []adminapi.Queue{{Profile: "linux-small", Jobs: 3, OldestAgeSeconds: 61}},
			Instances:    []adminapi.Instance{{Profile: "linux-small", Count: 2, CPU: 2, MemoryMiB: 4096}},
			Observations: []adminapi.Observation{{Name: "scheduler", Freshness: "fresh", AgeSeconds: 2}},
			Operations:   adminapi.OperationSummary{Retrying: 1},
		}}
}

func TestOperatorCommandsHumanAndJSON(t *testing.T) {
	if admissionState(false) != "deferred" {
		t.Fatal("blocked admission rendered as allowed")
	}
	status := healthyStatus()
	deps := dependencies{newClient: func(string, time.Duration) (apiClient, error) {
		return fakeClient{status: status, live: status.Data.Live, ready: status.Data.Ready, metrics: "fleet_mode 1\n"}, nil
	}}
	tests := []struct {
		name     string
		args     []string
		wantCode int
		contains []string
	}{
		{name: "status", args: []string{"status"}, contains: []string{"READY", "shadow", "HOST PRESSURE", "disk 200 GiB", "memory 8192 MiB", "QUEUES", "linux-small", "1m1s"}},
		{name: "status json", args: []string{"status", "--output", "json"}, contains: []string{`"apiVersion": "fleet.v1"`, `"revision": 9`}},
		{name: "queues", args: []string{"queues"}, contains: []string{"PROFILE", "linux-small", "3"}},
		{name: "queues json", args: []string{"queues", "-o", "json"}, contains: []string{`"profile": "linux-small"`}},
		{name: "instances", args: []string{"instances"}, contains: []string{"MEMORY", "4096"}},
		{name: "instances json", args: []string{"instances", "--output", "json"}, contains: []string{`"memoryMiB": 4096`}},
		{name: "operations", args: []string{"operations"}, contains: []string{"retrying", "1", "dead"}},
		{name: "operations json", args: []string{"operations", "--output", "json"}, contains: []string{`"retrying": 1`}},
		{name: "observations", args: []string{"observations"}, contains: []string{"scheduler", "fresh"}},
		{name: "observations json", args: []string{"observations", "--output", "json"}, contains: []string{`"freshness": "fresh"`}},
		{name: "health", args: []string{"health"}, contains: []string{"live", "PASS", "ready"}},
		{name: "health json", args: []string{"health", "--output", "json"}, contains: []string{`"live"`, `"ok": true`}},
		{name: "doctor", args: []string{"doctor"}, contains: []string{"PASS", "admin API", "metrics", "RESULT"}},
		{name: "doctor json", args: []string{"doctor", "--output", "json"}, contains: []string{`"checks"`, `"admin API"`}},
		{name: "metrics", args: []string{"metrics"}, contains: []string{"fleet_mode 1"}},
		{name: "api version", args: []string{"api-version"}, contains: []string{adminapi.APIVersion}},
		{name: "version json", args: []string{"version", "--output", "json"}, contains: []string{`"version": "dev"`}},
		{name: "help", args: []string{"help"}, contains: []string{"fleet status", "READ-ONLY"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := executeWith(context.Background(), tt.args, &stdout, &stderr, deps); got != tt.wantCode {
				t.Fatalf("code=%d stdout=%q stderr=%q", got, stdout.String(), stderr.String())
			}
			for _, want := range tt.contains {
				if !strings.Contains(stdout.String(), want) {
					t.Fatalf("missing %q in %q", want, stdout.String())
				}
			}
		})
	}
}

func TestDegradedUnavailableAndUsageExitCodes(t *testing.T) {
	status := healthyStatus()
	status.Data.Ready = adminapi.Check{Reasons: []string{"successful_tick_expired"}}
	degraded := dependencies{newClient: func(string, time.Duration) (apiClient, error) {
		return fakeClient{status: status, live: adminapi.Check{OK: true}, ready: status.Data.Ready}, nil
	}}
	queueStatus := healthyStatus()
	queueStatus.Data.QueueSLO = &adminapi.Check{OK: false, Reasons: []string{"queue_slo_breached"}}
	queueDegraded := dependencies{newClient: func(string, time.Duration) (apiClient, error) {
		return fakeClient{status: queueStatus, live: queueStatus.Data.Live, ready: queueStatus.Data.Ready}, nil
	}}
	unavailable := dependencies{newClient: func(string, time.Duration) (apiClient, error) {
		return fakeClient{}, errors.New("offline")
	}}
	remoteUnavailable := dependencies{newClient: func(string, time.Duration) (apiClient, error) {
		return fakeClient{err: errors.New("offline")}, nil
	}}
	tests := []struct {
		name string
		args []string
		deps dependencies
		code int
		text string
	}{
		{name: "require ready", args: []string{"status", "--require-ready"}, deps: degraded, code: exitDegraded, text: "NOT READY"},
		{name: "queue degraded", args: []string{"status"}, deps: queueDegraded, code: exitSuccess, text: "DEGRADED"},
		{name: "health degraded", args: []string{"health"}, deps: degraded, code: exitDegraded, text: "successful_tick_expired"},
		{name: "doctor degraded", args: []string{"doctor"}, deps: degraded, code: exitDegraded, text: "FAIL"},
		{name: "offline", args: []string{"status"}, deps: unavailable, code: exitUnavailable, text: "offline"},
		{name: "remote offline", args: []string{"metrics"}, deps: remoteUnavailable, code: exitUnavailable, text: "fleet unavailable"},
		{name: "unknown", args: []string{"destroy-everything"}, deps: degraded, code: exitUsage, text: "unknown command"},
		{name: "bad output", args: []string{"status", "--output", "yaml"}, deps: degraded, code: exitUsage, text: "output"},
		{name: "bad timeout", args: []string{"status", "--timeout", "nope"}, deps: degraded, code: exitUsage, text: "invalid"},
		{name: "timeout range", args: []string{"status", "--timeout", "31s"}, deps: degraded, code: exitUsage, text: "invalid timeout"},
		{name: "position", args: []string{"status", "extra"}, deps: degraded, code: exitUsage, text: "unexpected positional"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := executeWith(context.Background(), tt.args, &stdout, &stderr, tt.deps); got != tt.code ||
				!strings.Contains(stdout.String()+stderr.String(), tt.text) {
				t.Fatalf("code=%d stdout=%q stderr=%q", got, stdout.String(), stderr.String())
			}
		})
	}
}

func TestIssue11ConnectionFlagsWorkBeforeCommand(t *testing.T) {
	const endpoint = "unix:///tmp/fleetctl-issue-11.sock"
	var gotEndpoint string
	deps := dependencies{newClient: func(got string, _ time.Duration) (apiClient, error) {
		gotEndpoint = got
		status := healthyStatus()
		return fakeClient{status: status}, nil
	}}
	var stdout, stderr bytes.Buffer
	if code := executeWith(context.Background(), []string{"--endpoint", endpoint, "status", "--output", "json"}, &stdout, &stderr, deps); code != exitSuccess {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if gotEndpoint != endpoint {
		t.Fatalf("endpoint=%q want=%q", gotEndpoint, endpoint)
	}
}

func TestSplitLeadingConnectionArgs(t *testing.T) {
	tests := []struct {
		name           string
		args           []string
		wantConnection []string
		wantRemaining  []string
	}{
		{name: "none", args: []string{"status"}, wantRemaining: []string{"status"}},
		{name: "separate values", args: []string{"--endpoint", "unix:///tmp/fleet.sock", "--timeout", "7s", "doctor"}, wantConnection: []string{"--endpoint", "unix:///tmp/fleet.sock", "--timeout", "7s"}, wantRemaining: []string{"doctor"}},
		{name: "equals values", args: []string{"--endpoint=unix:///tmp/fleet.sock", "--timeout=7s", "health"}, wantConnection: []string{"--endpoint=unix:///tmp/fleet.sock", "--timeout=7s"}, wantRemaining: []string{"health"}},
		{name: "missing value", args: []string{"--endpoint"}, wantConnection: []string{"--endpoint"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotConnection, gotRemaining := splitLeadingConnectionArgs(tt.args)
			if strings.Join(gotConnection, "|") != strings.Join(tt.wantConnection, "|") || strings.Join(gotRemaining, "|") != strings.Join(tt.wantRemaining, "|") {
				t.Fatalf("connection=%v remaining=%v", gotConnection, gotRemaining)
			}
		})
	}
}

func TestConfigValidationAndLegacyAlias(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "valid.json")
	invalid := filepath.Join(dir, "invalid.json")
	raw := `{"baseVm":"linux","vmPrefix":"gha","pollSeconds":20,"maxLinuxWhenMacosIdle":1,"maxLinuxCpu":2,"maxLinuxMemoryMb":4096,"linuxReservationAgeSeconds":300,"minFreeDiskGb":1,"linuxProfiles":[{"id":"small","label":"linux-small","cpu":1,"memoryMb":2048}],"macosBurst":{"enabled":false},"targets":[{"type":"repo","slug":"owner/repo","maxActive":1}]}`
	if err := os.WriteFile(valid, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(invalid, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	deps := defaultDependencies()
	for _, args := range [][]string{{"config", "validate", valid}, {"validate-config", valid}, {"config", "validate", "--output", "json", valid}} {
		var stdout, stderr bytes.Buffer
		if code := executeWith(context.Background(), args, &stdout, &stderr, deps); code != 0 || !strings.Contains(stdout.String(), "valid") {
			t.Fatalf("args=%v code=%d stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
		}
	}
	for _, path := range []string{invalid, filepath.Join(dir, "missing")} {
		var stdout, stderr bytes.Buffer
		if code := executeWith(context.Background(), []string{"config", "validate", path}, &stdout, &stderr, deps); code != exitFailure {
			t.Fatalf("path=%s code=%d stderr=%q", path, code, stderr.String())
		}
	}
	// A config that decodes and passes Config.Validate but references a
	// nonexistent scale set profile must still fail validation, because the daemon
	// would crash-loop building bindings at startup (production incident).
	runtimeGap := filepath.Join(dir, "runtime-gap.json")
	if err := os.WriteFile(runtimeGap, []byte(strings.Replace(raw, `"targets":`,
		`"github":{"scaleSets":[{"profile":"xl","name":"fleet-repo-xl","id":1,"maxCapacity":1}]},"targets":`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	var gapStdout, gapStderr bytes.Buffer
	if code := executeWith(context.Background(), []string{"config", "validate", runtimeGap}, &gapStdout, &gapStderr, deps); code != exitFailure ||
		!strings.Contains(gapStderr.String(), "unknown profile") {
		t.Fatalf("runtime gap not caught: code=%d stdout=%q stderr=%q", code, gapStdout.String(), gapStderr.String())
	}
	for _, mode := range []string{"shadow", "canary", "authority"} {
		var modeStdout, modeStderr bytes.Buffer
		if code := executeWith(context.Background(), []string{"config", "validate", "--mode", mode, valid}, &modeStdout, &modeStderr, deps); code != exitFailure || !strings.Contains(modeStderr.String(), "disk floor") {
			t.Fatalf("mode=%s code=%d stdout=%q stderr=%q", mode, code, modeStdout.String(), modeStderr.String())
		}
	}
	for _, args := range [][]string{{"config"}, {"config", "explain"}, {"config", "validate"}, {"config", "validate", "--output", "yaml", valid}, {"config", "validate", "--mode", "other", valid}} {
		var stdout, stderr bytes.Buffer
		if code := executeWith(context.Background(), args, &stdout, &stderr, deps); code != exitUsage {
			t.Fatalf("args=%v code=%d", args, code)
		}
	}
}

func TestHelperBranchesAndDefaultFactory(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := execute([]string{"version", "extra"}, &stdout, &stderr); code != exitUsage {
		t.Fatalf("version args code=%d", code)
	}
	if code := executeWith(context.Background(), nil, &stdout, &stderr, defaultDependencies()); code != exitUsage {
		t.Fatalf("empty args code=%d", code)
	}
	stdout.Reset()
	stderr.Reset()
	if code := executeWith(context.Background(), []string{"version", "--output", "yaml"}, &stdout, &stderr, defaultDependencies()); code != exitUsage {
		t.Fatalf("version output code=%d", code)
	}
	if code := executeWith(context.Background(), []string{"api-version", "extra"}, &stdout, &stderr, defaultDependencies()); code != exitUsage {
		t.Fatalf("api version args code=%d", code)
	}
	if client, err := defaultDependencies().newClient(adminapi.DefaultEndpoint(), time.Second); err != nil || client == nil {
		t.Fatalf("default client=%v err=%v", client, err)
	}
	if got := joinReasons(adminapi.Check{}); got != "unspecified" {
		t.Fatalf("reasons=%q", got)
	}
	if got := formatAge(0); got != "0s" {
		t.Fatalf("age=%q", got)
	}
	var canceled bytes.Buffer
	if code := remoteError(&canceled, context.Canceled); code != exitUnavailable || !strings.Contains(canceled.String(), "canceled") {
		t.Fatalf("code=%d error=%q", code, canceled.String())
	}
}

func TestRemoteSecondaryErrorsAndBrokenOutput(t *testing.T) {
	status := healthyStatus()
	tests := []struct {
		name   string
		args   []string
		client fakeClient
	}{
		{name: "status", args: []string{"status"}, client: fakeClient{err: errors.New("status down")}},
		{name: "live probe", args: []string{"health"}, client: fakeClient{liveErr: errors.New("live down")}},
		{name: "ready probe", args: []string{"health"}, client: fakeClient{live: adminapi.Check{OK: true}, readyErr: errors.New("ready down")}},
		{name: "doctor status", args: []string{"doctor"}, client: fakeClient{err: errors.New("status down")}},
		{name: "doctor metrics", args: []string{"doctor"}, client: fakeClient{status: status, metricsErr: errors.New("metrics down")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps := dependencies{newClient: func(string, time.Duration) (apiClient, error) { return tt.client, nil }}
			var stdout, stderr bytes.Buffer
			if code := executeWith(context.Background(), tt.args, &stdout, &stderr, deps); code != exitUnavailable {
				t.Fatalf("code=%d stderr=%q", code, stderr.String())
			}
		})
	}
	deps := dependencies{newClient: func(string, time.Duration) (apiClient, error) { return fakeClient{status: status}, nil }}
	var stderr bytes.Buffer
	if code := executeWith(context.Background(), []string{"status", "--output", "json"}, errorWriter{}, &stderr, deps); code != exitUnavailable {
		t.Fatalf("broken output code=%d stderr=%q", code, stderr.String())
	}
}

// TestExecuteReportsInjectedVersion pins that the operator surface reports the
// build identity it is handed rather than the development default, which is
// what lets one executable guarantee the daemon and the CLI agree.
func TestExecuteReportsInjectedVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Execute(context.Background(), []string{"version"}, &stdout, &stderr, "v4.5.6+test"); code != exitSuccess {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "v4.5.6+test" {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

type fakeProvisioner struct {
	next       int
	inspectErr error
	ensureErr  error
	plan       githubscaleset.ScaleSetPlan
}

func (f *fakeProvisioner) Inspect(context.Context, githubscaleset.ScaleSetSpec) (githubscaleset.ScaleSetPlan, error) {
	if f.inspectErr != nil {
		return githubscaleset.ScaleSetPlan{}, f.inspectErr
	}
	if f.plan.Action != "" {
		return f.plan, nil
	}
	return githubscaleset.ScaleSetPlan{Action: githubscaleset.ScaleSetCreate}, nil
}
func (f *fakeProvisioner) Ensure(_ context.Context, spec githubscaleset.ScaleSetSpec) (scaleset.RunnerScaleSet, error) {
	f.next++
	if f.ensureErr != nil {
		return scaleset.RunnerScaleSet{}, f.ensureErr
	}
	return scaleset.RunnerScaleSet{ID: f.next, Name: spec.Name}, nil
}

func fleetProvisionConfig() config.Config {
	cfg := config.Default()
	cfg.Targets = []config.Target{{Type: "repo", Slug: "owner/repo", MaxActive: 4}}
	sets := []config.ScaleSet{{Profile: "small", Name: "repo-small", MaxCapacity: 4, Labels: []string{"self-hosted", "linux-small"}},
		{Profile: "medium", Name: "repo-medium", MaxCapacity: 4, Labels: []string{"self-hosted", "linux-medium"}},
		{Profile: "large", Name: "repo-large", MaxCapacity: 2, Labels: []string{"self-hosted", "linux-large"}},
		{Profile: "builder", Name: "repo-builder", MaxCapacity: 1, Labels: []string{"self-hosted", "macos-builder"}},
		{Profile: "maestro", Name: "repo-maestro", MaxCapacity: 2, Labels: []string{"self-hosted", "macos-maestro"}}}
	cfg.GitHub = config.GitHub{SessionOwner: "host", CanonicalJobInventory: true,
		App:           config.GitHubApp{ClientID: "client", KeychainService: "service", KeychainAccount: "account"},
		Installations: []config.GitHubInstallation{{Name: "personal", InstallationID: 7}}, Scopes: []config.GitHubScope{{Name: "repo", Kind: config.ScopeRepository,
			ConfigURL: "https://github.com/owner/repo", Installation: "personal", Targets: []string{"owner/repo"}, ScaleSets: sets}}}
	return cfg
}

func encodedFleetConfig(t *testing.T) string {
	t.Helper()
	var output bytes.Buffer
	if err := config.Encode(&output, fleetProvisionConfig()); err != nil {
		t.Fatal(err)
	}
	return output.String()
}

func TestScaleSetProvisionCommandPlansGuardsAndPersists(t *testing.T) {
	cfg := fleetProvisionConfig()
	path := filepath.Join(t.TempDir(), "fleet.json")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Encode(file, cfg); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	fake := &fakeProvisioner{}
	deps := defaultDependencies()
	deps.loadPrivateKey = func(context.Context, string, string, string) (*githubscaleset.PrivateKeySecret, error) {
		return githubscaleset.NewPrivateKeySecret("pem"), nil
	}
	deps.openProvision = func(githubscaleset.GitHubAppAdminConfig) (provision.Client, error) { return fake, nil }
	var stdout, stderr bytes.Buffer
	if code := executeWith(context.Background(), []string{"scale-sets", "provision", "--config", path}, &stdout, &stderr, deps); code != exitSuccess || !strings.Contains(stdout.String(), "create") {
		t.Fatalf("plan code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := executeWith(context.Background(), []string{"scale-sets", "provision", "--config", path, "--apply"}, &stdout, &stderr, deps); code != exitUnsafe {
		t.Fatalf("unguarded apply code=%d err=%q", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	args := []string{"scale-sets", "provision", "--config", path, "--output", "json", "--apply", "--write", "--confirm", "provision-scale-sets", "--reason", "initial migration"}
	if code := executeWith(context.Background(), args, &stdout, &stderr, deps); code != exitSuccess || !strings.Contains(stdout.String(), `"id": 1`) {
		t.Fatalf("apply code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
	persisted, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := config.Decode(persisted)
	_ = persisted.Close()
	if err != nil || decoded.GitHub.Scopes[0].ScaleSets[4].ID == 0 {
		t.Fatalf("persisted config=%#v err=%v", decoded.GitHub.Scopes, err)
	}
}

type fakeReadCloser struct {
	io.Reader
	err error
}

func (f fakeReadCloser) Close() error { return f.err }

func TestScaleSetProvisionCommandFailureGuardsAndSecretRedaction(t *testing.T) {
	valid := encodedFleetConfig(t)
	newDeps := func() dependencies {
		deps := defaultDependencies()
		deps.openConfig = func(string) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(valid)), nil }
		deps.loadPrivateKey = func(context.Context, string, string, string) (*githubscaleset.PrivateKeySecret, error) {
			return githubscaleset.NewPrivateKeySecret("PRIVATE-KEY-SENTINEL"), nil
		}
		deps.openProvision = func(githubscaleset.GitHubAppAdminConfig) (provision.Client, error) { return &fakeProvisioner{}, nil }
		return deps
	}
	applyArgs := []string{"scale-sets", "provision", "--config", "fleet.json", "--apply", "--write", "--confirm", "provision-scale-sets", "--reason", "migration"}
	tests := []struct {
		name   string
		args   []string
		edit   func(*dependencies)
		writer io.Writer
		code   int
		text   string
	}{
		{name: "missing subcommand", args: []string{"scale-sets"}, code: exitUsage, text: "usage"},
		{name: "wrong subcommand", args: []string{"scale-sets", "delete"}, code: exitUsage, text: "usage"},
		{name: "bad flags", args: []string{"scale-sets", "provision", "--unknown"}, code: exitUsage, text: "flag provided"},
		{name: "bad output", args: []string{"scale-sets", "provision", "--config", "fleet.json", "--output", "yaml"}, code: exitUsage},
		{name: "mutation without apply", args: []string{"scale-sets", "provision", "--config", "fleet.json", "--write"}, code: exitUsage, text: "mutation flags"},
		{name: "open config", args: []string{"scale-sets", "provision", "--config", "fleet.json"}, edit: func(d *dependencies) {
			d.openConfig = func(string) (io.ReadCloser, error) { return nil, errors.New("permission denied") }
		}, code: exitFailure, text: "open config"},
		{name: "decode config", args: []string{"scale-sets", "provision", "--config", "fleet.json"}, edit: func(d *dependencies) {
			d.openConfig = func(string) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("{")), nil }
		}, code: exitFailure, text: "invalid config"},
		{name: "close config", args: []string{"scale-sets", "provision", "--config", "fleet.json"}, edit: func(d *dependencies) {
			d.openConfig = func(string) (io.ReadCloser, error) {
				return fakeReadCloser{Reader: strings.NewReader(valid), err: errors.New("close failed")}, nil
			}
		}, code: exitFailure, text: "close config"},
		{name: "conflict", args: []string{"scale-sets", "provision", "--config", "fleet.json"}, edit: func(d *dependencies) {
			d.openProvision = func(githubscaleset.GitHubAppAdminConfig) (provision.Client, error) {
				return &fakeProvisioner{inspectErr: operations.ErrConflict}, nil
			}
		}, code: exitUnsafe, text: "provision scale sets"},
		{name: "uncertain", args: []string{"scale-sets", "provision", "--config", "fleet.json"}, edit: func(d *dependencies) {
			d.openProvision = func(githubscaleset.GitHubAppAdminConfig) (provision.Client, error) {
				// "update" became a recognized action when drift repair landed; this case
				// proves an action the provisioner never emits still fails closed.
				return &fakeProvisioner{plan: githubscaleset.ScaleSetPlan{Action: "unrecognized-action"}}, nil
			}
		}, code: exitUnsafe, text: "uncertain"},
		{name: "ordinary provision failure", args: []string{"scale-sets", "provision", "--config", "fleet.json"}, edit: func(d *dependencies) {
			d.openProvision = func(githubscaleset.GitHubAppAdminConfig) (provision.Client, error) {
				return &fakeProvisioner{inspectErr: errors.New("GitHub denied")}, nil
			}
		}, code: exitFailure, text: "GitHub denied"},
		{name: "persist failure", args: applyArgs, edit: func(d *dependencies) {
			d.writeConfig = func(string, config.Config) error { return errors.New("disk full") }
		}, code: exitFailure, text: "persist config"},
		{name: "json output failure", args: []string{"scale-sets", "provision", "--config", "fleet.json", "--output", "json"}, writer: errorWriter{}, code: exitFailure},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps := newDeps()
			if tt.edit != nil {
				tt.edit(&deps)
			}
			var stdout, stderr bytes.Buffer
			writer := tt.writer
			if writer == nil {
				writer = &stdout
			}
			code := executeWith(context.Background(), tt.args, writer, &stderr, deps)
			combined := stdout.String() + stderr.String()
			if code != tt.code || (tt.text != "" && !strings.Contains(combined, tt.text)) {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if strings.Contains(combined, "PRIVATE-KEY-SENTINEL") {
				t.Fatalf("private key leaked: %q", combined)
			}
		})
	}
}

type fakeLoadedSecret struct {
	value     string
	destroyed bool
}

func (s *fakeLoadedSecret) Reveal() string { return s.value }
func (s *fakeLoadedSecret) Destroy()       { s.value, s.destroyed = "", true }

func TestPrivateKeyLoaderTransfersAndDestroysKeychainSecret(t *testing.T) {
	loaded := &fakeLoadedSecret{value: "PRIVATE-KEY-SENTINEL"}
	loader := privateKeyLoader(func(context.Context, string, string, string) (loadedSecret, error) { return loaded, nil })
	key, err := loader(context.Background(), "service", "account", "")
	if err != nil || key == nil || !loaded.destroyed || loaded.value != "" {
		t.Fatalf("load = %v, %v; secret=%#v", key, err, loaded)
	}
	if strings.Contains(fmt.Sprintf("%v %#v %+v", key, key, key), "PRIVATE-KEY-SENTINEL") {
		t.Fatal("private key leaked through formatting")
	}
	key.Destroy()
	want := errors.New("keychain unavailable")
	loader = privateKeyLoader(func(context.Context, string, string, string) (loadedSecret, error) { return nil, want })
	if _, err := loader(context.Background(), "service", "account", ""); !errors.Is(err, want) {
		t.Fatalf("load error = %v", err)
	}
	loader = privateKeyLoader(func(context.Context, string, string, string) (loadedSecret, error) { return nil, nil })
	if _, err := loader(context.Background(), "service", "account", ""); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("nil secret error = %v", err)
	}
	deps := defaultDependencies()
	if _, err := deps.loadPrivateKey(context.Background(), "", "", ""); err == nil {
		t.Fatal("default Keychain loader accepted empty reference")
	}
	path := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(path, []byte("file-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	fileKey, err := deps.loadPrivateKey(context.Background(), "", "", path)
	if err != nil || fileKey == nil {
		t.Fatalf("default file loader = %v, %v", fileKey, err)
	}
	fileKey.Destroy()
	if _, err := deps.openProvision(githubscaleset.GitHubAppAdminConfig{}); err == nil {
		t.Fatal("default provisioner accepted empty configuration")
	}
}

type fakeAtomicConfigFile struct {
	bytes.Buffer
	name                     string
	chmodErr, writeErr       error
	syncErr, closeErr        error
	chmodded, synced, closed bool
}

func (f *fakeAtomicConfigFile) Name() string { return f.name }
func (f *fakeAtomicConfigFile) Chmod(os.FileMode) error {
	f.chmodded = true
	return f.chmodErr
}
func (f *fakeAtomicConfigFile) Write(value []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return f.Buffer.Write(value)
}
func (f *fakeAtomicConfigFile) Sync() error {
	f.synced = true
	return f.syncErr
}
func (f *fakeAtomicConfigFile) Close() error {
	f.closed = true
	return f.closeErr
}

func TestAtomicWriteConfigFailsClosedAtEveryDurabilityStep(t *testing.T) {
	want := errors.New("injected failure")
	for _, tt := range []struct {
		name       string
		file       *fakeAtomicConfigFile
		createErr  error
		renameErr  error
		wantErr    bool
		wantRemove bool
	}{
		{name: "create", createErr: want, wantErr: true},
		{name: "chmod", file: &fakeAtomicConfigFile{name: "/tmp/config", chmodErr: want}, wantErr: true, wantRemove: true},
		{name: "encode", file: &fakeAtomicConfigFile{name: "/tmp/config", writeErr: want}, wantErr: true, wantRemove: true},
		{name: "sync", file: &fakeAtomicConfigFile{name: "/tmp/config", syncErr: want}, wantErr: true, wantRemove: true},
		{name: "close", file: &fakeAtomicConfigFile{name: "/tmp/config", closeErr: want}, wantErr: true, wantRemove: true},
		{name: "rename", file: &fakeAtomicConfigFile{name: "/tmp/config"}, renameErr: want, wantErr: true, wantRemove: true},
		{name: "success", file: &fakeAtomicConfigFile{name: "/tmp/config"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			removed := false
			renamed := false
			ops := atomicConfigOps{
				createTemp: func(directory, pattern string) (atomicConfigFile, error) {
					if directory != "/config" || pattern != ".fleet-config-*" {
						t.Fatalf("createTemp(%q, %q)", directory, pattern)
					}
					return tt.file, tt.createErr
				},
				remove: func(path string) error { removed = true; return nil },
				rename: func(old, new string) error {
					renamed = true
					if old != "/tmp/config" || new != "/config/fleet.json" {
						t.Fatalf("rename(%q, %q)", old, new)
					}
					return tt.renameErr
				},
			}
			err := atomicWriteConfigWith("/config/fleet.json", config.Default(), ops)
			if (err != nil) != tt.wantErr || removed != tt.wantRemove {
				t.Fatalf("error=%v removed=%v", err, removed)
			}
			if !tt.wantErr && (!renamed || !tt.file.chmodded || !tt.file.synced || !tt.file.closed) {
				t.Fatalf("success did not complete durability sequence: %#v", tt.file)
			}
		})
	}
}
