package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/hostprocess"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/macos"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/tart"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/lifecycle"
)

// hostProcessNodeConfig is the second daemon instance on a Mac (ADR 0090).
func hostProcessNodeConfig() config.Config {
	cfg := config.Default()
	cfg.Linux.Profiles, cfg.Linux.BaseVM = nil, ""
	cfg.Executor = config.Executor{Backend: config.ExecutorHostProcess,
		Image: "/Users/ci-runner/actions-runner-template", RunnerUser: "ci-runner"}
	return cfg
}

// TestAHostProcessNodeWiresTheLauncherEverywhere is the host-process node's
// acceptance criterion at the wiring level: on the same Apple platform, a file
// naming the hostprocess backend changes every constructor together, and the
// host probe — which measures the Mac, not the backend — stays the macOS one.
func TestAHostProcessNodeWiresTheLauncherEverywhere(t *testing.T) {
	node := platformFor("darwin")
	cfg := hostProcessNodeConfig()
	want := hostprocess.Runner{User: "ci-runner", Launcher: config.DefaultHostLauncher}
	if !node.executes(cfg) {
		t.Fatal("a host-process node cannot execute")
	}
	if node.macOSImage(cfg) != cfg.Executor.Image {
		t.Errorf("macOS image = %q, want the runner template", node.macOSImage(cfg))
	}
	if node.linuxImage(cfg) != cfg.Linux.BaseVM {
		t.Errorf("linux image = %q", node.linuxImage(cfg))
	}
	if _, ok := node.host(cfg).(*macos.Probe); !ok {
		t.Errorf("host probe = %T", node.host(cfg))
	}
	if _, ok := node.executor(cfg).(*hostprocess.Adapter); !ok {
		t.Errorf("executor inventory = %T", node.executor(cfg))
	}
	if adapter, ok := node.newVM(nil, cfg, nil).(*hostprocess.Adapter); !ok || adapter.Runner != want {
		t.Errorf("vm control = %#v", node.newVM(nil, cfg, nil))
	}
	if _, ok := node.newReaper(nil, cfg).(*hostprocess.Adapter); !ok {
		t.Errorf("reaper = %T", node.newReaper(nil, cfg))
	}
	if readiness, ok := node.readiness(cfg).(execReadiness); !ok || readiness.Runner != want {
		t.Errorf("readiness = %#v", node.readiness(cfg))
	}
	bootstrapper, ok := node.bootstrap(cfg).(lifecycle.StdinBootstrapper)
	if !ok || bootstrapper.Runner != (hostprocess.StdinRunner{Runner: want}) || bootstrapper.ContainerGuest {
		t.Errorf("bootstrapper = %#v", node.bootstrap(cfg))
	}
	if probe, ok := node.guestProbe(cfg).(execGuestProbe); !ok || probe.Runner != want {
		t.Errorf("guest probe = %#v", node.guestProbe(cfg))
	}
	// A host process has no guest filesystem to sample traffic from (ADR 0080).
	if node.guestExec(cfg) != nil {
		t.Errorf("guest exec = %#v, want none", node.guestExec(cfg))
	}
}

// TestATartNodeOnTheSameMacIsUnchanged keeps the first daemon instance exactly
// as it was: the dispatch must not leak the host-process wiring into it.
func TestATartNodeOnTheSameMacIsUnchanged(t *testing.T) {
	node := platformFor("darwin")
	cfg := config.Default()
	if node.macOSImage(cfg) != cfg.MacOS.BaseVM {
		t.Errorf("macOS image = %q, want the Tart base VM", node.macOSImage(cfg))
	}
	if err := node.preflight(context.Background(), cfg); err != nil {
		t.Errorf("a Tart node preflighted a backend it does not have: %v", err)
	}
	if _, ok := node.newVM(nil, cfg, nil).(*tart.Adapter); !ok {
		t.Errorf("vm control = %T", node.newVM(nil, cfg, nil))
	}
	if node.guestExec(cfg) == nil {
		t.Error("a Tart node lost its guest exec")
	}
	if platformFor("linux").macOSImage(cfg) != cfg.MacOS.BaseVM {
		t.Error("a Linux node invented a macOS image")
	}
}

// TestAHostProcessNodeRefusesToStartWhenSudoRefuses is the fail-closed
// preflight: a sudoers rule that does not admit the daemon is an error read at
// startup, not a queue of parked provisions.
func TestAHostProcessNodeRefusesToStartWhenSudoRefuses(t *testing.T) {
	refusing := filepath.Join(t.TempDir(), "sudo")
	if err := os.WriteFile(refusing, []byte("#!/bin/sh\necho 'sudo: a password is required' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hostSudo = hostprocess.SudoPath })
	hostSudo = refusing
	if err := platformFor("darwin").preflight(context.Background(), hostProcessNodeConfig()); err == nil {
		t.Fatal("a host-process node passed preflight while sudo refused it")
	}
}
