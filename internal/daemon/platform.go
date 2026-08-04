package daemon

import (
	"context"
	"strings"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/linux"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/macos"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/noexecutor"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/tart"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/discharge"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/executor"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/lifecycle"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// This file is the one place in the fleet that names a backend adapter, which is
// what ADR 0034 and the `lifecycle-never-names-a-backend` depguard rule reserve
// `internal/daemon` for. Everything else above `internal/executor` is written
// against the port and does not know which machine it is running on.
//
// The choice is made from `runtime.GOOS` rather than from build constraints on
// purpose. CI runs the whole suite on Linux and cross-compiles the macOS release
// from it, so a `//go:build darwin` file would be a production path that no gate
// ever compiles, let alone covers. A runtime switch over a pure function keeps
// both nodes' wiring inside `make ci` on either machine.

// platform is the half of the daemon's wiring that a node's execution
// technology decides.
type platform struct {
	// executes reports whether this node can bring an instance into existence.
	// A node that cannot may run in observe mode and in no other, because every
	// other mode exists to mutate something.
	executes  bool
	host      func(config.Config) executor.HostProbe
	executor  func(config.Config) app.ExecutorInventory
	newVM     func(runtimeStore, config.Config, lifecycle.DrainControl) lifecycle.VMControl
	newReaper func(runtimeStore, config.Config) discharge.VM
	readiness func(config.Config) lifecycle.Readiness
	bootstrap func(config.Config) lifecycle.Bootstrapper
}

// platformFor reports the wiring goos implies. macOS is Tart and the `vm_stat`
// host probe, unchanged. Every other platform is ADR 0034's second node before
// issue #139 lands its container adapter: a real daemon, measuring a real
// machine from `/proc`, with nothing to provision onto.
func platformFor(goos string) platform {
	if goos == "darwin" {
		return applePlatform()
	}
	return observeOnlyPlatform()
}

func applePlatform() platform {
	return platform{
		executes: true,
		host: func(cfg config.Config) executor.HostProbe {
			return &macos.Probe{Timeout: cfg.Timeouts.Tart, PressureAccounting: cfg.Guards.PressureMemoryAccounting}
		},
		executor: func(cfg config.Config) app.ExecutorInventory {
			return &tart.Adapter{CommandTimeout: cfg.Timeouts.Tart, StartTimeout: cfg.Timeouts.Boot}
		},
		newVM: func(store runtimeStore, cfg config.Config, control lifecycle.DrainControl) lifecycle.VMControl {
			return &tart.Adapter{Ownership: store, Confirmation: control, CommandTimeout: cfg.Timeouts.Tart,
				StartTimeout: cfg.Timeouts.Boot, ConfirmationMaxAge: deletionConfirmationMaxAge,
				MacOSVMPrefixes:      []string{"trf-" + strings.ToLower(cfg.MacOS.Builder.ID) + "-", "trf-" + strings.ToLower(cfg.MacOS.Maestro.ID) + "-"},
				MacOSRootDiskOptions: cfg.MacOS.RootDiskOptions, MacOSSharedDirectoryPath: cfg.MacOS.SharedDirectoryPath,
				LinuxNestedVirtualization: cfg.Linux.NestedVirtualization}
		},
		// newReaper builds the discharge path's own backend port. It deliberately
		// omits Confirmation: Reap does not consult GitHub runner evidence, because
		// the case it exists for is a registration that can never be confirmed
		// released.
		newReaper: func(store runtimeStore, cfg config.Config) discharge.VM {
			return &tart.Adapter{Ownership: store, CommandTimeout: cfg.Timeouts.Tart, StartTimeout: cfg.Timeouts.Boot}
		},
		readiness: func(cfg config.Config) lifecycle.Readiness {
			return execReadiness{Runner: tart.ExecRunner{}, Timeout: cfg.Timeouts.Boot,
				AttemptTimeout: cfg.Timeouts.Tart, RetryInterval: 250 * time.Millisecond, After: time.After}
		},
		bootstrap: func(cfg config.Config) lifecycle.Bootstrapper {
			return lifecycle.StdinBootstrapper{Runner: lifecycle.ExecStdinRunner{Binary: "tart"}, Timeout: cfg.Timeouts.Tart}
		},
	}
}

func observeOnlyPlatform() platform {
	return platform{
		executes: false,
		host: func(cfg config.Config) executor.HostProbe {
			return &linux.Probe{Timeout: cfg.Timeouts.Tart}
		},
		executor: func(config.Config) app.ExecutorInventory { return noexecutor.Backend{} },
		newVM: func(runtimeStore, config.Config, lifecycle.DrainControl) lifecycle.VMControl {
			return noexecutor.Backend{}
		},
		newReaper: func(runtimeStore, config.Config) discharge.VM { return noexecutor.Backend{} },
		readiness: func(config.Config) lifecycle.Readiness { return unreachableGuest{} },
		bootstrap: func(config.Config) lifecycle.Bootstrapper { return unreachableGuest{} },
	}
}

// unreachableGuest is the readiness probe and the JIT bootstrapper of a node
// with no execution technology. Neither can ever be called — nothing gets past
// Create — and both refuse rather than block, so a wiring mistake surfaces as a
// failed operation instead of a provisioning path that waits out its deadline.
type unreachableGuest struct{}

func (unreachableGuest) Wait(context.Context, operations.Instance) error {
	return noexecutor.ErrNoBackend
}

func (unreachableGuest) Bootstrap(context.Context, string, *githubscaleset.JITSecret) error {
	return noexecutor.ErrNoBackend
}
