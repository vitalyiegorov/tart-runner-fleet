package lifecycle

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

type fixedGate bool

func (g fixedGate) Verified(context.Context) bool { return bool(g) }

type recordingRunner struct {
	calls  [][]string
	failOn string
}

func (r *recordingRunner) Run(_ context.Context, _ io.Reader, args ...string) error {
	r.calls = append(r.calls, append([]string(nil), args...))
	if r.failOn != "" && strings.Contains(strings.Join(args, " "), r.failOn) {
		return errors.New("failed")
	}
	return nil
}

const testNPMRegistry = "http://192.168.1.64:3017/npm/"

func npmRoute(ready bool) *NPMRoute {
	return &NPMRoute{Registry: testNPMRegistry, Host: "192.168.1.64", Ready: fixedGate(ready)}
}

func isNPMCall(args []string) bool {
	return len(args) > 4 && (args[4] == npmClearScript || args[4] == npmWriteScript)
}

func TestAProvenGateGivesTheGuestItsRegistryBeforeTheRunnerStarts(t *testing.T) {
	runner := &recordingRunner{}
	bootstrap := StdinBootstrapper{Runner: runner, Gate: fixedGate(true), NPM: npmRoute(true)}
	if err := bootstrap.Bootstrap(context.Background(), "trf-small-1", jitSecret(), nil); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 3 {
		t.Fatalf("calls = %q", runner.calls)
	}
	clear := []string{"exec", "trf-small-1", "bash", "-c", npmClearScript, npmScriptName}
	if strings.Join(runner.calls[0], "\x00") != strings.Join(clear, "\x00") {
		t.Fatalf("first call = %q, want the cleanup", runner.calls[0])
	}
	write := []string{"exec", "trf-small-1", "bash", "-c", npmWriteScript, npmScriptName, testNPMRegistry, "192.168.1.64"}
	if strings.Join(runner.calls[1], "\x00") != strings.Join(write, "\x00") {
		t.Fatalf("second call = %q, want the write", runner.calls[1])
	}
	if runner.calls[2][3] != bootstrapHelper {
		t.Fatalf("the runner did not start after the setting: %q", runner.calls[2])
	}
}

// TestAnUnprovenGateClearsAnyRegistryAndLeavesTheResultsFallbackAlone: a launch can
// retry on the same guest, so what an earlier proof wrote must not outlive this one.
func TestAnUnprovenGateClearsAnyRegistryAndLeavesTheResultsFallbackAlone(t *testing.T) {
	runner := &recordingRunner{}
	bootstrap := StdinBootstrapper{Runner: runner, Gate: fixedGate(false), NPM: npmRoute(true)}
	if err := bootstrap.Bootstrap(context.Background(), "trf-small-1", jitSecret(), nil); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 3 {
		t.Fatalf("calls = %q", runner.calls)
	}
	if runner.calls[0][4] != resultsFallbackScript {
		t.Fatalf("first call = %q", runner.calls[0])
	}
	if runner.calls[1][4] != npmClearScript {
		t.Fatalf("second call = %q, want a clear", runner.calls[1])
	}
	if runner.calls[2][3] != bootstrapHelper {
		t.Fatalf("calls = %q", runner.calls)
	}
}

// TestAProvenGateWhoseNPMRouteIsDownSendsTheGuestDirect: a verified gate with a dead
// Verdaccio would otherwise fail every npm install in the job.
func TestAProvenGateWhoseNPMRouteIsDownSendsTheGuestDirect(t *testing.T) {
	runner := &recordingRunner{}
	bootstrap := StdinBootstrapper{Runner: runner, Gate: fixedGate(true), NPM: npmRoute(false)}
	if err := bootstrap.Bootstrap(context.Background(), "trf-small-1", jitSecret(), nil); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 || runner.calls[0][4] != npmClearScript || runner.calls[1][3] != bootstrapHelper {
		t.Fatalf("calls = %q", runner.calls)
	}
}

// TestAGateProofIsNotAskedTwiceAndTheNPMProbeNeedsIt: the probe never runs on an
// address the identity check just refused.
type countingGate struct {
	answer bool
	asked  *int
}

func (g countingGate) Verified(context.Context) bool {
	*g.asked++
	return g.answer
}

