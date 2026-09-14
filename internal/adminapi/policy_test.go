package adminapi

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestPolicyRoundTripsAsOneFlatObject pins the wire shape: the projection's keys
// and `policyDigest` are siblings, and the digest never re-enters the field set
// it identifies — a reader that diffed it back in would report drift between two
// nodes that agree on everything else.
func TestPolicyRoundTripsAsOneFlatObject(t *testing.T) {
	published := Policy{Digest: "abc123", Fields: map[string]any{
		"macosBurst":  map[string]any{"mixedPlatformAdmission": true},
		"pollSeconds": float64(20),
	}}
	encoded, err := json.Marshal(published)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{`"policyDigest":"abc123"`, `"mixedPlatformAdmission":true`, `"pollSeconds":20`} {
		if !strings.Contains(string(encoded), fragment) {
			t.Fatalf("encoded %s, missing %s", encoded, fragment)
		}
	}
	var decoded Policy
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Digest != "abc123" {
		t.Fatalf("digest = %q", decoded.Digest)
	}
	if _, leaked := decoded.Fields[PolicyDigestKey]; leaked {
		t.Fatalf("the digest was decoded back into the fields it identifies: %#v", decoded.Fields)
	}
	if len(decoded.Fields) != 2 {
		t.Fatalf("fields = %#v", decoded.Fields)
	}
}

// TestPolicyDecodesADocumentWithNoReadableDigest covers the older or hand-edited
// document: an unreadable identity is a comparison the client falls back from,
// never a status document it refuses outright.
func TestPolicyDecodesADocumentWithNoReadableDigest(t *testing.T) {
	var decoded Policy
	if err := json.Unmarshal([]byte(`{"pollSeconds":20,"policyDigest":7}`), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Digest != "" || len(decoded.Fields) != 1 {
		t.Fatalf("decoded = %+v", decoded)
	}
	if err := json.Unmarshal([]byte(`["not an object"]`), &decoded); err == nil {
		t.Fatal("a policy that is not an object decoded without error")
	}
}

// TestPolicyMarshalReportsAnUnencodableField keeps the error honest: the daemon
// builds Fields by decoding its own JSON so this cannot happen there, and a
// caller that constructs one by hand must be told rather than shipped a
// truncated document.
func TestPolicyMarshalReportsAnUnencodableField(t *testing.T) {
	if _, err := json.Marshal(Policy{Fields: map[string]any{"broken": make(chan int)}}); err == nil {
		t.Fatal("an unencodable field marshalled without error")
	}
}

// TestEffectivePolicyTreatsAnUndeclaredPolicyAsSilence follows every other
// Effective* accessor: a daemon that declares nothing has not declared
// disagreement, and the empty field set is never nil, so a renderer can range
// over it without a nil check.
func TestEffectivePolicyTreatsAnUndeclaredPolicyAsSilence(t *testing.T) {
	older := Status{}.EffectivePolicy()
	if older.Digest != "" {
		t.Fatalf("an unpublished policy invented the digest %q", older.Digest)
	}
	if older.Fields == nil {
		t.Fatal("fields are nil, which encodes differently from the empty set")
	}
	if len(older.Fields) != 0 {
		t.Fatalf("an unpublished policy invented fields %#v", older.Fields)
	}

	sparse := Status{Policy: &Policy{Digest: "d"}}.EffectivePolicy()
	if sparse.Digest != "d" || sparse.Fields == nil {
		t.Fatalf("a published policy with no fields = %+v", sparse)
	}

	published := Status{Policy: &Policy{Digest: "d", Fields: map[string]any{"pollSeconds": float64(20)}}}
	if effective := published.EffectivePolicy(); effective.Digest != "d" || len(effective.Fields) != 1 {
		t.Fatalf("a published policy was not returned unchanged: %+v", effective)
	}
}
