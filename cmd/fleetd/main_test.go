package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestExecute(t *testing.T) {
	original := run
	t.Cleanup(func() { run = original })
	run = func(_ context.Context, opts options) error {
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
		{name: "version", args: []string{"version"}, code: 0, out: "dev"},
		{name: "run defaults", args: []string{"run"}, code: 0},
		{name: "run flags", args: []string{"run", "--config", "x", "--database", "d", "--mode", "shadow", "--admin-socket", "/tmp/fleet.sock"}, code: 0},
		{name: "runtime error", args: []string{"run", "--config", "bad"}, code: 1, err: "fleetd failed"},
		{name: "usage", code: 2, err: "usage:"},
		{name: "wrong command", args: []string{"bad"}, code: 2, err: "usage:"},
		{name: "bad flag", args: []string{"run", "--bad"}, code: 2, err: "flag provided"},
		{name: "position", args: []string{"run", "extra"}, code: 2, err: "unexpected positional"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := execute(context.Background(), tt.args, &stdout, &stderr); code != tt.code || !strings.Contains(stdout.String(), tt.out) || !strings.Contains(stderr.String(), tt.err) {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestMainDelegatesExit(t *testing.T) {
	originalArgs, originalExit, originalRun := os.Args, exit, run
	t.Cleanup(func() { os.Args, exit, run = originalArgs, originalExit, originalRun })
	os.Args = []string{"fleetd", "version"}
	code := -1
	exit = func(value int) { code = value }
	main()
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
}
