package telemetry

import (
	"fmt"
	"time"
)

// CacheGateMetric is the last answer to "is the LAN cache gate really ours?"
// (ADR 0078). An empty URL means this node configures no gate.
type CacheGateMetric struct {
	URL      string
	Verified bool
	ProbedAt time.Time
}

// SetCacheGate publishes the latest verification.
func (h *Health) SetCacheGate(metric CacheGateMetric) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.revision++
	h.cacheGate = metric
	h.cacheGatePublished = true
}

// cacheGateResult judges the published answer, or returns nil when nothing has
// been published: a daemon that has not looked has no verdict.
func cacheGateResult(snapshot Snapshot) *HealthResult {
	if !snapshot.CacheGatePublished {
		return nil
	}
	gate := snapshot.CacheGate
	if gate.URL == "" || gate.Verified {
		return &HealthResult{OK: true, Reasons: []string{}}
	}
	return &HealthResult{OK: false, Reasons: []string{fmt.Sprintf(
		"cache gate unreachable: %s did not prove its identity at %s, so guests launched since use GitHub's results service",
		gate.URL, gate.ProbedAt.UTC().Format(time.RFC3339))}}
}
