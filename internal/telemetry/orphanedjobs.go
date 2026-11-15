package telemetry

import "time"

// SetUndeliveredJobs replaces one scope's read-only REST diagnosis. A failed
// refresh never calls this; its observation freshness invalidates the evidence.
func (h *Health) SetUndeliveredJobs(observation string, reasons []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.undeliveredJobs == nil {
		h.undeliveredJobs = map[string][]string{}
	}
	h.undeliveredJobs[observation] = append([]string(nil), reasons...)
	h.revision++
}

// Called under the snapshot lock. Findings expire with their REST observation,
// so an old snapshot cannot diagnose a job that has since started or vanished.
func (h *Health) undeliveredJobFindings(now time.Time) []string {
	var reasons []string
	for name, rows := range h.undeliveredJobs {
		observation := h.observations[name]
		if observation.Freshness == ObservationFresh && !observation.ObservedAt.IsZero() && !observation.ObservedAt.After(now) && now.Sub(observation.ObservedAt) <= h.criticalObservationTTL {
			reasons = append(reasons, rows...)
		}
	}
	return reasons
}
