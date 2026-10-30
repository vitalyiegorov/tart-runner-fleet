package daemon

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/hostprocess"
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
	// macOSImage is what a macOS instance is made from: the Tart base VM, or the
	// runner template directory a host-process node clones (ADR 0090).
	macOSImage func(config.Config) string
	// guestProbe asks a running guest whether it is still executing anything. It
	// is the same command runner readiness polls, under a much shorter deadline,
	// because the question is not "has it booted yet" but "is it still there"
	// (ADR 0040).
	guestProbe func(config.Config) app.GuestProbe
	// guestExec is the `exec <instance> ...` runner the guest probe and the
	// end-of-job traffic sample share, or nil on a node with no guest (ADR 0080).
	guestExec func(config.Config) executor.CommandRunner
}

// platformFor reports the wiring goos implies. macOS is Tart and the `vm_stat`
// host probe, unless the node's file names the hostprocess backend: then it is
// the second daemon instance on the Mac of ADR 0090, and every constructor but
// the host probe follows. Every other platform is ADR 0034's second node, whose
// backend its own configuration names.
func platformFor(goos string) platform {
	if goos == "darwin" {
		return eitherPlatform(config.Config.RunsHostProcesses, hostProcessPlatform(), applePlatform())
	}
	return linuxPlatform()
}

// eitherPlatform is the one place a configuration chooses between two wirings
// of one operating system, so no constructor can follow the file while another
// does not.
func eitherPlatform(choose func(config.Config) bool, chosen, otherwise platform) platform {
	pick := func(cfg config.Config) platform {
		if choose(cfg) {
			return chosen
		}
		return otherwise
	}
	return platform{
		executes:  func(cfg config.Config) bool { return pick(cfg).executes(cfg) },
		preflight: func(ctx context.Context, cfg config.Config) error { return pick(cfg).preflight(ctx, cfg) },
		host:      func(cfg config.Config) executor.HostProbe { return pick(cfg).host(cfg) },
		executor:  func(cfg config.Config) app.ExecutorInventory { return pick(cfg).executor(cfg) },
		newVM: func(store runtimeStore, cfg config.Config, control lifecycle.DrainControl) lifecycle.VMControl {
			return pick(cfg).newVM(store, cfg, control)
		},
		newReaper:  func(store runtimeStore, cfg config.Config) discharge.VM { return pick(cfg).newReaper(store, cfg) },
		readiness:  func(cfg config.Config) lifecycle.Readiness { return pick(cfg).readiness(cfg) },
		bootstrap:  func(cfg config.Config) lifecycle.Bootstrapper { return pick(cfg).bootstrap(cfg) },
		linuxImage: func(cfg config.Config) string { return pick(cfg).linuxImage(cfg) },
		macOSImage: func(cfg config.Config) string { return pick(cfg).macOSImage(cfg) },
		guestProbe: func(cfg config.Config) app.GuestProbe { return pick(cfg).guestProbe(cfg) },
		guestExec:  func(cfg config.Config) executor.CommandRunner { return pick(cfg).guestExec(cfg) },
	}
}

func appleHost(cfg config.Config) executor.HostProbe {
	return &macos.Probe{Timeout: cfg.Timeouts.Tart, PressureAccounting: cfg.Guards.PressureMemoryAccounting}
}

func macOSBaseVM(cfg config.Config) string { return cfg.MacOS.BaseVM }

// hostSudo is the sudo a host-process node invokes; a test points it at a stub.
var hostSudo = hostprocess.SudoPath

// hostRunner is the launcher behind sudo, as the runner user. It is the
// CommandRunner for every verb, the readiness poll, and the liveness probe.
func hostRunner(cfg config.Config) hostprocess.Runner {
	runner := hostprocess.Runner{User: cfg.Executor.RunnerUser, Launcher: cfg.HostLauncherPath()}
	if hostSudo != hostprocess.SudoPath {
		runner.Sudo = hostSudo
	}
	return runner
}

