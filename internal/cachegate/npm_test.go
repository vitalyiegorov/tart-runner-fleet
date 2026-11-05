package cachegate

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const lanGate = "192.168.1.64:3017"

// TestDecideNPMAllowsOnlyTheReadRoutesAClientNeeds is the route's whole policy,
// stated independently of what the registry would do with the request.
func TestDecideNPMAllowsOnlyTheReadRoutesAClientNeeds(t *testing.T) {
	for _, test := range []struct {
		method, path string
		status       int
	}{
		// Metadata, in every spelling a client sends.
		{http.MethodGet, "/npm/lodash", 0},
		{http.MethodHead, "/npm/lodash", 0},
		{http.MethodGet, "/npm/JSONStream", 0},
		{http.MethodGet, "/npm/@types/node", 0},
		{http.MethodGet, "/npm/@types%2fnode", 0},
		{http.MethodGet, "/npm/@types%2Fnode", 0},
		// Valid names under a scope, which only an unscoped name may not use.
		{http.MethodGet, "/npm/@_koi/react", 0},
		{http.MethodGet, "/npm/@scope/_private-looking", 0},
		{http.MethodGet, "/npm/@_koi/react/-/react-1.0.0.tgz", 0},
		{http.MethodGet, "/npm/@scope/_y/-/_y-1.0.0.tgz", 0},
		// Tarballs: what a frozen lockfile asks for without reading metadata.
		{http.MethodGet, "/npm/lodash/-/lodash-4.17.21.tgz", 0},
		{http.MethodGet, "/npm/@types/node/-/node-22.0.0.tgz", 0},
		{http.MethodGet, "/npm/@types%2fnode/-/node-22.0.0.tgz", 0},
		// One version or tag: how pnpm 12 fetches its own binary, which the image's Corepack
		// shim runs before anything else, so refusing this fails the job's first pnpm call.
		{http.MethodGet, "/npm/@pnpm/exe.linux-x64/12.16.1", 0},
		{http.MethodGet, "/npm/@pnpm%2Fexe.linux-x64/12.16.1", 0},
		{http.MethodGet, "/npm/lodash/latest", 0},
		{http.MethodGet, "/npm/lodash/4.17.21", 0},
		// The readiness probe, and `npm dist-tag ls`.
		{http.MethodGet, "/npm/-/ping", 0},
		{http.MethodGet, "/npm/-/package/lodash/dist-tags", 0},
		{http.MethodGet, "/npm/-/package/@types%2fnode/dist-tags", 0},
		// Attestations: where pnpm reads a publish time, before it falls back to a full document.
		{http.MethodGet, "/npm/-/npm/v1/attestations/metro-runtime@0.87.1", 0},
		{http.MethodHead, "/npm/-/npm/v1/attestations/metro-runtime@0.87.1", 0},
		{http.MethodGet, "/npm/-/npm/v1/attestations/@babel/core@7.0.0", 0},
		{http.MethodGet, "/npm/-/npm/v1/attestations/@babel%2fcore@7.0.0", 0},

		// Every mutation, by method, whatever the path is.
		{http.MethodPut, "/npm/lodash", http.StatusForbidden},                                  // publish
		{http.MethodPut, "/npm/@types%2fnode", http.StatusForbidden},                           // publish
		{http.MethodPut, "/npm/lodash/-rev/1-a", http.StatusForbidden},                         // unpublish
		{http.MethodDelete, "/npm/lodash/-rev/1-a", http.StatusForbidden},                      // unpublish
		{http.MethodDelete, "/npm/lodash/-/lodash-4.17.21.tgz/-rev/1-a", http.StatusForbidden}, // unpublish
		{http.MethodPut, "/npm/-/user/org.couchdb.user:eve", http.StatusForbidden},             // adduser, login
		{http.MethodPost, "/npm/-/v1/login", http.StatusForbidden},                             // web login
		{http.MethodDelete, "/npm/-/user/token/abcdef", http.StatusForbidden},                  // logout
		{http.MethodPost, "/npm/-/npm/v1/tokens", http.StatusForbidden},                        // token create
		{http.MethodDelete, "/npm/-/npm/v1/tokens/token/abcdef", http.StatusForbidden},         // token revoke
		{http.MethodPost, "/npm/-/npm/v1/security/audits", http.StatusForbidden},               // audit
		{http.MethodPost, "/npm/-/npm/v1/security/audits/quick", http.StatusForbidden},         // audit
		{http.MethodPost, "/npm/-/npm/v1/security/advisories/bulk", http.StatusForbidden},      // audit
		{http.MethodPut, "/npm/-/package/lodash/dist-tags/latest", http.StatusForbidden},       // dist-tag add
		{http.MethodDelete, "/npm/-/package/lodash/dist-tags/latest", http.StatusForbidden},    // dist-tag rm
		{http.MethodPatch, "/npm/lodash", http.StatusForbidden},                                // nothing a registry does
		{http.MethodOptions, "/npm/lodash", http.StatusForbidden},                              // nor this
		{"PROPFIND", "/npm/lodash", http.StatusForbidden},                                      // an unknown verb is not a read
		{http.MethodPost, "/npm/_session", http.StatusForbidden},                               // CouchDB login
		{http.MethodPost, "/npm/-/verdaccio/sec/login", http.StatusForbidden},                  // Verdaccio's own login
		{http.MethodPost, "/npm/-/verdaccio/sec/reset_password", http.StatusForbidden},         // and its reset
		{http.MethodPut, "/npm/-/_view/starredByUser", http.StatusForbidden},                   // legacy star
		{http.MethodPost, "/npm/-/npm/v1/security/advisories/bulk?write=true", http.StatusForbidden},

		// Attestations can be read and nothing else.
		{http.MethodPost, "/npm/-/npm/v1/attestations/lodash@4.17.21", http.StatusForbidden},
		{http.MethodPut, "/npm/-/npm/v1/attestations/lodash@4.17.21", http.StatusForbidden},
		{http.MethodPatch, "/npm/-/npm/v1/attestations/lodash@4.17.21", http.StatusForbidden},
		{http.MethodDelete, "/npm/-/npm/v1/attestations/lodash@4.17.21", http.StatusForbidden},

		// Reads that name an account, a token, or Verdaccio's own surface.
		{http.MethodGet, "/npm/-/whoami", http.StatusNotFound},
		{http.MethodGet, "/npm/-/user/org.couchdb.user:eve", http.StatusNotFound},
		{http.MethodGet, "/npm/-/npm/v1/tokens", http.StatusNotFound},
		{http.MethodGet, "/npm/-/npm/v1/security/audits", http.StatusNotFound},
		// Attestations are one package at one version, and nothing near them.
		{http.MethodGet, "/npm/-/npm/v1/attestations", http.StatusNotFound},
		{http.MethodGet, "/npm/-/npm/v1/attestations/", http.StatusNotFound},
		{http.MethodGet, "/npm/-/npm/v1/attestations/lodash", http.StatusNotFound},
		{http.MethodGet, "/npm/-/npm/v1/attestations/lodash@", http.StatusNotFound},
		{http.MethodGet, "/npm/-/npm/v1/attestations/lodash%404.17.21", http.StatusNotFound},
		{http.MethodGet, "/npm/-/npm/v1/attestations/lodash@4.17.21/extra", http.StatusNotFound},
		{http.MethodGet, "/npm/-/npm/v1/attestations/../tokens", http.StatusNotFound},
		{http.MethodGet, "/npm/-/npm/v1/attestations/lodash@4.17.21%2f..%2ftokens", http.StatusNotFound},
		{http.MethodGet, "/npm/-/v1/search", http.StatusNotFound},
		{http.MethodGet, "/npm/-/verdaccio/data/packages", http.StatusNotFound},
		{http.MethodGet, "/npm/-/web/detail/lodash", http.StatusNotFound},
		{http.MethodGet, "/npm/-/metrics", http.StatusNotFound},
		{http.MethodGet, "/npm/_session", http.StatusNotFound},
		{http.MethodGet, "/npm/", http.StatusNotFound},
		{http.MethodGet, "/npm", http.StatusNotFound},

		// A path the backend might read differently from the gate.
		{http.MethodGet, "/npm/../metrics", http.StatusNotFound},
		{http.MethodGet, "/npm/%2e%2e/metrics", http.StatusNotFound},
		{http.MethodGet, "/npm/..%2fmetrics", http.StatusNotFound},
		{http.MethodGet, "/npm/.hidden", http.StatusNotFound},
		{http.MethodGet, "/npm/@scope/..", http.StatusNotFound},
		{http.MethodGet, "/npm/@../x", http.StatusNotFound},
		{http.MethodGet, "/npm/@scope/.hidden", http.StatusNotFound},
		{http.MethodGet, "/npm//lodash", http.StatusNotFound},
		{http.MethodGet, "/npm/lodash/", http.StatusNotFound},
		{http.MethodGet, "/npm/-%2fwhoami", http.StatusNotFound},
		{http.MethodGet, "/npm/lodash%2f-%2flodash-4.17.21.tgz", http.StatusNotFound},
		{http.MethodGet, "/npm/%6codash", http.StatusNotFound},
		{http.MethodGet, "/npm/lodash/-/../../-/whoami", http.StatusNotFound},
		{http.MethodGet, "/npm/lodash/-/lodash-4.17.21.tar", http.StatusNotFound},
		{http.MethodGet, "/npm/lodash/-rev", http.StatusNotFound},
		{http.MethodGet, "/npm/lodash/.hidden", http.StatusNotFound},
		{http.MethodGet, "/npm/lodash/1.0.0/extra", http.StatusNotFound},
		{http.MethodGet, "/npm/lodash/1.0.0%2f..", http.StatusNotFound},
		{http.MethodGet, "/npm/-/package/lodash/dist-tags/latest", http.StatusNotFound},
	} {
		path, _, _ := strings.Cut(test.path, "?")
		verdict := DecideNPM(test.method, path, lanGate, http.Header{})
		if test.status == 0 {
			if !verdict.Allow || verdict.Upstream != NPM {
				t.Errorf("%s %s = %+v, want allowed to the npm cache", test.method, test.path, verdict)
			}
			continue
		}
		if verdict.Allow || verdict.Status != test.status {
			t.Errorf("%s %s = %+v, want %d", test.method, test.path, verdict, test.status)
		}
	}
}

