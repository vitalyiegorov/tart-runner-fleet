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
				return app.ValidateLocalHost(cfg, tc.goos, os.Stat)
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
