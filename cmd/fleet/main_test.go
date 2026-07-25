package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

// TestExecuteRoutesRunToTheDaemon pins that `run` reaches the daemon surface
// and not the operator surface. The daemon rejects an unknown flag with its own
// usage line, which the CLI would never emit.
func TestExecuteRoutesRunToTheDaemon(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := execute(context.Background(), []string{"run", "--not-a-flag"}, &stdout, &stderr); code != 2 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "flag provided but not defined") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

// TestExecuteRoutesEverythingElseToTheCLI pins that non-`run` verbs reach the
// operator surface, including the empty argument vector.
func TestExecuteRoutesEverythingElseToTheCLI(t *testing.T) {
	for _, tt := range []struct {
		name     string
		args     []string
		wantCode int
		contains string
	}{
		{name: "help", args: []string{"help"}, wantCode: 0, contains: "READ-ONLY"},
		{name: "no args", args: nil, wantCode: 2, contains: "usage"},
		{name: "unknown", args: []string{"nonsense"}, wantCode: 2, contains: "unknown command"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := execute(context.Background(), tt.args, &stdout, &stderr)
			if code != tt.wantCode {
				t.Fatalf("code=%d stderr=%q", code, stderr.String())
			}
			if !strings.Contains(stdout.String()+stderr.String(), tt.contains) {
				t.Fatalf("missing %q in %q %q", tt.contains, stdout.String(), stderr.String())
			}
		})
	}
}

// TestVersionIsTheInjectedBuildIdentity pins that the entry point hands its one
// version variable to the operator surface. The daemon half of the same
// invariant is pinned by TestExecuteThreadsInjectedVersion in internal/daemon;
// together they establish that one executable cannot report two builds.
func TestVersionIsTheInjectedBuildIdentity(t *testing.T) {
	original := version
	t.Cleanup(func() { version = original })
	version = "v9.9.9+identity"

	var stdout, stderr bytes.Buffer
	if code := execute(context.Background(), []string{"version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != version {
		t.Fatalf("stdout=%q want %q", stdout.String(), version)
	}
}

func TestMainDelegatesExit(t *testing.T) {
	originalArgs, originalExit := os.Args, exit
	t.Cleanup(func() { os.Args, exit = originalArgs, originalExit })
	os.Args = []string{"fleet", "version"}
	code := -1
	exit = func(value int) { code = value }
	main()
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
}
