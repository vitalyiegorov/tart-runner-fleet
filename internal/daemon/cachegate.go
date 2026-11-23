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
	verifier cachegate.Verifier
	url      string
	health   *telemetry.Health
	now      func() time.Time
	// container says this node's guests are containers, for which a failed proof
	// rewrites nothing and changes only the npm setting.
	container bool
	// roaming is the node's own gate a guest tries when this one fails its proof.
	roaming string
	// warn records why a proof failed; nil records nothing (issue #537).
	warn func(string, ...any)
}

func (g publishedGate) Verified(ctx context.Context) bool {
	err := g.verifier.Prove(ctx)
	verified := err == nil
	if err != nil && g.warn != nil {
		g.warn("cache gate did not prove its identity", "gate", g.url, "error", err)
	}
	g.health.SetCacheGate(telemetry.CacheGateMetric{URL: g.url, Verified: verified, ProbedAt: g.now(), Container: g.container,
		Roaming: g.roaming})
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
//
// fetch is how this node reaches its gate, and nil means Go's own client.
func newCacheGate(ctx context.Context, cfg config.Config, health *telemetry.Health, now func() time.Time,
	fetch cachegate.Fetch, warn func(string, ...any)) lifecycle.GateVerifier {
	if cfg.CacheGate == (config.CacheGate{}) {
		health.SetCacheGate(telemetry.CacheGateMetric{})
		return nil
	}
	public, _ := cachegate.ParsePublicKey(cfg.CacheGate.PublicKey)
	published := publishedGate{verifier: cachegate.Verifier{Gate: gateAddress(cfg.CacheGate.URL), Public: public, Fetch: fetch},
		url: cfg.CacheGate.URL, health: health, now: now, container: cfg.CacheGate.NPMHost() == cachegate.NPMContainerHost,
		roaming: cfg.CacheGate.RoamingURL, warn: warn}
	published.Verified(ctx)
	return published
}

// newNPMRoute is nil when the node gives its guests no registry, which is the
// default. Otherwise it asks once at once, for the same reason newCacheGate does.
func newNPMRoute(ctx context.Context, cfg config.Config, health *telemetry.Health, now func() time.Time, fetch cachegate.Fetch) *lifecycle.NPMRoute {
	if cfg.CacheGate.NPMRegistry == "" {
		return nil
	}
	ready := publishedNPM{verifier: cachegate.Verifier{Gate: gateAddress(cfg.CacheGate.URL), Fetch: fetch}, registry: cfg.CacheGate.NPMRegistry,
		health: health, now: now}
	ready.Verified(ctx)
	return &lifecycle.NPMRoute{Registry: cfg.CacheGate.NPMRegistry, Host: cfg.CacheGate.NPMHost(), Ready: ready}
}

// newRoaming is nil when the node names no roaming gate, which is the default: a
// guest whose LAN gate does not prove itself then uses GitHub (ADR 0078).
func newRoaming(cfg config.Config) *lifecycle.Roaming {
	if cfg.CacheGate.RoamingURL == "" {
		return nil
	}
	return &lifecycle.Roaming{URL: cfg.CacheGate.RoamingURL, Host: cfg.CacheGate.RoamingHost()}
}

// withCacheGate gives a node's bootstrapper the gate to ask, the npm route to use and
// the roaming gate to fall back to. Tart and podman guests take the same path, since
// both start through `exec`.
func withCacheGate(bootstrapper lifecycle.Bootstrapper, gate lifecycle.GateVerifier, npm *lifecycle.NPMRoute,
	roaming *lifecycle.Roaming) lifecycle.Bootstrapper {
	stdin, ok := bootstrapper.(lifecycle.StdinBootstrapper)
	if !ok || gate == nil {
		return bootstrapper
	}
	stdin.Gate, stdin.NPM, stdin.Roaming = gate, npm, roaming
	return stdin
}

// appleCurl is the exempt client a macOS node proves its gate through (issue #537).
const appleCurl = "/usr/bin/curl"

// gateFetchFor is how a node on goos reaches its gate. Only macOS needs a client
// other than Go's own: Local Network privacy refuses an ad-hoc signed launchd
// agent's LAN connections, and an Apple platform binary is exempt.
func gateFetchFor(goos string) cachegate.Fetch {
	if goos != "darwin" {
		return nil
	}
	return cachegate.CurlFetch(appleCurl)
}
