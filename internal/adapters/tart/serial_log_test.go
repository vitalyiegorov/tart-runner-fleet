package tart

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// Issue #236's guest kernel panicked and the panic reached nobody: the base
// image named a console the VM does not expose, and the adapter passed no sink
// to capture one into. This is the host half of that fix, and it is off by
// default because the guest half ships in an image rebuild.
func TestALinuxGuestCanWriteItsConsoleToTheHost(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "serial")
	adapter, runner, ownership := serialAdapter(t, directory)
	if err := adapter.Start(context.Background(), "trf-small-1", ownership); err != nil {
		t.Fatal(err)
	}
	run := findCommand(t, runner, "run")
	want := filepath.Join(directory, "trf-small-1.log")
	if index := indexOf(run, "--serial-path"); index < 0 || run[index+1] != want {
		t.Fatalf("tart run must name this instance's own console sink; got %v", run)
	}
	// The directory is created rather than assumed: a `tart run` that fails for
	// every instance on a node whose operator made one typo is a worse outcome
	// than a diagnostic file nobody reads.
	if info, err := os.Stat(directory); err != nil || !info.IsDir() {
		t.Fatalf("the console directory must be created; stat = %v %v", info, err)
	}
}

// Every node today runs with no console sink, and its argument vector must not
// move by a byte.
func TestAnUnconfiguredConsoleSinkChangesNothing(t *testing.T) {
	adapter, runner, ownership := serialAdapter(t, "")
	if err := adapter.Start(context.Background(), "trf-small-1", ownership); err != nil {
		t.Fatal(err)
	}
	if run := findCommand(t, runner, "run"); indexOf(run, "--serial-path") >= 0 {
		t.Fatalf("an unconfigured sink must add no flag; got %v", run)
	}
}

// A macOS guest never gets the flag, for the same reason it never gets
// --nested: the setting is a Linux-guest fact and the base image that fixes the
// console is the Linux one.
func TestAMacOSGuestNeverGetsAConsoleSink(t *testing.T) {
	directory := t.TempDir()
	adapter, runner, ownership := serialAdapter(t, directory)
	adapter.MacOSVMPrefixes = []string{"trf-small-"}
	if err := adapter.Start(context.Background(), "trf-small-1", ownership); err != nil {
		t.Fatal(err)
	}
	if run := findCommand(t, runner, "run"); indexOf(run, "--serial-path") >= 0 {
		t.Fatalf("a macOS guest must not be given a Linux console sink; got %v", run)
	}
}

// A node configured to keep a panic trace and silently not keeping one has a
// fact its operator needs, and discovering it after the next panic is the whole
// failure mode this exists to end.
func TestAnUnusableConsoleDirectoryFailsTheStart(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	adapter, _, ownership := serialAdapter(t, filepath.Join(file, "serial"))
	err := adapter.Start(context.Background(), "trf-small-1", ownership)
	if err == nil || !strings.Contains(err.Error(), "permission") {
		t.Fatalf("an unusable console directory must fail the start; got %v", err)
	}
}

func serialAdapter(t *testing.T, directory string) (*Adapter, *fakeRunner, operations.Ownership) {
	t.Helper()
	adapter, runner, registry, ownership := testAdapter(time.Unix(100, 0).UTC())
	adapter.LinuxSerialLogDirectory = directory
	runner.vms["trf-small-1"] = vm{Name: "trf-small-1", Source: "local"}
	if err := registry.PutOwnership(context.Background(), "trf-small-1", ownership); err != nil {
		t.Fatal(err)
	}
	return adapter, runner, ownership
}

func indexOf(args []string, want string) int {
	for index, arg := range args {
		if arg == want && index+1 < len(args) {
			return index
		}
	}
	return -1
}
