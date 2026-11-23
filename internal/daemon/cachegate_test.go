package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/lifecycle"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/telemetry"
)

func gateTestHealth(t *testing.T) *telemetry.Health {
	t.Helper()
	health, err := telemetry.NewHealth(wallClock{}, telemetry.HealthConfig{})
	if err != nil {
		t.Fatal(err)
	}
	return health
}

func TestANodeWithNoGateGetsNoVerifierAndAKnownPosture(t *testing.T) {
	health := gateTestHealth(t)
	if gate := newCacheGate(context.Background(), config.Config{}, health, time.Now, nil, nil); gate != nil {
		t.Fatalf("gate = %v", gate)
	}
	snapshot := health.Snapshot()
	if !snapshot.CacheGatePublished || snapshot.CacheGate.URL != "" {
		t.Fatalf("%+v", snapshot.CacheGate)
	}
	bootstrapper := lifecycle.StdinBootstrapper{}
	if got := withCacheGate(bootstrapper, nil, nil, config.CacheGate{}).(lifecycle.StdinBootstrapper); got.Gate != nil || got.NPM != nil {
		t.Fatal("a node with no gate got one")
	}
}

// TestAnUnreachableGateIsPublishedAsUnverifiedBeforeAnyLaunch uses a port nothing
// listens on: the doctor row must be true before the first guest asks.
func TestAnUnreachableGateIsPublishedAsUnverifiedBeforeAnyLaunch(t *testing.T) {
	health := gateTestHealth(t)
	at := time.Unix(100, 0)
	cfg := config.Config{CacheGate: config.CacheGate{URL: "http://127.0.0.1:1/",
		PublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}}
	gate := newCacheGate(context.Background(), cfg, health, func() time.Time { return at }, nil, nil)
	snapshot := health.Snapshot()
	if !snapshot.CacheGatePublished || snapshot.CacheGate.Verified || !snapshot.CacheGate.ProbedAt.Equal(at) ||
		snapshot.CacheGate.URL != cfg.CacheGate.URL {
		t.Fatalf("%+v", snapshot.CacheGate)
	}
	if gate.Verified(context.Background()) {
		t.Fatal("verified")
	}
	if got := withCacheGate(lifecycle.StdinBootstrapper{}, gate, nil, config.CacheGate{}).(lifecycle.StdinBootstrapper); got.Gate == nil || got.NPM != nil {
		t.Fatal("the bootstrapper did not get the gate")
	}
}

// TestAnUnparseableGateFailsClosed: only reachable past Validate by a bug, and it
// must still read as not ours.
func TestAnUnparseableGateFailsClosed(t *testing.T) {
	cfg := config.Config{CacheGate: config.CacheGate{URL: "http://bad host/", PublicKey: "bad"}}
	if newCacheGate(context.Background(), cfg, gateTestHealth(t), time.Now, nil, nil).Verified(context.Background()) {
		t.Fatal("verified")
	}
}

type otherBootstrapper struct{ lifecycle.Bootstrapper }

func TestANodeWithNoNPMRegistryGetsNoRoute(t *testing.T) {
	health := gateTestHealth(t)
	cfg := config.Config{CacheGate: config.CacheGate{URL: "http://127.0.0.1:1/", PublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}}
	if route := newNPMRoute(context.Background(), cfg, health, time.Now, nil); route != nil {
		t.Fatalf("route = %+v", route)
	}
	if snapshot := health.Snapshot(); snapshot.CacheGate.NPMRegistry != "" {
		t.Fatalf("%+v", snapshot.CacheGate)
	}
}