func TestTheNPMRouteIsNeverAskedOfAnUnprovenGate(t *testing.T) {
	proofs, probes := 0, 0
	bootstrap := StdinBootstrapper{Runner: &recordingRunner{}, Gate: countingGate{false, &proofs},
		NPM: &NPMRoute{Registry: testNPMRegistry, Host: "192.168.1.64", Ready: countingGate{true, &probes}}}
	if err := bootstrap.Bootstrap(context.Background(), "trf-small-1", jitSecret(), nil); err != nil {
		t.Fatal(err)
	}
	if proofs != 1 || probes != 0 {
		t.Fatalf("proofs=%d probes=%d", proofs, probes)
	}
}

// TestANodeWithNoNPMRouteNeverTouchesAGuestsNpmConfig is the feature's off switch,
// and the rollback: every existing call is exactly what it was.
func TestANodeWithNoNPMRouteNeverTouchesAGuestsNpmConfig(t *testing.T) {
	for _, verified := range []bool{true, false} {
		runner := &recordingRunner{}
		bootstrap := StdinBootstrapper{Runner: runner, Gate: fixedGate(verified)}
		if err := bootstrap.Bootstrap(context.Background(), "trf-small-1", jitSecret(), nil); err != nil {
			t.Fatal(err)
		}
		for _, call := range runner.calls {
			if isNPMCall(call) {
				t.Fatalf("verified=%v: %q", verified, call)
			}
		}
	}
}

// TestAFailedRegistryWriteStillStartsTheRunner: the cleanup has already succeeded, the
// file is written atomically, and its absence means npmjs, so this failure, even a lost
// exec, costs a cold job and not a job.
func TestAFailedRegistryWriteStillStartsTheRunner(t *testing.T) {
	runner := &recordingRunner{failOn: testNPMRegistry}
	bootstrap := StdinBootstrapper{Runner: runner, Gate: fixedGate(true), NPM: npmRoute(true)}
	if err := bootstrap.Bootstrap(context.Background(), "trf-small-1", jitSecret(), nil); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 3 || runner.calls[1][4] != npmWriteScript || runner.calls[2][3] != bootstrapHelper {
		t.Fatalf("calls = %q", runner.calls)
	}
}

// TestAFailedClearNeverStartsTheRunner holds the same line the results fallback does:
// a guest that may still name an unproven gate does not run.
func TestAFailedClearNeverStartsTheRunner(t *testing.T) {
	for name, launch := range map[string]struct {
		gate  GateVerifier
		ready bool
	}{"unproven gate": {fixedGate(false), true}, "npm route down": {fixedGate(true), false}, "proven gate": {fixedGate(true), true}} {
		runner := &failingRunner{failOn: npmClearScript}
		secret := jitSecret()
		bootstrap := StdinBootstrapper{Runner: runner, Gate: launch.gate, NPM: npmRoute(launch.ready)}
		if err := bootstrap.Bootstrap(context.Background(), "trf-small-1", secret, nil); err == nil {
			t.Fatalf("%s: a guest that may name an unproven gate was started", name)
		}
		if secret.Reveal() != "" {
			t.Fatalf("%s: the secret survived", name)
		}
		for _, call := range runner.calls {
			if len(call) > 3 && call[3] == bootstrapHelper {
				t.Fatalf("%s: calls = %q", name, runner.calls)
			}
		}
	}
}

// failingRunner fails a call holding exactly this argument.
type failingRunner struct {
	calls  [][]string
	failOn string
}

func (r *failingRunner) Run(_ context.Context, _ io.Reader, args ...string) error {
	r.calls = append(r.calls, append([]string(nil), args...))
	for _, arg := range args {
		if arg == r.failOn {
			return errors.New("failed")
		}
	}
	return nil
}

func TestAContainerGuestTakesTheSameSelection(t *testing.T) {
	runner := &recordingRunner{}
	bootstrap := StdinBootstrapper{Runner: runner, ContainerGuest: true, Gate: fixedGate(true),
		NPM: &NPMRoute{Registry: "http://host.containers.internal:3017/npm/", Host: "host.containers.internal", Ready: fixedGate(true)}}
	if err := bootstrap.Bootstrap(context.Background(), "trf-small-1", jitSecret(), nil); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 3 || runner.calls[0][4] != npmClearScript || runner.calls[1][4] != npmWriteScript ||
		runner.calls[1][6] != "http://host.containers.internal:3017/npm/" || runner.calls[1][7] != "host.containers.internal" {
		t.Fatalf("calls = %q", runner.calls)
	}
}

