package config

import (
	"fmt"
	"net/url"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/cachegate"
)

// CacheGate is the LAN cache gate a node's Tart guests are baked to use and the
// public key it must prove itself with before each guest launch (ADR 0078). Both
// empty means no check: guests keep what their image says, so a release on a node
// nobody has configured sends nothing to the WAN.
//
// NPMRegistry is the optional address a guest uses for the gate's npm cache (ADR
// 0092). Empty, which is the default, means no guest is ever given a registry. It is
// what makes a podman node able to configure a gate at all: its guests' results URL
// is image ENV and needs no check, but a registry setting does.
type CacheGate struct {
	URL         string `json:"url"`
	PublicKey   string `json:"publicKey"`
	NPMRegistry string `json:"npmRegistry,omitempty"`
}

// NPMHost is the host of NPMRegistry, and empty when there is none.
func (g CacheGate) NPMHost() string {
	parsed, err := url.Parse(g.NPMRegistry)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}

func (c Config) validateCacheGate() error {
	if c.CacheGate == (CacheGate{}) {
		return nil
	}
	podman := c.Executor.Backend == ExecutorPodman
	if podman && c.CacheGate.NPMRegistry == "" {
		return fmt.Errorf("cacheGate: a podman node's containers reach their own gate through the host, " +
			"and have no baked address to verify; it configures a gate only to give them npmRegistry")
	}
	parsed, err := url.Parse(c.CacheGate.URL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("cacheGate.url %q must be an absolute http or https URL", c.CacheGate.URL)
	}
	if _, err := cachegate.ParsePublicKey(c.CacheGate.PublicKey); err != nil {
		return fmt.Errorf("cacheGate.publicKey: %w", err)
	}
	if c.CacheGate.NPMRegistry == "" {
		return nil
	}
	return validateNPMRegistry(parsed, c.CacheGate.NPMRegistry, podman)
}

// validateNPMRegistry holds a guest's registry setting to the direct LAN (ADR 0092).
// The gate the node proves must be plain HTTP at an address, never a hostname a tunnel
// could answer to, and the registry must be that same gate as this node's guests reach
// it: the same address on Tart, and pasta's name for the host on podman, whose guests
// cannot use the host's own LAN address.
func validateNPMRegistry(gate *url.URL, raw string, podman bool) error {
	host := gate.Hostname()
	direct := cachegate.DirectLANAddress(host) || (podman && cachegate.LoopbackAddress(host))
	if gate.Scheme != "http" || !direct {
		return fmt.Errorf("cacheGate.url %q must be plain http at a direct LAN address when npmRegistry is set, never a tunnel", gate)
	}
	registry, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("cacheGate.npmRegistry: %w", err)
	}
	want := host
	if podman {
		want = cachegate.NPMContainerHost
	}
	if registry.Scheme != "http" || registry.User != nil || registry.RawQuery != "" || registry.Fragment != "" ||
		registry.Path != cachegate.NPMPrefix || registry.Hostname() != want || registry.Port() == "" || registry.Port() != gate.Port() {
		return fmt.Errorf("cacheGate.npmRegistry %q must be exactly http://%s:%s%s", raw, want, gate.Port(), cachegate.NPMPrefix)
	}
	return nil
}

func decodeCacheGate(wire *CacheGate) CacheGate {
	if wire == nil {
		return CacheGate{}
	}
	return *wire
}

func encodeCacheGate(gate CacheGate) *CacheGate {
	if gate == (CacheGate{}) {
		return nil
	}
	return &gate
}
