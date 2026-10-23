// Package cachegate is the write gate in front of a node's LAN GitHub Actions
// cache server (ADR 0078).
//
// The cache server verifies every runtime token's signature and keeps GitHub's
// cache scoping: a job reads the scopes its token grants and writes the one it
// may write. GitHub grants a pull-request job write access to its own
// `refs/pull/N/merge` scope, so behind the server alone a pull request can still
// store entries. This fleet's rule is stricter: trusted refs seed, pull requests
// only read. The gate is the whole of that rule.
//
// The gate never authorizes anything on a token's word. It reads the claims
// without verifying the signature and uses them only to refuse; a request it
// lets through is verified by the server exactly as if the gate were absent.
// A forged token therefore cannot widen access: it is refused here or rejected
// there.
package cachegate

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"
)

const cacheService = "/twirp/github.actions.results.api.v1.CacheService/"

// writePaths are the two halves of an Actions cache save: reserving an entry
// and committing it. Refusing either keeps an entry from ever becoming
// restorable.
var writePaths = map[string]bool{
	cacheService + "CreateCacheEntry":         true,
	cacheService + "FinalizeCacheEntryUpload": true,
}

// operatorPrefixes are cache-server surfaces a guest has no reason to reach.
var operatorPrefixes = []string{"/management-api", "/metrics", "/_docs"}

// cachePrefixes are the paths the cache server answers: the cache service
// itself, the upload and download URLs it signs, and its health probe.
// Everything else a runner sends to its results URL is not cache traffic.
var cachePrefixes = []string{cacheService, "/upload/", "/devstoreaccount1/", "/download/", "/health"}

// trustedRefPrefixes are the scopes a write may target: branches and tags,
// which only a principal with push access can run a workflow on.
var trustedRefPrefixes = []string{"refs/heads/", "refs/tags/"}

// writePermission is the bit GitHub sets in a cache scope's Permission for
// write access (1 is read, 3 is both).
const writePermission = 2

// scope is one entry of the `ac` claim GitHub puts in a job's runtime token.
type scope struct {
	Scope      string `json:"Scope"`
	Permission int    `json:"Permission"`
}

// Upstream names where an allowed request is sent.
type Upstream int

const (
	// Results is GitHub's own results service. Once a runner's results URL
	// points at the gate, every artifact and log call arrives here too, and
	// none of it should depend on the cache server being up.
	Results Upstream = iota
	// CacheServer is the node's LAN cache server.
	CacheServer
)

// Verdict is the gate's answer for one request.
type Verdict struct {
	Allow    bool
	Upstream Upstream
	Status   int
	Reason   string
}

// Decide judges one request from its path and headers alone. The method is
// deliberately not an input: the server routes FinalizeCacheEntryUpload for
// any verb, so a write path is a write path whatever it is sent with. Decide is
// pure, so every case is a table entry rather than a server round trip.
func Decide(path string, header http.Header) Verdict {
	for _, prefix := range operatorPrefixes {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return Verdict{Status: http.StatusNotFound, Reason: "operator surface"}
		}
	}
	if !hasPrefix(path, cachePrefixes) {
		return Verdict{Allow: true, Upstream: Results}
	}
	if !writePaths[path] {
		return Verdict{Allow: true, Upstream: CacheServer}
	}
	scopes, ok := claimedScopes(header.Get("Authorization"))
	if !ok {
		return Verdict{Status: http.StatusForbidden, Reason: "unreadable cache scopes"}
	}
	writable := 0
	for _, s := range scopes {
		if s.Permission&writePermission == 0 {
			continue
		}
		if !trusted(s.Scope) {
			return Verdict{Status: http.StatusForbidden, Reason: "write scope " + boundScope(s.Scope) + " is read-only on this fleet"}
		}
		writable++
	}
	if writable == 0 {
		return Verdict{Status: http.StatusForbidden, Reason: "token grants no write scope"}
	}
	return Verdict{Allow: true, Upstream: CacheServer}
}

// maxLoggedScope bounds how much of a refused scope a reason quotes. The scope
// comes from a token the gate has not verified, and the reason is logged, so a
// forged token must not be able to write an arbitrarily long line.
const maxLoggedScope = 128

// boundScope is the scope as a refusal names it: whole when short, otherwise
// cut to maxLoggedScope bytes on a rune boundary with its true length named.
func boundScope(value string) string {
	if len(value) <= maxLoggedScope {
		return value
	}
	cut := maxLoggedScope
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return fmt.Sprintf("%s... (%d bytes)", value[:cut], len(value))
}

func trusted(ref string) bool { return hasPrefix(ref, trustedRefPrefixes) }

func hasPrefix(value string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

// claimedScopes reads the `ac` claim of a bearer JWT without verifying it.
func claimedScopes(authorization string) ([]scope, bool) {
	raw, found := strings.CutPrefix(authorization, "Bearer ")
	if !found {
		return nil, false
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, false
	}
	var claims struct {
		AC *string `json:"ac"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.AC == nil {
		return nil, false
	}
	var scopes []scope
	if json.Unmarshal([]byte(*claims.AC), &scopes) != nil {
		return nil, false
	}
	return scopes, true
}
