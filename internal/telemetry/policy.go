package telemetry

// PolicyMetric is the node's declaration of the load-bearing configuration it
// runs with: the bounded projection its configuration package produced, and the
// digest of it (ADR 0053).
//
// The projection is carried as decoded JSON rather than as a struct for the
// reason the DTO gives: what is load-bearing is the configuration package's
// judgement, and neither telemetry nor the API contract should have to restate
// that inventory to carry it.
type PolicyMetric struct {
	// Digest is the hex sha256 of the canonical encoding of Fields.
	Digest string
	// Fields is the projection, keyed exactly as the configuration file keys it.
	Fields map[string]any
}

// SetPolicy publishes what this node is configured with. Like the runner image
// set and the guest-console posture, it is a fact about the configuration this
// process started with — configuration does not change without a restart — so it
// is stated once at startup and replaces whatever was there.
func (h *Health) SetPolicy(metric PolicyMetric) {
	h.mu.Lock()
	h.revision++
	h.policy = &metric
	h.mu.Unlock()
}