// runNPMScript runs the real script in a throwaway HOME with a stand-in curl.
func runNPMScript(t *testing.T, script, existing string, curlExit int, args ...string) (string, []string, string, error) {
	t.Helper()
	home, bin := t.TempDir(), t.TempDir()
	recorded := filepath.Join(bin, "curl.args")
	curl := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + recorded + "\nprintf 200\nexit " + strconv.Itoa(curlExit) + "\n"
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(curl), 0o755); err != nil {
		t.Fatal(err)
	}
	rc := filepath.Join(home, ".npmrc")
	if existing != "" {
		if err := os.WriteFile(rc, []byte(existing), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.Command("bash", append([]string{"-c", script, npmScriptName}, args...)...)
	command.Env = []string{"HOME=" + home, "PATH=" + bin + ":" + os.Getenv("PATH")}
	runErr := command.Run()
	raw, _ := os.ReadFile(rc)
	curlArgs, _ := os.ReadFile(recorded)
	entries, _ := os.ReadDir(home)
	leftovers := ""
	for _, entry := range entries {
		if entry.Name() != ".npmrc" {
			leftovers += entry.Name()
		}
	}
	return string(raw), strings.Fields(string(curlArgs)), leftovers, runErr
}

func TestTheScriptWritesTheRegistryOnlyAfterAskingDirectly(t *testing.T) {
	got, curlArgs, leftovers, err := runNPMScript(t, npmWriteScript, "", 0, testNPMRegistry, "192.168.1.64")
	if err != nil {
		t.Fatal(err)
	}
	want := npmMarker + "\nregistry=" + testNPMRegistry + "\nnoproxy=192.168.1.64\nno-proxy=192.168.1.64\n"
	if got != want {
		t.Fatalf(".npmrc = %q, want %q", got, want)
	}
	if leftovers != "" {
		t.Fatalf("left behind %q", leftovers)
	}
	joined := strings.Join(curlArgs, " ")
	for _, flag := range []string{"--noproxy *", "--max-redirs 0", "--proto =http", "--write-out %{http_code}", testNPMRegistry + "-/ping"} {
		if !strings.Contains(joined, flag) {
			t.Fatalf("curl %q lacks %q", joined, flag)
		}
	}
}

// TestAGuestThatCannotReachTheRouteGetsNoRegistry: the host proved the gate from its
// own network. A guest on a NAT or an isolated network must not be sent there blind,
// since no client falls back once its registry is set.
func TestAGuestThatCannotReachTheRouteGetsNoRegistry(t *testing.T) {
	got, _, leftovers, err := runNPMScript(t, npmWriteScript, "", 7, testNPMRegistry, "192.168.1.64")
	if err != nil {
		t.Fatal(err)
	}
	if got != "" || leftovers != "" {
		t.Fatalf(".npmrc = %q, leftovers = %q", got, leftovers)
	}
}

// TestTheScriptClearsOnlyItsOwnFile keeps an image's own config. That guest stays
// direct, because nothing merges a file this fleet did not write.
func TestTheScriptClearsOnlyItsOwnFile(t *testing.T) {
	mine := npmMarker + "\nregistry=" + testNPMRegistry + "\nnoproxy=192.168.1.64\n"
	got, curlArgs, _, err := runNPMScript(t, npmClearScript, mine, 0)
	if err != nil || got != "" || len(curlArgs) != 0 {
		t.Fatalf("clear: .npmrc = %q, curl = %q, err = %v", got, curlArgs, err)
	}
	theirs := "registry=https://npm.example/\n"
	got, curlArgs, _, err = runNPMScript(t, npmClearScript, theirs, 0)
	if err != nil || got != theirs || len(curlArgs) != 0 {
		t.Fatalf("clear: .npmrc = %q, curl = %q, err = %v", got, curlArgs, err)
	}
	got, curlArgs, _, err = runNPMScript(t, npmWriteScript, theirs, 0, testNPMRegistry, "192.168.1.64")
	if err != nil || got != theirs || len(curlArgs) != 0 {
		t.Fatalf("write: .npmrc = %q, curl = %q, err = %v", got, curlArgs, err)
	}
	got, _, _, err = runNPMScript(t, npmClearScript, "", 0)
	if err != nil || got != "" {
		t.Fatalf("clear with nothing there: .npmrc = %q, err = %v", got, err)
	}
}

func TestAnUnverifiedGateRewritesTheGuestBeforeTheRunnerStarts(t *testing.T) {
	runner := &recordingRunner{}
	bootstrap := StdinBootstrapper{Runner: runner, Gate: fixedGate(false)}
	if err := bootstrap.Bootstrap(context.Background(), "trf-small-1", jitSecret(), nil); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("calls = %q", runner.calls)
	}
	want := []string{"exec", "trf-small-1", "bash", "-c", resultsFallbackScript}
	if strings.Join(runner.calls[0], "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("first call = %q", runner.calls[0])
	}
	if runner.calls[1][3] != bootstrapHelper {
		t.Fatalf("the runner did not start after the rewrite: %q", runner.calls[1])
	}
}

