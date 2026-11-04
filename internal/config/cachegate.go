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
type CacheGate struct {
	URL       string `json:"url"`
	PublicKey string `json:"publicKey"`
}

func (c Config) validateCacheGate() error {
	if c.CacheGate == (CacheGate{}) {
		return nil
	}
	if c.Executor.Backend == ExecutorPodman {
		return fmt.Errorf("cacheGate: a podman node's containers reach their own gate through the host, " +
			"and have no baked address to verify")
	}
	parsed, err := url.Parse(c.CacheGate.URL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("cacheGate.url %q must be an absolute http or https URL", c.CacheGate.URL)
	}
	if _, err := cachegate.ParsePublicKey(c.CacheGate.PublicKey); err != nil {
		return fmt.Errorf("cacheGate.publicKey: %w", err)
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
