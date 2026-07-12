package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExecute(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "valid.json")
	invalid := filepath.Join(dir, "invalid.json")
	if err := os.WriteFile(valid, []byte(`{
      "baseVm":"linux", "vmPrefix":"gha", "pollSeconds":20,
      "maxLinuxWhenMacosIdle":1, "maxLinuxCpu":2, "maxLinuxMemoryMb":4096,
      "linuxReservationAgeSeconds":300, "minFreeDiskGb":1,
      "linuxProfiles":[{"id":"small","label":"linux-small","cpu":1,"memoryMb":2048}],
      "macosBurst":{"enabled":false},
      "targets":[{"type":"repo","slug":"owner/repo","maxActive":1}]
    }`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(invalid, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantOutput string
		wantError  string
	}{
		{name: "version", args: []string{"version"}, wantCode: 0, wantOutput: "dev"},
		{name: "valid", args: []string{"validate-config", valid}, wantCode: 0, wantOutput: "valid"},
		{name: "invalid", args: []string{"validate-config", invalid}, wantCode: 1, wantError: "invalid config"},
		{name: "missing", args: []string{"validate-config", filepath.Join(dir, "missing")}, wantCode: 1, wantError: "open config"},
		{name: "usage", args: nil, wantCode: 2, wantError: "usage:"},
		{name: "bad arity", args: []string{"validate-config"}, wantCode: 2, wantError: "usage:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := execute(tt.args, &stdout, &stderr); got != tt.wantCode {
				t.Fatalf("execute() = %d, want %d", got, tt.wantCode)
			}
			if !strings.Contains(stdout.String(), tt.wantOutput) || !strings.Contains(stderr.String(), tt.wantError) {
				t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		})
	}
}

func TestMainDelegatesExitCode(t *testing.T) {
	originalArgs, originalExit := os.Args, exit
	t.Cleanup(func() { os.Args, exit = originalArgs, originalExit })
	os.Args = []string{"fleetctl", "version"}
	got := -1
	exit = func(code int) { got = code }
	main()
	if got != 0 {
		t.Fatalf("exit code = %d", got)
	}
}
