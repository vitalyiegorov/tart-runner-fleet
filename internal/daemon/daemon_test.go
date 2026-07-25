package daemon

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestExecute(t *testing.T) {
	original := run
	t.Cleanup(func() { run = original })
	var received options
	run = func(_ context.Context, opts options) error {
		received = opts
		if opts.ConfigPath == "bad" {
			return errors.New("no secret")
		}
		return nil
	}
	tests := []struct {
		name     string
		args     []string
		code     int
		out, err string
	}{
		{name: "run defaults", args: []string{"run"}, code: 0},
		{name: "run flags", args: []string{"run", "--config", "x", "--database", "d", "--mode", "shadow", "--admin-socket", "/tmp/fleet.sock"}, code: 0},
		{name: "canary requires selector", args: []string{"run", "--mode", "canary"}, code: 2, err: "canary requires"},
		{name: "canary selector", args: []string{"run", "--mode", "canary", "--canary-scope", "personal-repo", "--canary-profile", "small"}, code: 0},
		{name: "runtime error", args: []string{"run", "--config", "bad"}, code: 1, err: "fleet daemon failed"},
		{name: "usage", code: 2, err: "usage:"},
		{name: "wrong command", args: []string{"bad"}, code: 2, err: "usage:"},
		{name: "bad flag", args: []string{"run", "--bad"}, code: 2, err: "flag provided"},
		{name: "position", args: []string{"run", "extra"}, code: 2, err: "unexpected positional"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := Execute(context.Background(), tt.args, &stdout, &stderr, "dev"); code != tt.code || !strings.Contains(stdout.String(), tt.out) || !strings.Contains(stderr.String(), tt.err) {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if tt.name == "canary selector" && (received.CanaryScope != "personal-repo" || received.CanaryProfile != "small") {
				t.Fatalf("canary selector not forwarded: %#v", received)
			}
		})
	}
}

// TestExecuteThreadsInjectedVersion pins that the build identity handed to the
// daemon reaches the runtime options, and therefore GitHub and telemetry,
// rather than a package-local default. This is what lets one executable
// guarantee that the daemon and the operator interface are the same build.
func TestExecuteThreadsInjectedVersion(t *testing.T) {
	original := run
	t.Cleanup(func() { run = original })
	var received options
	run = func(_ context.Context, opts options) error {
		received = opts
		return nil
	}
	var stdout, stderr bytes.Buffer
	if code := Execute(context.Background(), []string{"run"}, &stdout, &stderr, "v1.2.3+test"); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if received.Version != "v1.2.3+test" {
		t.Fatalf("version=%q", received.Version)
	}
}
