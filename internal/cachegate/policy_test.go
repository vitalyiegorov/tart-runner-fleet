package cachegate

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"
)

// token builds the bearer value a runner presents: header.payload.signature,
// with the cache scopes GitHub grants serialised as a JSON *string* inside the
// `ac` claim. The signature is junk on purpose: the gate never trusts a token
// to allow anything, it only refuses on what the token claims, and the cache
// server behind it verifies every signature it is shown (ADR 0078).
func token(t *testing.T, scopes []scope) string {
	t.Helper()
	ac, err := json.Marshal(scopes)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{"ac": string(ac), "repository_id": "123"})
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return "Bearer " + enc([]byte(`{"alg":"RS256"}`)) + "." + enc(payload) + ".c2ln"
}

func header(value string) http.Header {
	h := http.Header{}
	if value != "" {
		h.Set("Authorization", value)
	}
	return h
}

const (
	create   = "/twirp/github.actions.results.api.v1.CacheService/CreateCacheEntry"
	finalize = "/twirp/github.actions.results.api.v1.CacheService/FinalizeCacheEntryUpload"
	lookup   = "/twirp/github.actions.results.api.v1.CacheService/GetCacheEntryDownloadURL"
)

// TestPullRequestTokensCannotWrite is the security property of #401: GitHub
// hands a pull-request job write permission on refs/pull/N/merge, and the gate
// refuses both halves of a save under that grant, so a pull request can read
// the default branch's caches and write none.
func TestPullRequestTokensCannotWrite(t *testing.T) {
	pr := token(t, []scope{{Scope: "refs/pull/7/merge", Permission: 3}, {Scope: "refs/heads/main", Permission: 1}})
	for _, path := range []string{create, finalize} {
		if v := Decide(path, header(pr)); v.Allow || v.Status != http.StatusForbidden {
			t.Fatalf("%s: pull-request save allowed: %+v", path, v)
		}
	}
	if v := Decide(lookup, header(pr)); !v.Allow {
		t.Fatalf("pull-request restore refused: %+v", v)
	}
}

func TestTrustedRefsMayWrite(t *testing.T) {
	for _, s := range []scope{
		{Scope: "refs/heads/main", Permission: 3},
		{Scope: "refs/heads/release/v1", Permission: 2},
		{Scope: "refs/tags/v1.2.3", Permission: 3},
	} {
		v := Decide(create, header(token(t, []scope{s, {Scope: "refs/heads/main", Permission: 1}})))
		if !v.Allow || v.Upstream != CacheServer {
			t.Fatalf("%s refused: %+v", s.Scope, v)
		}
	}
}

// TestWritesFailClosed pins that every token the gate cannot read is a refusal
// for a write, never a pass-through: an unreadable claim is not evidence that
// the job is trusted.
func TestWritesFailClosed(t *testing.T) {
	enc := base64.RawURLEncoding.EncodeToString
	for name, auth := range map[string]string{
		"missing":          "",
		"not bearer":       "Basic abc",
		"two segments":     "Bearer a.b",
		"bad base64":       "Bearer a.!!!.c",
		"payload not json": "Bearer a." + enc([]byte("nope")) + ".c",
		"no ac claim":      "Bearer a." + enc([]byte(`{"repository_id":"1"}`)) + ".c",
		"ac not a string":  "Bearer a." + enc([]byte(`{"ac":[{"Scope":"refs/heads/main","Permission":3}]}`)) + ".c",
		"ac not json":      "Bearer a." + enc([]byte(`{"ac":"[{"}`)) + ".c",
		"no write scope":   token(t, []scope{{Scope: "refs/heads/main", Permission: 1}}),
		"unknown ref kind": token(t, []scope{{Scope: "refs/remotes/x", Permission: 3}}),
		"mixed":            token(t, []scope{{Scope: "refs/heads/main", Permission: 3}, {Scope: "refs/pull/1/merge", Permission: 2}}),
	} {
		if v := Decide(create, header(auth)); v.Allow || v.Status != http.StatusForbidden {
			t.Fatalf("%s: allowed %+v", name, v)
		}
	}
}

// TestOperatorSurfacesAreNotServedToGuests keeps the cache server's management
// API, metrics and API docs off the address guests reach. They stay on the
// server's loopback port for the operator.
func TestOperatorSurfacesAreNotServedToGuests(t *testing.T) {
	for _, path := range []string{"/management-api/cacheEntries", "/metrics", "/_docs", "/_docs/spec.json"} {
		if v := Decide(path, header("")); v.Allow || v.Status != http.StatusNotFound {
			t.Fatalf("%s: %+v", path, v)
		}
	}
}

// TestCacheTrafficGoesToTheCacheServer covers the requests the gate has no
// objection to that belong to the cache: restores, the upload and download
// URLs the server signs, and its health probe.
func TestCacheTrafficGoesToTheCacheServer(t *testing.T) {
	for _, path := range []string{
		lookup,
		"/devstoreaccount1/upload/1234567890",
		"/upload/1234567890",
		"/download/0b4c1d7e-1f3c-4a7e-9a59-7f8a1d9b2c3d",
		"/health",
	} {
		if v := Decide(path, header("")); !v.Allow || v.Upstream != CacheServer {
			t.Fatalf("%s: %+v", path, v)
		}
	}
}

// TestEverythingElseGoesStraightToGitHub pins the blast radius: the runner
// sends every results-service call to the gate once its URL is overridden, and
// only cache calls depend on the cache server. Artifact uploads keep working
// while the cache server is down or being replaced.
func TestEverythingElseGoesStraightToGitHub(t *testing.T) {
	for _, path := range []string{
		"/twirp/github.actions.results.api.v1.ArtifactService/CreateArtifact",
		"/twirp/results.services.receiver.Receiver/GetJobLogsSignedBlobURL",
		"/metricsx",
		"/uploads",
		"/",
	} {
		if v := Decide(path, header("")); !v.Allow || v.Upstream != Results {
			t.Fatalf("%s: %+v", path, v)
		}
	}
}

// TestARefusalNamesAtMostABoundedScope pins the review finding on #408: the
// scope a refusal names comes from an unverified token, so a forged token must
// not be able to write an arbitrarily long line into the gate's log.
func TestARefusalNamesAtMostABoundedScope(t *testing.T) {
	long := "refs/pull/" + strings.Repeat("x", 10000)
	verdict := Decide(create, http.Header{"Authorization": {token(t, []scope{{Scope: long, Permission: 3}})}})
	if verdict.Allow || verdict.Status != http.StatusForbidden {
		t.Fatalf("verdict = %+v, want a refusal", verdict)
	}
	if len(verdict.Reason) > maxLoggedScope+64 || !strings.Contains(verdict.Reason, "10010 bytes") {
		t.Fatalf("reason is %d bytes, want the scope cut to %d and its true length named: %.200q",
			len(verdict.Reason), maxLoggedScope, verdict.Reason)
	}
	wide := Decide(create, http.Header{"Authorization": {token(t, []scope{{Scope: "refs/pull/x" + strings.Repeat("é", 200), Permission: 3}})}})
	if !utf8.ValidString(wide.Reason) {
		t.Fatalf("a cut through a multi-byte rune must back off to its start: %q", wide.Reason)
	}
	short := Decide(create, http.Header{"Authorization": {token(t, []scope{{Scope: "refs/pull/1/merge", Permission: 3}})}})
	if short.Reason != "write scope refs/pull/1/merge is read-only on this fleet" {
		t.Fatalf("a short scope must be named whole, got %q", short.Reason)
	}
}
