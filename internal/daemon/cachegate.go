package daemon

import (
	"context"
	"net/url"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/cachegate"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/lifecycle"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/telemetry"
)

// publishedGate asks the gate and tells the doctor row what it answered.
type publishedGate struct {
	verifier lifecycle.GateVerifier
	url      string
	health   *telemetry.Health
	now      func() time.Time
	// container says this node's guests are containers, for which a failed proof
	// rewrites nothing and changes only the npm setting.
	container bool
}

func (g publishedGate) Verified(ctx context.Context) bool {
	verified := g.verifier.Verified(ctx)
	g.health.SetCacheGate(telemetry.CacheGateMetric{URL: g.url, Verified: verified, ProbedAt: g.now(), Container: g.container})
	return verified
}

// publishedNPM asks the gate's npm route and tells the doctor row what it answered.
type publishedNPM struct {
	verifier cachegate.Verifier
	registry string
	health   *telemetry.Health
	now      func() time.Time
}

func (n publishedNPM) Verified(ctx context.Context) bool {
	ready := n.verifier.NPMReady(ctx)
	n.health.SetCacheGateNPM(n.registry, ready, n.now())
	return ready
}

// gateAddress reads a value Validate accepted. One that somehow fails verifies nothing.
func gateAddress(raw string) *url.URL {
	gate, _ := url.Parse(raw)
	if gate == nil {
		return &url.URL{}
	}
	return gate
}

// newCacheGate is nil when the node configures no gate. Otherwise it asks once at
// once, so the doctor row is true before the first guest launches. A value Validate
// accepted cannot fail to parse, and one that somehow did verifies nothing.
func newCacheGate(ctx context.Context, cfg config.Config, health *telemetry.Health, now func() time.Time) lifecycle.GateVerifier {
	if cfg.CacheGate == (config.CacheGate{}) {
		health.SetCacheGate(telemetry.CacheGateMetric{})
		return nil
	}
	public, _ := cachegate.ParsePublicKey(cfg.CacheGate.PublicKey)
	published := publishedGate{verifier: cachegate.Verifier{Gate: gateAddress(cfg.CacheGate.URL), Public: public}, url: cfg.CacheGate.URL,
		health: health, now: now, container: cfg.CacheGate.NPMHost() == cachegate.NPMContainerHost}
	published.Verified(ctx)
	return published
}

// newNPMRoute is nil when the node gives its guests no registry, which is the
// default. Otherwise it asks once at once, for the same reason newCacheGate does.
func newNPMRoute(ctx context.Context, cfg config.Config, health *telemetry.Health, now func() time.Time) *lifecycle.NPMRoute {
	if cfg.CacheGate.NPMRegistry == "" {
		return nil
	}
	ready := publishedNPM{verifier: cachegate.Verifier{Gate: gateAddress(cfg.CacheGate.URL)}, registry: cfg.CacheGate.NPMRegistry,
		health: health, now: now}
	ready.Verified(ctx)
	return &lifecycle.NPMRoute{Registry: cfg.CacheGate.NPMRegistry, Host: cfg.CacheGate.NPMHost(), Ready: ready}
}

// withCacheGate gives a node's bootstrapper the gate to ask and the npm route to use.
// Tart and podman guests take the same path, since both start through `exec`.
func withCacheGate(bootstrapper lifecycle.Bootstrapper, gate lifecycle.GateVerifier, npm *lifecycle.NPMRoute) lifecycle.Bootstrapper {
	stdin, ok := bootstrapper.(lifecycle.StdinBootstrapper)
	if !ok || gate == nil {
		return bootstrapper
	}
	stdin.Gate, stdin.NPM = gate, npm
	return stdin
}
