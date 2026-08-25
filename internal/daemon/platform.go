package daemon

import (
	"context"
	"strings"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/linux"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/macos"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/noexecutor"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/podman"
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
//
// The operating system does not decide alone. On Linux the backend is a property
// of the node's configuration, because the same kernel serves both stages of the
// ADR 0034 bring-up: a machine with no `executor` block is Phase 1 Part A —
// a real daemon measuring a real machine from `/proc`, with nothing to
// provision onto — and a machine with `executor.backend: podman` is the
// authority node of issue #139. Every constructor below therefore takes the
// decoded configuration, and `executes` is a question asked of it.

// platform is the half of the daemon's wiring that a node's execution
// technology decides.
type platform struct {
	// executes reports whether this node can bring an instance into existence.
	// A node that cannot may run in observe mode and in no other, because every
	// other mode exists to mutate something.
	executes func(config.Config) bool
	// preflight refuses to start a node whose execution technology is configured
	// but unusable. It runs once, before any mode above observe, so an absent or
	// root-ful podman is an error an operator reads at startup instead of a queue
	// of parked dead letters they read an hour later.
	preflight func(context.Context, config.Config) error
	host      func(config.Config) executor.HostProbe
	executor  func(config.Config) app.ExecutorInventory
	newVM     func(runtimeStore, config.Config, lifecycle.DrainControl) lifecycle.VMControl
	newReaper func(runtimeStore, config.Config) discharge.VM
	readiness func(config.Config) lifecycle.Readiness
	bootstrap func(config.Config) lifecycle.Bootstrapper
	// linuxImage is what a Linux instance is made from on this node: a Tart base
	// VM on node A, an OCI reference on node B. It is the one dimension of
	// executor.InstanceSpec whose meaning is a property of the backend.
	linuxImage func(config.Config) string
	// guestProbe asks a running guest whether it is still executing anything. It
	// is the same command runner readiness polls, under a much shorter deadline,
	// because the question is not "has it booted yet" but "is it still there"
	// (ADR 0040).
	guestProbe func(config.Config) app.GuestProbe
}

// platformFor reports the wiring goos implies. macOS is Tart and the `vm_stat`
// host probe, unchanged. Every other platform is ADR 0034's second node, whose
// backend its own configuration names.
func platformFor(goos string) platform {
	if goos == "darwin" {
		return applePlatform()
	}
	return linuxPlatform()
}

func applePlatform() platform {
	return platform{
		executes: func(config.Config) bool { return true },
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
				LinuxNestedVirtualization: cfg.Linux.NestedVirtualization,
				LinuxSerialLogDirectory:   cfg.Linux.SerialLogDirectory}
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
		linuxImage: func(cfg config.Config) string { return cfg.Linux.BaseVM },
		guestProbe: func(cfg config.Config) app.GuestProbe {
			return execGuestProbe{Runner: tart.ExecRunner{}, Timeout: cfg.GuestLiveness.ProbeTimeout}
		},
	}
}

// linuxPlatform is node B in both of its stages. The host probe is `/proc` in
// either, because measuring the machine never depended on being able to act on
// it; everything else follows the configured backend, and a node that names none
// wires `noexecutor` exactly as issue #138 left it.
func linuxPlatform() platform {
	return platform{
		executes:  runsContainers,
		preflight: podmanPreflight,
		host: func(cfg config.Config) executor.HostProbe {
			return &linux.Probe{Timeout: cfg.Timeouts.Tart}
		},
		executor: func(cfg config.Config) app.ExecutorInventory {
			return containerBackend(nil, cfg, nil)
		},
		newVM: func(store runtimeStore, cfg config.Config, control lifecycle.DrainControl) lifecycle.VMControl {
			return containerBackend(store, cfg, control)
		},
		// The reaper omits Confirmation for the same reason node A's does: Reap
		// exists for a registration that can never be confirmed released.
		newReaper: func(store runtimeStore, cfg config.Config) discharge.VM {
			return containerBackend(store, cfg, nil)
		},
		readiness: func(cfg config.Config) lifecycle.Readiness {
			if !runsContainers(cfg) {
				return unreachableGuest{}
			}
			// `podman exec <name> true` is the same argument vector `tart exec` takes,
			// which is why issue #137 typed the probe on executor.CommandRunner and
			// why a container node needs no second copy of the poll loop.
			return execReadiness{Runner: podman.ExecRunner{Binary: cfg.Executor.Binary}, Timeout: cfg.Timeouts.Boot,
				AttemptTimeout: cfg.Timeouts.Tart, RetryInterval: 250 * time.Millisecond, After: time.After}
		},
		bootstrap: func(cfg config.Config) lifecycle.Bootstrapper {
			if !runsContainers(cfg) {
				return unreachableGuest{}
			}
			// The JIT configuration is piped into `podman exec -i <name> <helper>`
			// on stdin and never appears in argv or environment, exactly as it is
			// piped into `tart exec` on node A.
			//
			// What does differ is the guest: a container has no init system to place
			// the runner under and no machine to power off, so this node tells the
			// helper so. Nothing else can. The guest cannot answer the question from
			// its own filesystem — an image that installed the systemd package has
			// both binaries and still no PID 1 to use them (issue #273) — and no
			// layer between here and the guest is allowed to know what backend made
			// it (ADR 0034).
			return lifecycle.StdinBootstrapper{Runner: lifecycle.ExecStdinRunner{Binary: podmanBinary(cfg)},
				Timeout: cfg.Timeouts.Tart, ContainerGuest: true}
		},
		linuxImage: func(cfg config.Config) string {
			if !runsContainers(cfg) {
				return cfg.Linux.BaseVM
			}
			return cfg.Executor.Image
		},
		// A node with no execution technology has no guest to probe, and reporting
		// an unprobed guest as a refusing one would be an unavailable observation
		// dressed as a measurement (AGENTS.md §4).
		guestProbe: func(cfg config.Config) app.GuestProbe {
			if !runsContainers(cfg) {
				return nil
			}
			return execGuestProbe{Runner: podman.ExecRunner{Binary: cfg.Executor.Binary},
				Timeout: cfg.GuestLiveness.ProbeTimeout}
		},
	}
}

