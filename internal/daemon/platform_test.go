package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/linux"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/macos"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/noexecutor"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/podman"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/tart"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/lifecycle"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/reconcile"
)

// TestAppleNodesRunTartAndTheMacosProbe pins node A's wiring. It is asserted
// from an explicit `goos` rather than from the machine running the suite,
// because CI runs on Linux and cross-compiles the macOS release: without this,
// the whole production wiring of the live node would be compiled by every gate
// and executed by none.
func TestAppleNodesRunTartAndTheMacosProbe(t *testing.T) {
	node := platformFor("darwin")
	cfg := config.Default()
	if !node.executes(cfg) {
		t.Fatal("an Apple node has an execution technology")
	}
	if node.linuxImage(cfg) != cfg.Linux.BaseVM {
		t.Errorf("linux image = %q, want the Tart base VM", node.linuxImage(cfg))
	}
	if _, ok := node.host(cfg).(*macos.Probe); !ok {
		t.Errorf("host probe = %T", node.host(cfg))
	}
	if _, ok := node.executor(cfg).(*tart.Adapter); !ok {
		t.Errorf("executor inventory = %T", node.executor(cfg))
	}
	if _, ok := node.newVM(nil, cfg, nil).(*tart.Adapter); !ok {
		t.Errorf("vm control = %T", node.newVM(nil, cfg, nil))
	}
	if _, ok := node.newReaper(nil, cfg).(*tart.Adapter); !ok {
		t.Errorf("reaper = %T", node.newReaper(nil, cfg))
	}
	if _, ok := node.readiness(cfg).(execReadiness); !ok {
		t.Errorf("readiness = %T", node.readiness(cfg))
	}
	bootstrapper, ok := node.bootstrap(cfg).(lifecycle.StdinBootstrapper)
	if !ok {
		t.Fatalf("bootstrapper = %T", node.bootstrap(cfg))
	}
	// A Tart guest is a virtual machine: it has systemd and a power switch, and
	// ADR 0010's poweroff contract is unchanged for it (issue #273).
	if bootstrapper.ContainerGuest {
		t.Error("an Apple node's guests are VMs and must be bootstrapped as VMs")
	}
}

// TestNonAppleNodesObserveFromProcWithNoBackend is Phase 1 Part A of
// docs/MULTI_NODE_PLAN.md expressed as wiring: a real daemon, measuring a real
// machine from /proc, with nothing to provision onto because its configuration
// names no container backend.
func TestNonAppleNodesObserveFromProcWithNoBackend(t *testing.T) {
	for _, goos := range []string{"linux", "freebsd"} {
		node := platformFor(goos)
		cfg := config.Default()
		if node.executes(cfg) {
			t.Fatalf("%s claimed an execution technology it does not have", goos)
		}
		if err := node.preflight(context.Background(), cfg); err != nil {
			t.Errorf("%s preflighted a backend it has none of: %v", goos, err)
		}
		if node.linuxImage(cfg) != cfg.Linux.BaseVM {
			t.Errorf("%s linux image = %q", goos, node.linuxImage(cfg))
		}
		if _, ok := node.host(cfg).(*linux.Probe); !ok {
			t.Errorf("%s host probe = %T", goos, node.host(cfg))
		}
		if _, ok := node.executor(cfg).(noexecutor.Backend); !ok {
			t.Errorf("%s executor inventory = %T", goos, node.executor(cfg))
		}
		if _, ok := node.newVM(nil, cfg, nil).(noexecutor.Backend); !ok {
			t.Errorf("%s vm control = %T", goos, node.newVM(nil, cfg, nil))
		}
		if _, ok := node.newReaper(nil, cfg).(noexecutor.Backend); !ok {
			t.Errorf("%s reaper = %T", goos, node.newReaper(nil, cfg))
		}
		if err := node.readiness(cfg).Wait(context.Background(), operations.Instance{}); !errors.Is(err, noexecutor.ErrNoBackend) {
			t.Errorf("%s readiness = %v", goos, err)
		}
		if err := node.bootstrap(cfg).Bootstrap(context.Background(), "trf-small-1", nil, nil); !errors.Is(err, noexecutor.ErrNoBackend) {
			t.Errorf("%s bootstrap = %v", goos, err)
		}
	}
}

// containerNodeConfig is node B after Phase 2: the same Linux machine, with an
// `executor` block naming the runtime it now has.
func containerNodeConfig() config.Config {
	cfg := config.Default()
	cfg.Executor = config.Executor{Backend: config.ExecutorPodman,
		Image: "ghcr.io/vitalyiegorov/trf-runner-amd64:2026-08", Binary: "/usr/bin/podman",
		KVMProfiles: []string{"large"}}
	return cfg
}