// TestDecideNPMRefusesAnythingButADirectLANAddress pins cloudflared to admin and
// health: a tunnel hostname here would move cache data over it.
func TestDecideNPMRefusesAnythingButADirectLANAddress(t *testing.T) {
	for host, allowed := range map[string]bool{
		"192.168.1.64:3017":             true,
		"10.0.0.2:3017":                 true,
		"172.16.0.1:3017":               true,
		"127.0.0.1:3017":                true,
		"[::1]:3017":                    true,
		"host.containers.internal:3017": true,
		"HOST.CONTAINERS.INTERNAL:3017": true,
		"gate.example.dev":              false,
		"gate.example.dev:3017":         false,
		"cache.vitaliiyehorov.dev":      false,
		"8.8.8.8:3017":                  false,
		"100.64.0.1:3017":               false,
		"host.containers.internal.evil": false,
		"":                              false,
	} {
		if verdict := DecideNPM(http.MethodGet, "/npm/lodash", host, http.Header{}); verdict.Allow != allowed {
			t.Errorf("host %q = %+v, want allow=%v", host, verdict, allowed)
		} else if !allowed && verdict.Status != http.StatusForbidden {
			t.Errorf("host %q status = %d", host, verdict.Status)
		}
	}
	for _, name := range []string{"Cf-Connecting-Ip", "Cf-Ray", "Cf-Warp-Tag-Id", "Cdn-Loop"} {
		header := http.Header{}
		header.Set(name, "x")
		if verdict := DecideNPM(http.MethodGet, "/npm/lodash", lanGate, header); verdict.Allow || verdict.Status != http.StatusForbidden {
			t.Errorf("a request carrying %s reached the npm cache: %+v", name, verdict)
		}
	}
}

