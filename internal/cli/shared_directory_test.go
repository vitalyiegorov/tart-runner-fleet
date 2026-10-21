package cli

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
)

func TestConfigValidateLocalShareAndRemoteFleet(t *testing.T) {
	for _, tc := range []struct {
		name, goos                       string
		disabled, multiple, alias, valid bool
		want                             int
	}{
		{name: "absent local mac", goos: "darwin", want: exitFailure},
		{name: "legacy alias", goos: "darwin", alias: true, want: exitFailure},
		{name: "valid local directory", goos: "darwin", valid: true},
		{name: "disabled mac", goos: "darwin", disabled: true},
		{name: "linux does not inspect mac path", goos: "linux"},
		{name: "multiple remote macs", goos: "darwin", multiple: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.MacOS.Enabled = !tc.disabled
			cfg.MacOS.SharedDirectoryPath = t.TempDir() + "/absent"
			if tc.valid {
				cfg.MacOS.SharedDirectoryPath = t.TempDir()
			}
			var body bytes.Buffer
			if err := config.Encode(&body, cfg); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			path := writeNode(t, dir, "local.json", body.String())
			args := []string{"config", "validate", path}
			if tc.alias {
				args = []string{"validate-config", path}
			}
			if tc.multiple {
				args = append(args, writeNode(t, dir, "remote.json", body.String()))
			}
			d := defaultDependencies()
			calls := 0
			d.validateLocalHost = func(cfg config.Config) error {
				calls++
				return app.ValidateLocalHost(cfg, tc.goos, os.Stat, func() (string, error) { return "26.5", nil })
			}
			var stdout, stderr bytes.Buffer
			code := executeWith(context.Background(), args, &stdout, &stderr, d)
			if code != tc.want {
				t.Fatalf("code=%d stderr=%q", code, stderr.String())
			}
			if tc.multiple && calls != 0 {
				t.Fatal("cross-node validation inspected local host")
			}
			if tc.want == exitFailure && (!strings.Contains(stderr.String(), "sharedDirectoryPath") || stdout.Len() != 0) {
				t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		})
	}
}

// Replay #363 at the operator's own check: `fleet config validate` on the
// owning Mac names an unvalidated host macOS before the daemon is ever started.
func TestConfigValidateRefusesMacOSGuestsOnUnvalidatedHostOS(t *testing.T) {
	for _, tc := range []struct {
		name, version string
		disabled      bool
		want          int
	}{
		{name: "macos 27 host", version: "27.0.1", want: exitFailure},
		{name: "macos 27 host with macos guests disabled", version: "27.0.1", disabled: true},
		{name: "macos 26 host", version: "26.5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.MacOS.Enabled = !tc.disabled
			var body bytes.Buffer
			if err := config.Encode(&body, cfg); err != nil {
				t.Fatal(err)
			}
			path := writeNode(t, t.TempDir(), "local.json", body.String())
			d := defaultDependencies()
			d.validateLocalHost = func(cfg config.Config) error {
				return app.ValidateLocalHost(cfg, "darwin", os.Stat, func() (string, error) { return tc.version, nil })
			}
			var stdout, stderr bytes.Buffer
			code := executeWith(context.Background(), []string{"config", "validate", path}, &stdout, &stderr, d)
			if code != tc.want {
				t.Fatalf("code=%d stderr=%q", code, stderr.String())
			}
			if tc.want == exitFailure && (!strings.Contains(stderr.String(), "macosBurst.enabled") || !strings.Contains(stderr.String(), tc.version)) {
				t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		})
	}
}
