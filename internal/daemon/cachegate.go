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
}

func (g publishedGate) Verified(ctx context.Context) bool {
	verified := g.verifier.Verified(ctx)
	g.health.SetCacheGate(telemetry.CacheGateMetric{URL: g.url, Verified: verified, ProbedAt: g.now()})
	return verified
}

// newCacheGate is nil when the node configures no gate. Otherwise it asks once at
// once, so the doctor row is true before the first guest launches. A value Validate
// accepted cannot fail to parse, and one that somehow did verifies nothing.
func newCacheGate(ctx context.Context, cfg config.Config, health *telemetry.Health, now func() time.Time) lifecycle.GateVerifier {
	if cfg.CacheGate == (config.CacheGate{}) {
		health.SetCacheGate(telemetry.CacheGateMetric{})
		return nil
	}
	gate, _ := url.Parse(cfg.CacheGate.URL)
	public, _ := cachegate.ParsePublicKey(cfg.CacheGate.PublicKey)
	if gate == nil {
		gate = &url.URL{}
	}
	published := publishedGate{verifier: cachegate.Verifier{Gate: gate, Public: public}, url: cfg.CacheGate.URL, health: health, now: now}
	published.Verified(ctx)
	return published
}

// withCacheGate gives a Tart node's bootstrapper the gate to ask. A podman node
// cannot configure one, and its bootstrapper is returned untouched.
func withCacheGate(bootstrapper lifecycle.Bootstrapper, gate lifecycle.GateVerifier) lifecycle.Bootstrapper {
	stdin, ok := bootstrapper.(lifecycle.StdinBootstrapper)
	if !ok || gate == nil {
		return bootstrapper
	}
	stdin.Gate = gate
	return stdin
}
