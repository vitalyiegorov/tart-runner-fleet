package telemetry

import "time"

// UpdateDrainMetric is this node's published answer to one question: am I
// refusing admission on purpose, to reach the quiescence my own update needs?
//
// It exists because the alternative is indistinguishable from a fault. A node
// that has stopped admitting while healthy, idle, and with a queue building
// looks broken; ADR 0011's gate meant that state previously never arrived
// deliberately at all, and the busiest nodes simply ran the oldest code —
// 1011 consecutive refusals on one of them (#230, #282).
type UpdateDrainMetric struct {
	// Draining reports that new instances are being refused so the live ones
	// can finish. Nothing already running is affected.
	Draining bool
	// Candidate is the newest generation on disk that is not the running one.
	Candidate string
	// PendingSince is when that candidate was first observed unapplied, and
	// Since is when the current phase began. Together they separate "waiting to
	// start draining" from "draining", which is the difference between a node
	// that is fine and a node an operator may want to look at.
	PendingSince time.Time
	Since        time.Time
}

// SetUpdateDrain publishes the node's current drain posture.
func (h *Health) SetUpdateDrain(metric UpdateDrainMetric) {
	h.mu.Lock()
	h.revision++
	h.updateDrain = &metric
	h.mu.Unlock()
}

// UpdateDrain judges the published posture. A node draining is not broken, but
// it is running at zero admission on purpose and an operator reading `doctor`
// should be told so plainly rather than left to infer it from an empty queue.
func (h *Health) UpdateDrain() HealthResult {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return updateDrainResult(h.updateDrain)
}

// updateDrainResult is the judgement itself, shared by the health check and the
// versioned status envelope so the two can never disagree.
func updateDrainResult(metric *UpdateDrainMetric) HealthResult {
	if metric == nil || !metric.Draining {
		return HealthResult{OK: true, Reasons: []string{}}
	}
	candidate := metric.Candidate
	if candidate == "" {
		candidate = "a newer generation"
	}
	return HealthResult{OK: false, Reasons: []string{
		"this node is refusing admission to reach the quiescence " + candidate +
			" needs; running work finishes untouched and admission resumes when the update lands or the drain times out"}}
}
