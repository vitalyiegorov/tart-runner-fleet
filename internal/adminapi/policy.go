package adminapi

import "encoding/json"

// PolicyDigestKey is the one key of the `policy` object this package knows by
// name. It is a constant because the daemon that writes it and every reader that
// strips it before comparing must agree exactly.
const PolicyDigestKey = "policyDigest"

// Policy is the node's own declaration of the load-bearing configuration it runs
// with: a bounded, credential-free projection of its effective settings, plus a
// digest of them (ADR 0053, issue #304). Two nodes are compared by digest at a
// glance and by field when the digests differ.
//
// The projected fields are deliberately NOT a struct here. This package is the
// fleet.v1 compatibility contract and it owns the envelope — an object whose
// keys are a bounded projection, plus `policyDigest` — while the inventory of
// what is load-bearing belongs to the configuration package that can answer for
// it. Carrying the fields as decoded JSON is what lets a newer daemon publish a
// key this client has never heard of and still be diffed correctly against its
// peer, which is the entire point of publishing the document at all.
type Policy struct {
	// Digest is the hex sha256 of the canonical encoding of Fields. It never
	// appears inside Fields, so a reader that recomputes it from the fields it was
	// given gets the same answer the publisher did.
	Digest string
	// Fields is the projection itself, keyed exactly as the configuration file
	// keys it. Absent from Fields means the publishing daemon does not project
	// that key at all; it never means the node has the setting off, which is the
	// distinction issue #304 turned on.
	Fields map[string]any
}

// MarshalJSON writes the fields and the digest as one flat object, so an
// operator reading `data.policy` sees the settings and the identity of the set
// together rather than one nested inside the other.
func (p Policy) MarshalJSON() ([]byte, error) {
	document := make(map[string]any, len(p.Fields)+1)
	for key, value := range p.Fields {
		document[key] = value
	}
	document[PolicyDigestKey] = p.Digest
	return json.Marshal(document)
}

// UnmarshalJSON splits the digest back out of the flat object. A document whose
// digest is missing or is not a string decodes to an empty digest rather than an
// error: an unreadable identity is a comparison this client falls back from, not
// a status document it must refuse.
func (p *Policy) UnmarshalJSON(data []byte) error {
	document := map[string]any{}
	if err := json.Unmarshal(data, &document); err != nil {
		return err
	}
	digest, _ := document[PolicyDigestKey].(string)
	delete(document, PolicyDigestKey)
	p.Digest, p.Fields = digest, document
	return nil
}

// EffectivePolicy reads the policy declaration an older daemon does not publish,
// on the same terms as every Effective* accessor: absence is not a finding. A
// daemon that declares nothing returns an empty digest and an empty — never nil
// — field set, so a caller may range over it without a nil check and a renderer
// can say "not reported by this daemon" instead of inventing agreement.
func (s Status) EffectivePolicy() Policy {
	if s.Policy == nil {
		return Policy{Fields: map[string]any{}}
	}
	policy := *s.Policy
	if policy.Fields == nil {
		policy.Fields = map[string]any{}
	}
	return policy
}