func newHostProcessAdapter(store runtimeStore, cfg config.Config, control lifecycle.DrainControl) *hostprocess.Adapter {
	return &hostprocess.Adapter{Runner: hostRunner(cfg), Ownership: store, Confirmation: control,
		CommandTimeout: cfg.Timeouts.Tart, StopTimeout: containerStopGrace, ConfirmationMaxAge: deletionConfirmationMaxAge}
}

// hostProcessPlatform is the second daemon instance on a Mac (ADR 0090). It
// measures the same machine the Tart instance does, with the same probe, and
// runs each job as a host process under the runner user. The JIT reaches the
// launcher on sudo's standard input exactly as it reaches a guest helper on
// `tart exec -i`'s, and the launcher accepts the same `exec` vectors, so the
// lifecycle's readiness, bootstrap, and liveness code is reused unchanged. A
// host process has no guest filesystem, so there is no traffic sample.
func hostProcessPlatform() platform {
	return platform{
		executes: func(config.Config) bool { return true },
		preflight: func(ctx context.Context, cfg config.Config) error {
			return newHostProcessAdapter(nil, cfg, nil).Healthy(ctx)
		},
		host:     appleHost,
		executor: func(cfg config.Config) app.ExecutorInventory { return newHostProcessAdapter(nil, cfg, nil) },
		newVM: func(store runtimeStore, cfg config.Config, control lifecycle.DrainControl) lifecycle.VMControl {
			return newHostProcessAdapter(store, cfg, control)
		},
		// The reaper omits Confirmation for the reason every other node's does.
		newReaper: func(store runtimeStore, cfg config.Config) discharge.VM {
			return newHostProcessAdapter(store, cfg, nil)
		},
		readiness: func(cfg config.Config) lifecycle.Readiness {
			return execReadiness{Runner: hostRunner(cfg), Timeout: cfg.Timeouts.Boot,
				AttemptTimeout: cfg.Timeouts.Tart, RetryInterval: 250 * time.Millisecond, After: time.After}
		},
		bootstrap: func(cfg config.Config) lifecycle.Bootstrapper {
			return lifecycle.StdinBootstrapper{Runner: hostprocess.StdinRunner{Runner: hostRunner(cfg)},
				Timeout: cfg.Timeouts.Tart}
		},
		linuxImage: func(cfg config.Config) string { return cfg.Linux.BaseVM },
		macOSImage: func(cfg config.Config) string { return cfg.Executor.Image },
		guestProbe: func(cfg config.Config) app.GuestProbe {
			return execGuestProbe{Runner: hostRunner(cfg), Timeout: cfg.GuestLiveness.ProbeTimeout}
		},
		guestExec: func(config.Config) executor.CommandRunner { return nil },
	}
}

func applePlatform() platform {
	return platform{
		executes:  func(config.Config) bool { return true },
		preflight: func(context.Context, config.Config) error { return nil },
		host:      appleHost,
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
		macOSImage: macOSBaseVM,
		guestProbe: func(cfg config.Config) app.GuestProbe {
			return execGuestProbe{Runner: tart.ExecRunner{}, Timeout: cfg.GuestLiveness.ProbeTimeout}
		},
		guestExec: func(config.Config) executor.CommandRunner { return tart.ExecRunner{} },
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
		macOSImage: macOSBaseVM,
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
		guestExec: func(cfg config.Config) executor.CommandRunner {
			if !runsContainers(cfg) {
				return nil
			}
			return podman.ExecRunner{Binary: cfg.Executor.Binary}
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
		EmulatorSlotsDir:    cfg.Executor.EmulatorSlotsDir,
		VectorViewDir:       vectorViewDir(cfg),
		CommandTimeout:      cfg.Timeouts.Tart, StopTimeout: containerStopGrace,
		ConfirmationMaxAge: deletionConfirmationMaxAge}
}

// vectorViewDir is where the narrowed `/proc` view a container is shown is
// materialised. A node whose state directory is unknown — every unit test that
// wires a backend from a bare configuration — gets the empty string, which
// disables the narrowing and leaves the container seeing the host exactly as it
// did before issue #291.
func vectorViewDir(cfg config.Config) string {
	if cfg.StateDir == "" {
		return ""
	}
	return filepath.Join(cfg.StateDir, "vectorview")
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
