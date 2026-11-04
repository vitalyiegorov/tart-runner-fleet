package lifecycle

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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

func TestAnUnverifiedGateRewritesTheGuestBeforeTheRunnerStarts(t *testing.T) {
	runner := &recordingRunner{}
	bootstrap := StdinBootstrapper{Runner: runner, Gate: fixedGate(false)}
	if err := bootstrap.Bootstrap(context.Background(), "trf-small-1", jitSecret(), nil); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("calls = %q", runner.calls)
	}
	want := []string{"exec", "trf-small-1", "bash", "-lc", resultsFallbackScript}
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
