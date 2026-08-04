package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/linux"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/macos"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/noexecutor"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/tart"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
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
	if !node.executes {
		t.Fatal("an Apple node has an execution technology")
	}
	cfg := config.Default()
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
	if node.bootstrap(cfg) == nil {
		t.Error("nil bootstrapper")
	}
}

// TestNonAppleNodesObserveFromProcWithNoBackend is Phase 1 Part A of
// docs/MULTI_NODE_PLAN.md expressed as wiring: a real daemon, measuring a real
// machine from /proc, with nothing to provision onto until issue #139 lands the
// container adapter.
func TestNonAppleNodesObserveFromProcWithNoBackend(t *testing.T) {
	for _, goos := range []string{"linux", "freebsd"} {
		node := platformFor(goos)
		if node.executes {
			t.Fatalf("%s claimed an execution technology it does not have", goos)
		}
		cfg := config.Default()
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
		if err := node.bootstrap(cfg).Bootstrap(context.Background(), "trf-small-1", nil); !errors.Is(err, noexecutor.ErrNoBackend) {
			t.Errorf("%s bootstrap = %v", goos, err)
		}
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
	production, ok := newDependencies("linux").inventory(nil, config.Default(), nil).(app.ProductionInventory)
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
	d.executes = false
	refused := []options{
		{Mode: reconcile.Shadow, ConfigPath: "unused"},
		{Mode: reconcile.Authority, ConfigPath: "unused"},
		{Mode: reconcile.Canary, CanaryScope: "fleet-repo", CanaryProfile: "small", ConfigPath: "unused"},
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

// TestDefaultDependenciesFollowTheRunningMachine ties the production entry point
// to the platform table.
func TestDefaultDependenciesFollowTheRunningMachine(t *testing.T) {
	if executes := defaultDependencies().executes; executes != (runtime.GOOS == "darwin") {
		t.Fatalf("%s node executes = %v", runtime.GOOS, executes)
	}
}
