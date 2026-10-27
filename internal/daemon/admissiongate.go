package daemon

import "time"

// admissionGate remembers the admission decision this node last observed, so the
// daemon can say when the gate moved and status can say when the current state
// began (issue #436). It decides nothing: admission is the host probe's and the
// update drain's, and this only dates their conclusion.
//
// It is edge-triggered rather than rate limited. A window would drop the very
// edges a flapping gate is made of, and a log that shows "closed" twice with no
// "opened" between is worse evidence than none. Edges are bounded by the tick.
type admissionGate struct {
	observed bool
	allowed  bool
	reason   string
	since    time.Time
}

// admissionTransition is one edge of the gate. Previous and Held describe the
// state it left; both are empty on the first observation after a start.
type admissionTransition struct {
	Allowed         bool
	Reason          string
	PreviousAllowed bool
	Previous        string
	Held            time.Duration
}

// Observe folds one tick's decision into the gate and returns the edge it made,
// if any. An empty reason is an unobserved host: not a state, so neither an edge
// nor a reset of the clock. A daemon that starts open has made no edge worth a
// line; one that starts closed has, because nothing earlier says why.
func (g *admissionGate) Observe(at time.Time, allowed bool, reason string) (admissionTransition, bool) {
	if g == nil || reason == "" {
		return admissionTransition{}, false
	}
	if g.observed && g.allowed == allowed && g.reason == reason {
		return admissionTransition{}, false
	}
	edge := admissionTransition{Allowed: allowed, Reason: reason}
	if g.observed {
		edge.PreviousAllowed, edge.Previous, edge.Held = g.allowed, g.reason, at.Sub(g.since)
	}
	first := !g.observed
	g.observed, g.allowed, g.reason, g.since = true, allowed, reason, at
	return edge, !first || !allowed
}

// Since is when the current admission state began; zero before any observation.
func (g *admissionGate) Since() time.Time {
	if g == nil {
		return time.Time{}
	}
	return g.since
}
