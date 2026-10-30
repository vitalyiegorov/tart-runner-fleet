package config

import (
	"bytes"
	"os"
	"reflect"
	"strings"
	"testing"
)

// hostProcessNode is the second daemon instance on a Mac (ADR 0090): it runs
// macOS jobs as host processes under a dedicated runner user, so it declares no
// Linux execution, keeps the shared admission envelope, and names the runner
// template, the runner user, and the root-installed launcher.
func hostProcessNode() Config {
	cfg := macOSOnlyNode()
	cfg.Executor = Executor{Backend: ExecutorHostProcess, Image: "/Users/ci-runner/actions-runner-template",
		RunnerUser: "ci-runner"}
	return cfg
}

func TestAHostProcessNodeValidatesAndRoundTrips(t *testing.T) {
	cfg := hostProcessNode()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if !cfg.RunsHostProcesses() {
		t.Fatal("a node naming the hostprocess backend does not run host processes")
	}
	if Default().RunsHostProcesses() || podmanNode().RunsHostProcesses() {
		t.Fatal("a Tart or container node claimed to run host processes")
	}
	var encoded bytes.Buffer
	if err := Encode(&encoded, cfg); err != nil {
		t.Fatalf("Encode() = %v", err)
	}
	decoded, err := Decode(bytes.NewReader(encoded.Bytes()))
	if err != nil {
		t.Fatalf("Decode() = %v", err)
	}
	if !reflect.DeepEqual(decoded.Executor, cfg.Executor) {
		t.Fatalf("executor round trip = %#v, want %#v", decoded.Executor, cfg.Executor)
	}
	if cfg.HostLauncherPath() != DefaultHostLauncher {
		t.Fatalf("launcher = %q, want the root-installed default", cfg.HostLauncherPath())
	}
	cfg.Executor.Binary = "/opt/trf/launcher"
	if cfg.HostLauncherPath() != "/opt/trf/launcher" {
		t.Fatalf("launcher = %q, want the configured path", cfg.HostLauncherPath())
	}
}

func TestHostProcessValidation(t *testing.T) {
	for name, testCase := range map[string]struct {
		mutate  func(*Config)
		wantErr string
	}{
		"no runner user": {
			mutate: func(c *Config) { c.Executor.RunnerUser = "" }, wantErr: "runnerUser"},
		"root as the runner user": {
			mutate: func(c *Config) { c.Executor.RunnerUser = "root" }, wantErr: "runnerUser"},
		"a runner user that is not a short name": {
			mutate: func(c *Config) { c.Executor.RunnerUser = "ci runner" }, wantErr: "runnerUser"},
		"a runner user read as an option": {
			mutate: func(c *Config) { c.Executor.RunnerUser = "-u" }, wantErr: "runnerUser"},
		"a relative runner template": {
			mutate: func(c *Config) { c.Executor.Image = "actions-runner" }, wantErr: "absolute"},
		"an unclean runner template": {
			mutate: func(c *Config) { c.Executor.Image = "/Users/ci-runner/../admin/runner" }, wantErr: "absolute"},
		"a relative launcher": {
			mutate: func(c *Config) { c.Executor.Binary = "launcher" }, wantErr: "absolute"},
		"a container-only setting": {
			mutate: func(c *Config) { c.Executor.HoldCommand = []string{"sleep"} }, wantErr: "container"},
		"a kvm grant": {
			mutate: func(c *Config) { c.Executor.KVMProfiles = []string{"maestro"} }, wantErr: "container"},
		"an emulator slot directory": {
			mutate: func(c *Config) { c.Executor.EmulatorSlotsDir = "/var/lib/slots" }, wantErr: "container"},
		"no macOS profiles to run": {
			mutate: func(c *Config) { c.MacOS.Enabled = false; c.Linux = Default().Linux }, wantErr: "macosBurst"},
		"Linux profiles beside host processes": {
			mutate:  func(c *Config) { c.Linux.Profiles = Default().Linux.Profiles; c.Linux.BaseVM = "linux" },
			wantErr: "linuxProfiles"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := hostProcessNode()
			testCase.mutate(&cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("Validate() = %v, want an error naming %q", err, testCase.wantErr)
			}
		})
	}
}

// TestARunnerUserBelongsToTheHostProcessBackend keeps the new key from meaning
// anything on a node that does not spawn host processes.
func TestARunnerUserBelongsToTheHostProcessBackend(t *testing.T) {
	cfg := podmanNode()
	cfg.Executor.RunnerUser = "ci-runner"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "runnerUser") {
		t.Fatalf("Validate() = %v, want a refusal of runnerUser on a container node", err)
	}
	cfg = Default()
	cfg.Executor = Executor{RunnerUser: "ci-runner"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() accepted a runner user with no backend")
	}
}

// TestTheDeployedHostProcessExampleValidates keeps deploy/macos-host-runner's
// example node file honest: it is what an operator copies, so it must be a file
// this release accepts.
func TestTheDeployedHostProcessExampleValidates(t *testing.T) {
	file, err := os.Open("../../deploy/macos-host-runner/fleet.host-sim.example.json")
	if err != nil {
		t.Fatalf("open example: %v", err)
	}
	defer file.Close()
	cfg, err := Decode(file)
	if err != nil {
		t.Fatalf("Decode() = %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if !cfg.RunsHostProcesses() {
		t.Fatal("the host-process example does not name the hostprocess backend")
	}
}

// TestAHostProcessNodeFillsSlotsWithItsMacOSProfiles is the slot half of ADR
// 0090. Apple's two-guest limit is why a macOS profile never justified more
// than four slots; a host process is not a guest, so on a host-process node the
// macOS profiles are what fill the envelope, and a Tart node is unchanged.
func TestAHostProcessNodeFillsSlotsWithItsMacOSProfiles(t *testing.T) {
	cfg := hostProcessNode()
	cfg.Linux.MaxInstances = 5
	cfg.Linux.Capacity = Resources{CPU: 6, MemoryMiB: 14336}
	cfg.HostBudget = Resources{CPU: 6, MemoryMiB: 14336}
	cfg.MacOS.Maestro = Profile{ID: "host-sim-1x2", Label: "trf-macos-arm64-1x2", Resources: Resources{CPU: 1, MemoryMiB: 2048}, MaxActive: 5}
	cfg.MacOS.Builder = Profile{ID: "host-unexposed", Label: "trf-macos-arm64-2x4", Resources: Resources{CPU: 2, MemoryMiB: 4096}, MaxActive: 1}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a host-process node with five 1x2 slots was refused: %v", err)
	}
	cfg.Executor = Executor{}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "slots") {
		t.Fatalf("a Tart node justified five slots with macOS guests: %v", err)
	}
}
