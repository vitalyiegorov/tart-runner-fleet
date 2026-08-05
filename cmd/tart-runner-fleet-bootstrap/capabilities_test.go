package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/guestbootstrap"
)

// TestExecuteAcceptsOnlyTheCapabilityFlag keeps the helper's argument vector as
// closed as it has always been. Exactly one optional flag exists; anything else
// is still a usage error, so a guest cannot be talked into doing something the
// daemon did not ask for.
func TestExecuteAcceptsOnlyTheCapabilityFlag(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantSeen []string
	}{
		{name: "no arguments", wantCode: 0},
		{name: "one capability", args: []string{guestbootstrap.CapabilityFlag + "=redroid-android"},
			wantCode: 0, wantSeen: []string{"redroid-android"}},
		{name: "several capabilities", args: []string{guestbootstrap.CapabilityFlag + "=jvm,maestro-cli"},
			wantCode: 0, wantSeen: []string{"jvm", "maestro-cli"}},
		{name: "an unknown argument", args: []string{"unexpected"}, wantCode: 2},
		{name: "the flag with no value", args: []string{guestbootstrap.CapabilityFlag + "="}, wantCode: 2},
		{name: "the flag separated from its value", args: []string{guestbootstrap.CapabilityFlag, "jvm"}, wantCode: 2},
		{name: "a capability outside the vocabulary",
			args: []string{guestbootstrap.CapabilityFlag + "=Redroid_Android"}, wantCode: 2},
		{name: "more arguments than exist", args: []string{guestbootstrap.CapabilityFlag + "=jvm", "extra"}, wantCode: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stderr bytes.Buffer
			var seen []string
			run := func(_ context.Context, _ io.Reader, config guestbootstrap.Config) error {
				seen = config.RequiredCapabilities
				return nil
			}
			if code := execute(test.args, strings.NewReader("jit"), &stderr, run); code != test.wantCode {
				t.Fatalf("execute(%v) = %d, want %d (stderr %q)", test.args, code, test.wantCode, stderr.String())
			}
			if strings.Join(seen, ",") != strings.Join(test.wantSeen, ",") {
				t.Fatalf("required capabilities = %v, want %v", seen, test.wantSeen)
			}
		})
	}
}

// TestExecuteReportsACapabilityFailureOutLoud is the one exception to "never
// reflect a child error": a capability failure is built from the operator's own
// flag and the image's own manifest before standard input is read, so it cannot
// contain the JIT configuration, and the two statuses tell the daemon which of
// the two opposite repairs is needed.
func TestExecuteReportsACapabilityFailureOutLoud(t *testing.T) {
	tests := []struct {
		name     string
		failure  *guestbootstrap.CapabilityError
		wantCode int
		wantText string
	}{
		{name: "missing", failure: &guestbootstrap.CapabilityError{MissingCapability: "redroid-android",
			Detail: "image \"linux-runner-base\" sealed at 2026-08-05T04:00:00Z declares [container-runtime]"},
			wantCode: guestbootstrap.ExitCapabilityMissing, wantText: "redroid-android"},
		{name: "unverifiable", failure: &guestbootstrap.CapabilityError{Detail: "cannot be read"},
			wantCode: guestbootstrap.ExitCapabilityUnverifiable, wantText: "cannot be read"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stderr bytes.Buffer
			run := func(context.Context, io.Reader, guestbootstrap.Config) error { return test.failure }
			if code := execute(nil, strings.NewReader("jit"), &stderr, run); code != test.wantCode {
				t.Fatalf("execute() = %d, want %d", code, test.wantCode)
			}
			if !strings.Contains(stderr.String(), test.wantText) {
				t.Fatalf("stderr = %q, want it to name %q", stderr.String(), test.wantText)
			}
		})
	}
}

// TestExecuteWithoutARunnerFails covers the wiring guard: a nil bootstrap is a
// programming error, and it must fail the instance rather than report success.
func TestExecuteWithoutARunnerFails(t *testing.T) {
	var stderr bytes.Buffer
	if code := execute(nil, strings.NewReader("jit"), &stderr, nil); code != 1 ||
		stderr.String() != "runner bootstrap failed\n" {
		t.Fatalf("execute(nil run) = %d, stderr=%q", code, stderr.String())
	}
}