// runsContainers reports whether this node's configuration names a container
// backend. It is the whole of the Phase 1 / Phase 2 distinction.
func runsContainers(cfg config.Config) bool {
	return cfg.Executor.Backend == config.ExecutorPodman
}

func podmanBinary(cfg config.Config) string {
	if cfg.Executor.Binary == "" {
		return podman.DefaultBinary
	}
	return cfg.Executor.Binary
}

// containerBackend builds the node's one backend. A store and a drain control
// are supplied by the callers that have them; the inventory and the reaper pass
// nil, which is exactly what the Tart wiring above does with the same fields.
func containerBackend(store runtimeStore, cfg config.Config, control lifecycle.DrainControl) executor.Backend {
	if !runsContainers(cfg) {
		return noexecutor.Backend{}
	}
	return newPodmanAdapter(store, cfg, control)
}

func newPodmanAdapter(store runtimeStore, cfg config.Config, control lifecycle.DrainControl) *podman.Adapter {
	return &podman.Adapter{Runner: podman.ExecRunner{Binary: cfg.Executor.Binary},
		Ownership: store, Confirmation: control,
		Image: cfg.Executor.Image, HoldCommand: cfg.Executor.HoldCommand,
		KVMInstancePrefixes: kvmInstancePrefixes(cfg),
		CommandTimeout:      cfg.Timeouts.Tart, StopTimeout: containerStopGrace,
		ConfirmationMaxAge: deletionConfirmationMaxAge}
}

// kvmInstancePrefixes turns the configured profile IDs into the instance-name
// prefixes `reconcile.Controller` mints, which is how the adapter recognises the
// one profile ADR 0034 grants `/dev/kvm`.
func kvmInstancePrefixes(cfg config.Config) []string {
	prefixes := make([]string, 0, len(cfg.Executor.KVMProfiles))
	for _, profile := range cfg.Executor.KVMProfiles {
		prefixes = append(prefixes, "trf-"+strings.ToLower(profile)+"-")
	}
	return prefixes
}

// podmanPreflight is the fail-closed gate of issue #139. A node with no
// container backend has nothing to check; a node with one must prove podman is
// installed and rootless before it is allowed to promise GitHub a runner.
func podmanPreflight(ctx context.Context, cfg config.Config) error {
	if !runsContainers(cfg) {
		return nil
	}
	return newPodmanAdapter(nil, cfg, nil).Healthy(ctx)
}

// unreachableGuest is the readiness probe and the JIT bootstrapper of a node
// with no execution technology. Neither can ever be called — nothing gets past
// Create — and both refuse rather than block, so a wiring mistake surfaces as a
// failed operation instead of a provisioning path that waits out its deadline.
type unreachableGuest struct{}

func (unreachableGuest) Wait(context.Context, operations.Instance) error {
	return noexecutor.ErrNoBackend
}

func (unreachableGuest) Bootstrap(context.Context, string, *githubscaleset.JITSecret, []string) error {
	return noexecutor.ErrNoBackend
}