// npmBackend records what reaches the npm cache and answers like a registry does.
type npmBackend struct {
	*httptest.Server
	mu       sync.Mutex
	requests []*http.Request
	uris     []string
	bodies   []string
	response func(http.ResponseWriter, *http.Request)
}

func newNPMBackend(t *testing.T) (*npmBackend, *url.URL) {
	t.Helper()
	backend := &npmBackend{}
	backend.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		backend.mu.Lock()
		backend.requests = append(backend.requests, r)
		backend.uris = append(backend.uris, r.RequestURI)
		backend.bodies = append(backend.bodies, string(body))
		respond := backend.response
		backend.mu.Unlock()
		if respond != nil {
			respond(w, r)
			return
		}
		_, _ = io.WriteString(w, "npm:"+r.RequestURI)
	}))
	t.Cleanup(backend.Close)
	target, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	return backend, target
}

func (b *npmBackend) seen() ([]*http.Request, []string, []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*http.Request(nil), b.requests...), append([]string(nil), b.uris...), append([]string(nil), b.bodies...)
}

func npmGate(t *testing.T, npm *url.URL, logs io.Writer, counters *Counters) (*httptest.Server, *url.URL, func() []string) {
	t.Helper()
	cache, _ := upstream(t)
	results, resultsSeen := upstream(t)
	var route *npmCache
	if npm != nil {
		route = newNPMCache(filepath.Join(t.TempDir(), "data"), npm)
	}
	gate := httptest.NewServer(countingHandler(cache, results, route, logs, counters))
	t.Cleanup(gate.Close)
	return gate, cache, resultsSeen
}