// TestAConfiguredContainerNodeWiresPodmanEverywhere is issue #139's acceptance
// criterion at the wiring level: the same Linux platform that observed in Part A
// provisions in Part B, and every one of the five constructors changes together.
// A backend wired into four of five places is the failure this pins.
func TestAConfiguredContainerNodeWiresPodmanEverywhere(t *testing.T) {
	node := platformFor("linux")
	cfg := containerNodeConfig()
	if !node.executes(cfg) {
		t.Fatal("a node with a configured container runtime cannot execute")
	}
	if node.linuxImage(cfg) != cfg.Executor.Image {
		t.Errorf("linux image = %q, want the configured OCI reference", node.linuxImage(cfg))
	}
	if _, ok := node.host(cfg).(*linux.Probe); !ok {
		t.Errorf("host probe = %T; measuring the machine never depended on the backend", node.host(cfg))
	}
	if _, ok := node.executor(cfg).(*podman.Adapter); !ok {
		t.Errorf("executor inventory = %T", node.executor(cfg))
	}
	if _, ok := node.newReaper(nil, cfg).(*podman.Adapter); !ok {
		t.Errorf("reaper = %T", node.newReaper(nil, cfg))
	}
	if _, ok := node.readiness(cfg).(execReadiness); !ok {
		t.Errorf("readiness = %T", node.readiness(cfg))
	}
	bootstrapper, ok := node.bootstrap(cfg).(lifecycle.StdinBootstrapper)
	if !ok {
		t.Fatalf("bootstrapper = %T", node.bootstrap(cfg))
	}
	if runner, isExec := bootstrapper.Runner.(lifecycle.ExecStdinRunner); !isExec || runner.Binary != "/usr/bin/podman" {
		t.Errorf("JIT bootstrap runs %#v, want the configured podman binary", bootstrapper.Runner)
	}
	// Issue #273: this file is the one place that knows the guest is a container,
	// so it is the one place that can tell the guest helper (ADR 0010 amendment).
	if !bootstrapper.ContainerGuest {
		t.Error("a podman node's guests are containers and the bootstrap helper was not told so")
	}

	adapter, ok := node.newVM(nil, cfg, nil).(*podman.Adapter)
	if !ok {
		t.Fatalf("vm control = %T", node.newVM(nil, cfg, nil))
	}
	if adapter.Image != cfg.Executor.Image || adapter.CommandTimeout != cfg.Timeouts.Tart ||
		adapter.StopTimeout != containerStopGrace || adapter.ConfirmationMaxAge != deletionConfirmationMaxAge {
		t.Errorf("adapter = %#v", adapter)
	}
	// ADR 0034 grants /dev/kvm to the named profile alone, and the adapter reads
	// the profile off the instance-name prefix the scheduler mints.
	if !reflect.DeepEqual(adapter.KVMInstancePrefixes, []string{"trf-large-"}) {
		t.Errorf("kvm prefixes = %v, want the configured profile only", adapter.KVMInstancePrefixes)
	}
}

// TestAnUnusablePodmanRefusesToStartTheNode is the fail-closed half. The
// preflight shells the real binary, so a node configured with one that cannot
// exist must refuse rather than promise GitHub a runner.
func TestAnUnusablePodmanRefusesToStartTheNode(t *testing.T) {
	cfg := containerNodeConfig()
	cfg.Executor.Binary = filepath.Join(t.TempDir(), "podman-that-is-not-installed")
	if err := platformFor("linux").preflight(context.Background(), cfg); err == nil {
		t.Fatal("a node with no container runtime preflighted successfully")
	}
}

// TestPodmanBinaryFallsBackToThePath states the default an operator gets from a
// distribution package.
func TestPodmanBinaryFallsBackToThePath(t *testing.T) {
	cfg := containerNodeConfig()
	cfg.Executor.Binary = ""
	if got := podmanBinary(cfg); got != "podman" {
		t.Fatalf("podmanBinary() = %q", got)
	}
}

// noInstanceStore is a durable store holding no live instance, which is every
// state a node with no execution technology can be in.
type noInstanceStore struct{}

func (noInstanceStore) LiveInstances(context.Context) ([]operations.Instance, error) {
	return nil, nil
}

// TestObserveOnlyInventoryStillObservesTheMachine proves the two halves compose:
// the node reports no instances and, on Linux, a fresh reading of its own
// machine. That pair is what "observe steady state" means on a node with no
// backend, and it is the acceptance criterion of issue #138.
func TestObserveOnlyInventoryStillObservesTheMachine(t *testing.T) {
	production, ok := newDependencies("linux").inventory(nil, config.Default(), nil, nil).(app.ProductionInventory)
	if !ok {
		t.Fatal("production inventory adapter type changed")
	}
	production.Store = noInstanceStore{}
	instances, host := production.Observe(context.Background())
	if instances.State != domain.ObservationFresh || len(instances.Value) != 0 {
		t.Fatalf("instances = %#v", instances)
	}
	if runtime.GOOS == "linux" && host.State != domain.ObservationFresh {
		t.Fatalf("a Linux node did not observe its own machine: %#v", host)
	}
	if runtime.GOOS != "linux" && host.State != domain.ObservationUnavailable {
		t.Fatalf("%s reported a /proc observation it cannot have: %#v", runtime.GOOS, host)
	}
}