func TestAVerifiedGateLeavesTheGuestAlone(t *testing.T) {
	runner := &recordingRunner{}
	bootstrap := StdinBootstrapper{Runner: runner, Gate: fixedGate(true)}
	if err := bootstrap.Bootstrap(context.Background(), "trf-small-1", jitSecret(), nil); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 || runner.calls[0][3] != bootstrapHelper {
		t.Fatalf("calls = %q", runner.calls)
	}
}

// TestANodeWithNoGateConfiguredIsUnchanged is the rollout's safety: a release on
// a node nobody has configured must not send its guests to the WAN.
func TestANodeWithNoGateConfiguredIsUnchanged(t *testing.T) {
	runner := &recordingRunner{}
	if err := (StdinBootstrapper{Runner: runner}).Bootstrap(context.Background(), "trf-small-1", jitSecret(), nil); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls = %q", runner.calls)
	}
}

func TestAFailedRewriteNeverStartsTheRunner(t *testing.T) {
	runner := &recordingRunner{failOn: "bash"}
	secret := jitSecret()
	bootstrap := StdinBootstrapper{Runner: runner, Gate: fixedGate(false)}
	if err := bootstrap.Bootstrap(context.Background(), "trf-small-1", secret, nil); err == nil {
		t.Fatal("a guest still pointed at an unverified gate was started")
	}
	if len(runner.calls) != 1 || secret.Reveal() != "" {
		t.Fatalf("calls = %q, secret left = %v", runner.calls, secret.Reveal() != "")
	}
}

func TestTheRewriteCancelledReturnsTheContextError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	bootstrap := StdinBootstrapper{Runner: &recordingRunner{failOn: "bash"}, Gate: fixedGate(false)}
	if err := bootstrap.Bootstrap(ctx, "trf-small-1", jitSecret(), nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func runFallback(t *testing.T, body string, exists bool) (string, error) {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, "actions-runner")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".env")
	if exists {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.Command("sh", "-c", resultsFallbackScript)
	command.Env = append(os.Environ(), "HOME="+home)
	runErr := command.Run()
	raw, _ := os.ReadFile(path)
	return string(raw), runErr
}

func TestTheScriptRewritesTheOverrideAndKeepsIt(t *testing.T) {
	got, err := runFallback(t, "OTHER=1\nACTIONS_RESULTS_URL=http://192.168.1.64:3017/\nLAST=2\n", true)
	if err != nil {
		t.Fatal(err)
	}
	want := "OTHER=1\nACTIONS_RESULTS_URL=https://results-receiver.actions.githubusercontent.com/\nLAST=2\n"
	if got != want {
		t.Fatalf(".env = %q", got)
	}
}

// TestTheScriptIgnoresAnImageWithNoOverride: a macOS guest or the pre-gate
// rollback base has no patched runner, and the job message's value still wins.
func TestTheScriptIgnoresAnImageWithNoOverride(t *testing.T) {
	for name, exists := range map[string]bool{"no line": true, "no file": false} {
		got, err := runFallback(t, "OTHER=1\n", exists)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if exists && got != "OTHER=1\n" {
			t.Fatalf("%s: .env = %q", name, got)
		}
		if !exists && got != "" {
			t.Fatalf("%s: created .env = %q", name, got)
		}
	}
}
