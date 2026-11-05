package telemetry

import (
	"fmt"
	"time"
)

// CacheGateMetric is the last answer to "is the LAN cache gate really ours?"
// (ADR 0078). An empty URL means this node configures no gate. Container says its
// guests are containers, whose results URL no failed proof rewrites. ProbedAt is when the
// identity proof was last asked. An empty NPMRegistry means the node configures no npm
// cache (ADR 0093). Otherwise it is the registry a guest may be given, NPMReady is the
// host's latest answer from the gate's npm route, and NPMProbedAt is when it asked. The
// two probes run at different moments, so each keeps its own time.
type CacheGateMetric struct {
	URL         string
	Verified    bool
	ProbedAt    time.Time
	Container   bool
	NPMRegistry string
	NPMReady    bool
	NPMProbedAt time.Time
}

// SetCacheGate publishes the latest verification and keeps the npm route's answer,
// which SetCacheGateNPM publishes.
func (h *Health) SetCacheGate(metric CacheGateMetric) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.revision++
	metric.NPMRegistry, metric.NPMReady, metric.NPMProbedAt = h.cacheGate.NPMRegistry, h.cacheGate.NPMReady, h.cacheGate.NPMProbedAt
	h.cacheGate = metric
	h.cacheGatePublished = true
}

// SetCacheGateNPM publishes the latest answer from the gate's npm route.
func (h *Health) SetCacheGateNPM(registry string, ready bool, at time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.revision++
	h.cacheGate.NPMRegistry, h.cacheGate.NPMReady, h.cacheGate.NPMProbedAt = registry, ready, at
}

// cacheGateResult judges the published answer, or returns nil when nothing has
// been published: a daemon that has not looked has no verdict.
func cacheGateResult(snapshot Snapshot) *HealthResult {
	if !snapshot.CacheGatePublished {
		return nil
	}
	gate := snapshot.CacheGate
	probed := gate.ProbedAt.UTC().Format(time.RFC3339)
	switch {
	case gate.URL == "":
		return &HealthResult{OK: true, Reasons: []string{}}
	case !gate.Verified && gate.Container:
		return &HealthResult{OK: false, Reasons: []string{fmt.Sprintf(
			"cache gate unreachable: %s did not prove its identity at %s, so guests launched since reach npm directly",
			gate.URL, probed)}}
	case !gate.Verified:
		return &HealthResult{OK: false, Reasons: []string{fmt.Sprintf(
			"cache gate unreachable: %s did not prove its identity at %s, so guests launched since use GitHub's results service",
			gate.URL, probed)}}
	case gate.NPMRegistry != "" && !gate.NPMReady:
		return &HealthResult{OK: false, Reasons: []string{fmt.Sprintf(
			"npm cache not ready: %s did not answer at %s, so guests launched since reach npm directly",
			gate.NPMRegistry, gate.NPMProbedAt.UTC().Format(time.RFC3339))}}
	}
	return &HealthResult{OK: true, Reasons: []string{}}
}