// TestOnlyObserveModeStartsWithoutAnExecutionBackend is the fail-closed guard.
// Every mode above observe exists to mutate a machine this node cannot act on,
// so the daemon refuses at startup rather than accumulating parked dead letters.
func TestOnlyObserveModeStartsWithoutAnExecutionBackend(t *testing.T) {
	d := testDependencies(t)
	d.executes = func(config.Config) bool { return false }
	configPath := writeConfig(t, true)
	refused := []options{
		{Mode: reconcile.Shadow, ConfigPath: configPath},
		{Mode: reconcile.Authority, ConfigPath: configPath},
		{Mode: reconcile.Canary, CanaryScope: "fleet-repo", CanaryProfile: "small", ConfigPath: configPath},
	}
	for _, opts := range refused {
		err := runWithDependencies(context.Background(), opts, d)
		if err == nil || !strings.Contains(err.Error(), "execution backend") {
			t.Errorf("mode %s started without a backend: %v", opts.Mode, err)
		}
	}

	// Observe passes the guard and then fails on the configuration it was
	// actually given, which proves the refusal is about the mode and not a
	// blanket rejection of every start.
	observeErr := runWithDependencies(context.Background(), options{Mode: reconcile.Observe,
		ConfigPath: filepath.Join(t.TempDir(), "absent.json")}, d)
	if observeErr == nil || strings.Contains(observeErr.Error(), "execution backend") {
		t.Errorf("observe was refused for lacking a backend: %v", observeErr)
	}
}

// TestAConfiguredBackendMustBeProvenPresentBeforeTheNodeStarts is the fail-closed
// gate of issue #139 at the daemon boundary. A node whose configuration names a
// container runtime, on a machine where that runtime is absent or unusable,
// refuses to start in any mutating mode -- rather than starting, advertising
// capacity to GitHub, and parking every job it is then handed.
func TestAConfiguredBackendMustBeProvenPresentBeforeTheNodeStarts(t *testing.T) {
	d := testDependencies(t)
	d.executes = func(config.Config) bool { return true }
	d.preflight = func(context.Context, config.Config) error { return errors.New("podman is not installed") }
	configPath := writeConfig(t, true)

	err := runWithDependencies(context.Background(), options{Mode: reconcile.Shadow, ConfigPath: configPath}, d)
	if err == nil || !strings.Contains(err.Error(), "execution backend preflight") {
		t.Fatalf("a node with an unusable backend started: %v", err)
	}

	// Observe mode never mutates anything, so it is not gated on the runtime
	// being usable: a node whose podman broke must still be able to report what
	// it can see.
	observeErr := runWithDependencies(context.Background(), options{Mode: reconcile.Observe,
		ConfigPath: filepath.Join(t.TempDir(), "absent.json")}, d)
	if observeErr == nil || strings.Contains(observeErr.Error(), "preflight") {
		t.Fatalf("observe was gated on a backend it never uses: %v", observeErr)
	}
}

// TestDefaultDependenciesFollowTheRunningMachine ties the production entry point
// to the platform table.
func TestDefaultDependenciesFollowTheRunningMachine(t *testing.T) {
	if executes := defaultDependencies().executes(config.Default()); executes != (runtime.GOOS == "darwin") {
		t.Fatalf("%s node executes = %v", runtime.GOOS, executes)
	}
	// A Linux machine that names a container runtime executes on any host, which
	// is what makes the second node's wiring reachable from a macOS development
	// machine and from the Linux CI runner alike.
	if !defaultDependencies().executes(containerNodeConfig()) && runtime.GOOS != "darwin" {
		t.Fatal("a configured container node did not execute")
	}
}

// The narrowed `/proc` view a container is shown lives in the node's own state
// directory (ADR 0050). A configuration whose origin is unknown — every unit
// test that wires a backend from a bare document — disables the narrowing rather
// than guessing a path, and its containers see the host exactly as they did
// before the record.
func TestTheVectorViewLivesInTheNodesStateDirectory(t *testing.T) {
	if got := vectorViewDir(config.Config{}); got != "" {
		t.Fatalf("an unknown state directory must disable the narrowing, got %q", got)
	}
	if got := vectorViewDir(config.Config{StateDir: "/var/lib/trf/state"}); got != "/var/lib/trf/state/vectorview" {
		t.Fatalf("vectorViewDir = %q", got)
	}
}