// TestAnNPMRouteReachesBothKindsOfGuestAndPublishesItsAnswer: Omarchy's Podman guests
// and the Macs' Tart guests are the same path, unlike the results rewrite.
func TestAnNPMRouteReachesBothKindsOfGuestAndPublishesItsAnswer(t *testing.T) {
	at := time.Unix(100, 0)
	const registry = "http://host.containers.internal:3017/npm/"
	cfg := config.Config{CacheGate: config.CacheGate{URL: "http://127.0.0.1:1/",
		PublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", NPMRegistry: registry}}
	health := gateTestHealth(t)
	gate := newCacheGate(context.Background(), cfg, health, func() time.Time { return at }, nil, nil)
	route := newNPMRoute(context.Background(), cfg, health, func() time.Time { return at }, nil)
	if route == nil || route.Registry != registry || route.Host != "host.containers.internal" {
		t.Fatalf("route = %+v", route)
	}
	if route.Ready.Verified(context.Background()) {
		t.Fatal("a port nothing listens on read as ready")
	}
	snapshot := health.Snapshot()
	if snapshot.CacheGate.NPMRegistry != registry || snapshot.CacheGate.NPMReady || !snapshot.CacheGate.Container {
		t.Fatalf("%+v", snapshot.CacheGate)
	}
	got := withCacheGate(lifecycle.StdinBootstrapper{ContainerGuest: true}, gate, route, config.CacheGate{}).(lifecycle.StdinBootstrapper)
	if got.Gate == nil || got.NPM != route || !got.ContainerGuest {
		t.Fatalf("%+v", got)
	}
}

func TestATartRegistryIsNotAContainerGate(t *testing.T) {
	const registry = "http://192.168.1.64:3017/npm/"
	cfg := config.Config{CacheGate: config.CacheGate{URL: "http://127.0.0.1:1/",
		PublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", NPMRegistry: registry}}
	health := gateTestHealth(t)
	route := newNPMRoute(context.Background(), cfg, health, time.Now, nil)
	if route == nil || route.Host != "192.168.1.64" {
		t.Fatalf("route = %+v", route)
	}
	if health.Snapshot().CacheGate.Container {
		t.Fatal("a Tart node read as a container node")
	}
}

// TestARoamingGateReachesTheBootstrapper: a node that names its own forwarding gate
// hands it, with its LAN gate, to every launch.
func TestARoamingGateReachesTheBootstrapper(t *testing.T) {
	cfg := config.Config{CacheGate: config.CacheGate{URL: "http://192.168.1.64:3017/",
		PublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", RoamingURL: "http://192.168.64.1:3017"}}
	health := gateTestHealth(t)
	newCacheGate(context.Background(), cfg, health, time.Now, func(context.Context, string) (int, []byte, error) {
		return 0, nil, errors.New("off the LAN")
	}, nil)
	if got := health.Snapshot().CacheGate; got.Verified || got.Roaming != cfg.CacheGate.RoamingURL {
		t.Fatalf("%+v", got)
	}
	got := withCacheGate(lifecycle.StdinBootstrapper{}, publishedGate{}, nil, cfg.CacheGate).(lifecycle.StdinBootstrapper)
	if got.Roaming != cfg.CacheGate.RoamingURL || got.Home != cfg.CacheGate.URL {
		t.Fatalf("%+v", got)
	}
}

func TestAnotherBootstrapperIsLeftAlone(t *testing.T) {
	other := otherBootstrapper{}
	if got := withCacheGate(other, publishedGate{}, nil, config.CacheGate{}); got != lifecycle.Bootstrapper(other) {
		t.Fatalf("got %T", got)
	}
}

// TestAMacNodeProvesItsGateThroughAppleCurl is issue #537: macOS Local Network
// privacy answered the launchd agent's own proof with "no route to host" while the
// gate was healthy, so every Mac guest fell back to GitHub and lost its artifacts.
func TestAMacNodeProvesItsGateThroughAppleCurl(t *testing.T) {
	if newDependencies("darwin").gateFetch == nil {
		t.Fatal("a macOS node proves its gate through its own LAN connection")
	}
	if newDependencies("linux").gateFetch != nil {
		t.Fatal("a Linux node gained a curl dependency it does not need")
	}
}

// TestAFailedProofIsLoggedWithItsCause is issue #537: the doctor row said only that
// the gate did not prove itself, and nothing recorded why.
func TestAFailedProofIsLoggedWithItsCause(t *testing.T) {
	cfg := config.Config{CacheGate: config.CacheGate{URL: "http://127.0.0.1:1/",
		PublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}}
	var logged []any
	warn := func(_ string, args ...any) { logged = append(logged, args...) }
	refused := func(context.Context, string) (int, []byte, error) {
		return 0, nil, errors.New("connect: no route to host")
	}
	gate := newCacheGate(context.Background(), cfg, gateTestHealth(t), time.Now, refused, warn)
	if gate.Verified(context.Background()) {
		t.Fatal("verified")
	}
	if !strings.Contains(fmt.Sprint(logged...), "no route to host") {
		t.Fatalf("logged %v", logged)
	}
}