func TestTheNPMRouteStripsTheGatePrefixAndKeepsAScopesEncoding(t *testing.T) {
	backend, target := newNPMBackend(t)
	gate, _, _ := npmGate(t, target, io.Discard, &Counters{})
	for path, want := range map[string]string{
		"/npm/lodash":                    "/lodash",
		"/npm/@types%2fnode":             "/@types%2fnode",
		"/npm/@other%2Fnode":             "/@other%2Fnode",
		"/npm/@third/node":               "/@third/node",
		"/npm/lodash/-/lodash-1.0.0.tgz": "/lodash/-/lodash-1.0.0.tgz",
	} {
		resp, err := http.Get(gate.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(body) != "npm:"+want {
			t.Fatalf("%s: status=%d body=%q, want %q", path, resp.StatusCode, body, want)
		}
	}
	if _, uris, _ := backend.seen(); len(uris) != 5 {
		t.Fatalf("uris = %q", uris)
	}
}

// TestTheNPMRouteForwardsNoGuestCredential: nothing a guest attaches reaches
// the registry except the short list of read headers it needs, so a header
// nobody thought of is refused by default.
func TestTheNPMRouteForwardsNoGuestCredential(t *testing.T) {
	const secret = "npm_S3CRET-token"
	backend, target := newNPMBackend(t)
	var logs bytes.Buffer
	gate, _, _ := npmGate(t, target, &logs, &Counters{})

	req, _ := http.NewRequest(http.MethodGet, gate.URL+"/npm/lodash?write=true&token="+secret, strings.NewReader("body-"+secret))
	for _, name := range []string{"Authorization", "Proxy-Authorization", "Cookie", "Npm-Auth-Type", "Npm-Otp", "X-Api-Key",
		"X-Forwarded-Host", "X-Forwarded-For", "X-Forwarded-Proto", "Forwarded", "Npm-Session", "Npm-Command", "Referer"} {
		req.Header.Set(name, "Bearer "+secret)
	}
	req.Header.Set("Accept", "application/vnd.npm.install-v1+json")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("If-None-Match", `"abc"`)
	req.Header.Set("If-Modified-Since", "Mon, 02 Jan 2006 15:04:05 GMT")
	req.Header.Set("Range", "bytes=0-1")
	req.Header.Set("If-Range", `"abc"`)
	req.Header.Set("User-Agent", "pnpm/11")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	requests, uris, bodies := backend.seen()
	if len(requests) != 1 || uris[0] != "/lodash" || bodies[0] != "" {
		t.Fatalf("uris=%q bodies=%q", uris, bodies)
	}
	for name, values := range requests[0].Header {
		if strings.Contains(strings.Join(values, " "), secret) {
			t.Errorf("%s carried the credential to the backend", name)
		}
	}
	host := target.Host
	want := http.Header{"Accept": {npmAbbreviated}, "Accept-Encoding": {"gzip"}, "User-Agent": {npmUserAgent}}
	for name := range requests[0].Header {
		if _, ok := want[name]; !ok {
			t.Errorf("unexpected header %s reached the backend", name)
		}
	}
	for name, values := range want {
		if got := requests[0].Header[name]; len(got) != 1 || got[0] != values[0] {
			t.Errorf("%s = %q, want %q", name, got, values)
		}
	}
	if requests[0].Host != host {
		t.Errorf("host = %q, want the registry's own, %q", requests[0].Host, host)
	}
	if strings.Contains(logs.String(), secret) {
		t.Fatalf("the gate logged the credential: %q", logs.String())
	}
}

// TestARefusedMutationNeverReachesTheBackendAndLeavesNoTokenInTheLog: `npm logout`
// puts the token in the path, so a refusal must not quote the path.
func TestARefusedMutationNeverReachesTheBackendAndLeavesNoTokenInTheLog(t *testing.T) {
	const secret = "npm_S3CRET-token"
	backend, target := newNPMBackend(t)
	var logs bytes.Buffer
	gate, _, resultsSeen := npmGate(t, target, &logs, &Counters{})
	for _, request := range []struct{ method, path string }{
		{http.MethodDelete, "/npm/-/user/token/" + secret},
		{http.MethodPut, "/npm/-/user/org.couchdb.user:" + secret},
		{http.MethodPut, "/npm/lodash"},
		{http.MethodPost, "/npm/-/npm/v1/security/advisories/bulk"},
		{http.MethodPost, "/npm/-/npm/v1/attestations/lodash@4.17.21"},
		{http.MethodDelete, "/npm/-/npm/v1/attestations/lodash@4.17.21"},
	} {
		req, _ := http.NewRequest(request.method, gate.URL+request.path, strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+secret)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "read-only") || strings.Contains(string(body), secret) {
			t.Fatalf("%s %s: status=%d body=%q", request.method, request.path, resp.StatusCode, body)
		}
	}
	if requests, _, _ := backend.seen(); len(requests) != 0 {
		t.Fatalf("a refused mutation reached the backend: %d", len(requests))
	}
	if got := resultsSeen(); len(got) != 0 {
		t.Fatalf("a refused mutation reached GitHub: %q", got)
	}
	if !strings.Contains(logs.String(), "refused") || strings.Contains(logs.String(), secret) {
		t.Fatalf("logs = %q", logs.String())
	}
}

