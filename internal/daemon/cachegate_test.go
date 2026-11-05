package daemon

import (
	"context"
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
	if gate := newCacheGate(context.Background(), config.Config{}, health, time.Now); gate != nil {
		t.Fatalf("gate = %v", gate)
	}
	snapshot := health.Snapshot()
	if !snapshot.CacheGatePublished || snapshot.CacheGate.URL != "" {
		t.Fatalf("%+v", snapshot.CacheGate)
	}
	bootstrapper := lifecycle.StdinBootstrapper{}
	if got := withCacheGate(bootstrapper, nil, nil).(lifecycle.StdinBootstrapper); got.Gate != nil || got.NPM != nil {
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
	gate := newCacheGate(context.Background(), cfg, health, func() time.Time { return at })
	snapshot := health.Snapshot()
	if !snapshot.CacheGatePublished || snapshot.CacheGate.Verified || !snapshot.CacheGate.ProbedAt.Equal(at) ||
		snapshot.CacheGate.URL != cfg.CacheGate.URL {
		t.Fatalf("%+v", snapshot.CacheGate)
	}
	if gate.Verified(context.Background()) {
		t.Fatal("verified")
	}
	if got := withCacheGate(lifecycle.StdinBootstrapper{}, gate, nil).(lifecycle.StdinBootstrapper); got.Gate == nil || got.NPM != nil {
		t.Fatal("the bootstrapper did not get the gate")
	}
}

// TestAnUnparseableGateFailsClosed: only reachable past Validate by a bug, and it
// must still read as not ours.
func TestAnUnparseableGateFailsClosed(t *testing.T) {
	cfg := config.Config{CacheGate: config.CacheGate{URL: "http://bad host/", PublicKey: "bad"}}
	if newCacheGate(context.Background(), cfg, gateTestHealth(t), time.Now).Verified(context.Background()) {
		t.Fatal("verified")
	}
}

type otherBootstrapper struct{ lifecycle.Bootstrapper }

func TestANodeWithNoNPMRegistryGetsNoRoute(t *testing.T) {
	health := gateTestHealth(t)
	cfg := config.Config{CacheGate: config.CacheGate{URL: "http://127.0.0.1:1/", PublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}}
	if route := newNPMRoute(context.Background(), cfg, health, time.Now); route != nil {
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
	gate := newCacheGate(context.Background(), cfg, health, func() time.Time { return at })
	route := newNPMRoute(context.Background(), cfg, health, func() time.Time { return at })
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
	got := withCacheGate(lifecycle.StdinBootstrapper{ContainerGuest: true}, gate, route).(lifecycle.StdinBootstrapper)
	if got.Gate == nil || got.NPM != route || !got.ContainerGuest {
		t.Fatalf("%+v", got)
	}
}

func TestATartRegistryIsNotAContainerGate(t *testing.T) {
	const registry = "http://192.168.1.64:3017/npm/"
	cfg := config.Config{CacheGate: config.CacheGate{URL: "http://127.0.0.1:1/",
		PublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", NPMRegistry: registry}}
	health := gateTestHealth(t)
	route := newNPMRoute(context.Background(), cfg, health, time.Now)
	if route == nil || route.Host != "192.168.1.64" {
		t.Fatalf("route = %+v", route)
	}
	if health.Snapshot().CacheGate.Container {
		t.Fatal("a Tart node read as a container node")
	}
}

func TestAnotherBootstrapperIsLeftAlone(t *testing.T) {
	other := otherBootstrapper{}
	if got := withCacheGate(other, publishedGate{}, nil); got != lifecycle.Bootstrapper(other) {
		t.Fatalf("got %T", got)
	}
}
