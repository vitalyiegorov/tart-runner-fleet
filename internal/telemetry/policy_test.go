package telemetry

import (
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
)

// TestTheEnvelopeCarriesThePolicyDeclaration keeps the projection honest in both
// directions: a daemon that declared nothing emits no `policy` key at all, so an
// older client sees exactly the document it always saw, and one that has
// declared publishes the digest and the fields verbatim (ADR 0053).
func TestTheEnvelopeCarriesThePolicyDeclaration(t *testing.T) {
	health := runnerVersionHealth(t)
	envelope := policyEnvelope(health)
	if envelope.Data.Policy != nil {
		t.Fatalf("an undeclared policy must be absent from the document, got %#v", envelope.Data.Policy)
	}
	before := health.Snapshot().Revision

	health.SetPolicy(PolicyMetric{Digest: "d1", Fields: map[string]any{
		"macosBurst": map[string]any{"mixedPlatformAdmission": true}}})
	if health.Snapshot().Revision == before {
		t.Fatal("declaring a policy did not advance the revision the ETag is taken from")
	}
	envelope = policyEnvelope(health)
	if envelope.Data.Policy == nil || envelope.Data.Policy.Digest != "d1" {
		t.Fatalf("the declared digest must travel verbatim, got %#v", envelope.Data.Policy)
	}
	burst, ok := envelope.Data.Policy.Fields["macosBurst"].(map[string]any)
	if !ok || burst["mixedPlatformAdmission"] != true {
		t.Fatalf("the declared fields must travel verbatim, got %#v", envelope.Data.Policy.Fields)
	}
}

// TestAPolicyWithNoFieldsPublishesAnEmptyObject stops a declaration with nothing
// in it encoding as JSON null, which a reader would have to special-case and
// would eventually read as "no opinion" rather than "no fields".
func TestAPolicyWithNoFieldsPublishesAnEmptyObject(t *testing.T) {
	health := runnerVersionHealth(t)
	health.SetPolicy(PolicyMetric{Digest: "d2"})
	published := policyEnvelope(health).Data.Policy
	if published == nil || published.Fields == nil || len(published.Fields) != 0 {
		t.Fatalf("an empty declaration = %#v", published)
	}
}

func policyEnvelope(health *Health) adminapi.StatusEnvelope {
	return statusEnvelope(health.Snapshot(), "v", "authority", HealthResult{OK: true}, HealthResult{OK: true},
		HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true},
		HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true})
}