// TestAGateWithNoNPMCacheAnswers404NotGitHub: before this route existed a path
// under /npm/ went to GitHub's results service with whatever it carried. Nothing
// may read a missing cache as a results call.
func TestAGateWithNoNPMCacheAnswers404NotGitHub(t *testing.T) {
	gate, _, resultsSeen := npmGate(t, nil, io.Discard, &Counters{})
	for _, path := range []string{"/npm/lodash", "/npm/-/ping", "/npm", "/npm/../metrics"} {
		resp, err := http.Get(gate.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: status = %d", path, resp.StatusCode)
		}
	}
	if got := resultsSeen(); len(got) != 0 {
		t.Fatalf("reached GitHub: %q", got)
	}
}

// TestTheNPMRouteLeavesEveryOtherRouteAlone keeps the rollback promise: results
// and cache calls behave exactly as they did with no npm route.
func TestTheNPMRouteLeavesEveryOtherRouteAlone(t *testing.T) {
	backend, target := newNPMBackend(t)
	cache, cacheSeen := upstream(t)
	results, resultsSeen := upstream(t)
	gate := httptest.NewServer(countingHandler(cache, results, newNPMCache(filepath.Join(t.TempDir(), "data"), target), io.Discard, &Counters{}))
	t.Cleanup(gate.Close)
	for _, path := range []string{"/download/x", "/npmish/x", "/twirp/x/y", "/lodash"} {
		resp, err := http.Get(gate.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	if requests, _, _ := backend.seen(); len(requests) != 0 {
		t.Fatalf("the npm backend saw %d requests", len(requests))
	}
	if len(cacheSeen()) != 1 || len(resultsSeen()) != 3 {
		t.Fatalf("cache=%q results=%q", cacheSeen(), resultsSeen())
	}
}

// TestTheNPMRouteRefusesABackendRedirect: a guest may follow only the address the
// node proved. A redirect from the backend would send it somewhere nobody proved.
func TestTheNPMRouteRefusesABackendRedirect(t *testing.T) {
	backend, target := newNPMBackend(t)
	backend.response = func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://evil.example/x", http.StatusFound)
	}
	gate, _, _ := npmGate(t, target, io.Discard, &Counters{})
	client := http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(gate.URL + "/npm/lodash")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || resp.Header.Get("Location") != "" {
		t.Fatalf("status=%d location=%q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

// TestTheNPMRouteDropsABackendCookie: nothing the guest is told to send back may
// come from the cache.
func TestTheNPMRouteDropsABackendCookie(t *testing.T) {
	backend, target := newNPMBackend(t)
	backend.response = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Set-Cookie", "session=x")
		_, _ = io.WriteString(w, "{}")
	}
	gate, _, _ := npmGate(t, target, io.Discard, &Counters{})
	resp, err := http.Get(gate.URL + "/npm/lodash")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.Header.Get("Set-Cookie") != "" {
		t.Fatalf("set-cookie = %q", resp.Header.Get("Set-Cookie"))
	}
}

func TestADownNPMBackendIsABadGatewayAndNotAFallback(t *testing.T) {
	backend, target := newNPMBackend(t)
	backend.Close()
	gate, _, resultsSeen := npmGate(t, target, io.Discard, &Counters{})
	resp, err := http.Get(gate.URL + "/npm/lodash")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resultsSeen(); len(got) != 0 {
		t.Fatalf("reached GitHub: %q", got)
	}
}

func TestCountersReportTheNPMRouteOnTheExistingShape(t *testing.T) {
	_, target := newNPMBackend(t)
	counters := &Counters{}
	gate, _, _ := npmGate(t, target, io.Discard, counters)
	resp, err := http.Get(gate.URL + "/npm/lodash/-/lodash-1.0.0.tgz")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	refused, _ := http.NewRequest(http.MethodPut, gate.URL+"/npm/lodash", strings.NewReader("{}"))
	resp, err = http.DefaultClient.Do(refused)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	metrics := counters.render()
	for _, want := range []string{
		`fleet_cache_gate_bytes_total{upstream="npm",direction="to_guest"} ` + strconv.Itoa(len(body)),
		`fleet_cache_gate_bytes_total{upstream="npm",direction="from_guest"} 0`,
		`fleet_cache_gate_bytes_total{upstream="cache",direction="to_guest"} 0`,
		`fleet_cache_gate_bytes_total{upstream="results",direction="to_guest"} 0`,
	} {
		if !strings.Contains(metrics, want) {
			t.Fatalf("metrics missing %q:\n%s", want, metrics)
		}
	}
}

func TestExecuteRefusesAnUnusableNPMCacheOrRegistry(t *testing.T) {
	for name, extra := range map[string][]string{
		"relative cache": {"--npm-cache", "data"},
		"unclean cache":  {"--npm-cache", "/tmp/../data"},
		"no scheme":      {"--npm-cache", "/data", "--npm-upstream", "127.0.0.1:3020"},
		"other scheme":   {"--npm-cache", "/data", "--npm-upstream", "ftp://registry.example"},
		"credentials":    {"--npm-cache", "/data", "--npm-upstream", "https://user:pass@registry.example"},
		"a path":         {"--npm-cache", "/data", "--npm-upstream", "https://registry.example/npm"},
		"unparseable":    {"--npm-cache", "/data", "--npm-upstream", "http://[::1"},
	} {
		var stderr bytes.Buffer
		code := Execute(context.Background(), append([]string{"--listen", "127.0.0.1:0", "--upstream", "http://127.0.0.1:1"}, extra...), io.Discard, &stderr)
		if code != 2 || !strings.Contains(stderr.String(), "--npm-cache") {
			t.Fatalf("%s: code=%d stderr=%q", name, code, stderr.String())
		}
	}
}

func TestExecuteServesTheNPMRouteFromItsCache(t *testing.T) {
	_, target := newNPMBackend(t)
	address := freeAddress(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	var stdout bytes.Buffer
	cache, _ := upstream(t)
	go func() {
		done <- Execute(ctx, []string{"--listen", address, "--upstream", cache.String(), "--results-upstream", cache.String(),
			"--npm-cache", filepath.Join(t.TempDir(), "data"), "--npm-upstream", target.String()}, &stdout, io.Discard)
	}()
	if body := waitFor(t, "http://"+address+"/npm/lodash"); body != "npm:/lodash" {
		t.Fatalf("body = %q", body)
	}
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(stdout.String(), "npm "+target.Redacted()) {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

// TestARegistryWithNoCacheIsNoRoute: Omarchy's old drop-in passes --npm-upstream at a Verdaccio. A
// gate restarted on it after the release must start, and must serve no npm route.
func TestARegistryWithNoCacheIsNoRoute(t *testing.T) {
	address := freeAddress(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	cache, _ := upstream(t)
	go func() {
		done <- Execute(ctx, []string{"--listen", address, "--upstream", cache.String(), "--results-upstream", cache.String(),
			"--npm-upstream", "http://127.0.0.1:3020"}, io.Discard, io.Discard)
	}()
	waitFor(t, "http://"+address+"/health")
	response, err := http.Get("http://" + address + "/npm/lodash")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	cancel()
	if code := <-done; code != 0 || response.StatusCode != http.StatusNotFound {
		t.Fatalf("code=%d status=%d", code, response.StatusCode)
	}
}

func TestNPMReadyAsksTheGatesNPMRoute(t *testing.T) {
	hits := make(chan string, 1)
	status := http.StatusOK
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits <- r.URL.Path
		w.WriteHeader(status)
	}))
	t.Cleanup(gate.Close)
	target, _ := url.Parse(gate.URL)
	public, _, _ := ed25519.GenerateKey(nil)
	verifier := Verifier{Gate: target, Public: public}
	if !verifier.NPMReady(context.Background()) {
		t.Fatal("a route answering 200 read as not ready")
	}
	if got := <-hits; got != "/npm/-/ping" {
		t.Fatalf("asked %q", got)
	}
	for _, code := range []int{http.StatusNotFound, http.StatusBadGateway, http.StatusFound} {
		status = code
		if verifier.NPMReady(context.Background()) {
			t.Fatalf("status %d read as ready", code)
		}
		<-hits
	}
}

func TestNPMReadyIsFalseWhenNothingAnswers(t *testing.T) {
	gate := httptest.NewServer(http.NotFoundHandler())
	target, _ := url.Parse(gate.URL)
	gate.Close()
	if (Verifier{Gate: target}).NPMReady(context.Background()) {
		t.Fatal("nothing answered and it read as ready")
	}
	if (Verifier{Gate: &url.URL{Scheme: "http", Host: "bad host"}}).NPMReady(context.Background()) {
		t.Fatal("an unparseable target read as ready")
	}
}

// TestAGateWithNoNPMCacheExportsNoNPMSeries keeps the off switch honest: a node that
// never enabled the route scrapes exactly the series it always did.
func TestAGateWithNoNPMCacheExportsNoNPMSeries(t *testing.T) {
	_, target := newNPMBackend(t)
	cache, _ := upstream(t)
	for name, extra := range map[string][]string{"off": nil, "on": {"--npm-cache", filepath.Join(t.TempDir(), "data"), "--npm-upstream", target.String()}} {
		t.Run(name, func(t *testing.T) {
			gateAddress, metricsAddress := freeAddress(t), freeAddress(t)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan int, 1)
			args := append([]string{"--listen", gateAddress, "--upstream", cache.String(), "--results-upstream", cache.String(),
				"--metrics-listen", metricsAddress}, extra...)
			go func() { done <- Execute(ctx, args, io.Discard, io.Discard) }()
			body := waitFor(t, "http://"+metricsAddress+"/metrics")
			cancel()
			if code := <-done; code != 0 {
				t.Fatalf("code = %d", code)
			}
			if !strings.Contains(body, `upstream="cache"`) || !strings.Contains(body, `upstream="results"`) {
				t.Fatalf("metrics = %q", body)
			}
			if got, want := strings.Contains(body, `upstream="npm"`), extra != nil; got != want {
				t.Fatalf("npm series exported = %v, want %v:\n%s", got, want, body)
			}
		})
	}
}
