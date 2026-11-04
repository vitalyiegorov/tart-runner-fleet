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
	if got := withCacheGate(bootstrapper, nil).(lifecycle.StdinBootstrapper); got.Gate != nil {
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
	if got := withCacheGate(lifecycle.StdinBootstrapper{}, gate).(lifecycle.StdinBootstrapper); got.Gate == nil {
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

func TestAnotherBootstrapperIsLeftAlone(t *testing.T) {
	other := otherBootstrapper{}
	if got := withCacheGate(other, publishedGate{}); got != lifecycle.Bootstrapper(other) {
		t.Fatalf("got %T", got)
	}
}
