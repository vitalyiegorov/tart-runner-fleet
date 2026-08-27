package telemetry

import "time"

// SessionYieldMetric is this node's published answer to one question: am I
// currently competing for the jobs GitHub could bind to my scale sets?
//
// It exists because the absence of this fact was the unreadable half of issue
// #292. A node that has withdrawn and a node that is merely idle both show an
// empty queue, zero instances, and a healthy host — and for eleven hours that
// was indistinguishable from a node quietly holding a sibling's work hostage.
type SessionYieldMetric struct {
	// Yielded reports that this node has released the sessions behind its scale
	// sets, so GitHub binds new jobs to whichever node still holds one.
	Yielded bool
	// Reason is the admission reason that caused the withdrawal, carried
	// verbatim from the host probe so the metric and `fleet status` cannot
	// disagree about why.
	Reason string
	// Since is when the current condition began. A withdrawal nobody notices
	// for an hour is the failure repeating itself in a new place.
	Since time.Time
	// Bindings and Withdrawn count the scale-set sessions this node owns and how
	// many are actually released. They differ while a close GitHub refused is
	// still being retried, which is the one state where "yielded" alone lies.
	Bindings, Withdrawn int
}

// SetSessionYield publishes the node's current yield posture.
func (h *Health) SetSessionYield(metric SessionYieldMetric) {
	h.mu.Lock()
	h.revision++
	h.sessionYield = &metric
	h.mu.Unlock()
}

// SessionYield judges the published posture. A node that never published, or
// published while serving, passes. A withdrawn node fails on purpose: it is not
// broken, but it is not serving either, and a check that stayed green would
// reproduce exactly the silence this was built to end.
func (h *Health) SessionYield() HealthResult {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return sessionYieldResult(h.sessionYield)
}

// sessionYieldResult is the judgement itself, shared by the health check and the
// versioned status envelope so the two can never disagree about whether this
// node is serving.
func sessionYieldResult(metric *SessionYieldMetric) HealthResult {
	if metric == nil || !metric.Yielded {
		return HealthResult{OK: true, Reasons: []string{}}
	}
	reason := metric.Reason
	if reason == "" {
		reason = "admission refused"
	}
	detail := "this node withdrew its scale-set sessions (" + reason +
		"); GitHub binds new jobs to a sibling advertising the same labels until it rejoins"
	if metric.Withdrawn < metric.Bindings {
		detail += "; a session GitHub refused to release is still being retried"
	}
	return HealthResult{OK: false, Reasons: []string{detail}}
}
