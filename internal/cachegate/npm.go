package cachegate

import (
	"context"
	"net"
	"net/http"
	"regexp"
	"strings"
)

// NPMPrefix is the one prefix a gate serves its npm read-through cache under
// (ADR 0093). A guest's registry setting ends here.
const NPMPrefix = "/npm/"

// NPMPingPath is what a node and a guest ask before trusting the route.
const NPMPingPath = NPMPrefix + "-/ping"

// NPMContainerHost is the only name a guest may address the route by: rootless
// pasta's way of reaching the host (ADR 0078). Everything else must be an address.
const NPMContainerHost = "host.containers.internal"

// npmNotFound and npmRefused are the only reasons this route gives. Neither quotes
// the request: `npm logout` carries its token in the path.
const (
	npmNotFound = "not an npm cache read route"
	npmRefused  = "the npm cache is read-only and direct-LAN-only; publish, log in and audit against the registry itself"
)

// The route is an allowlist over the escaped path. Matching the escaped form means
// no percent-encoding except a scope's slash can reach the registry, so one request
// cannot be read two ways, or name a cache key twice. An unscoped name starts with a letter,
// digit or `~`, which keeps out `-` and `_session`. A scoped part may also start with
// `_`, as `@_koi/react` does. Nothing may start with a dot, which keeps out dot segments.
var (
	npmName = `(?:@[A-Za-z0-9~_][A-Za-z0-9._~-]*(?:/|%2[fF])[A-Za-z0-9~_][A-Za-z0-9._~-]*|[A-Za-z0-9~][A-Za-z0-9._~-]*)`
	// Metadata, one version or tag of it, or a tarball. A version starts with a letter or
	// digit, which keeps out `-rev` and `-/`.
	npmReads = regexp.MustCompile(`^/npm/` + npmName + `(?:/[A-Za-z0-9][A-Za-z0-9._~-]*|/-/[A-Za-z0-9~_][A-Za-z0-9._~-]*\.tgz)?$`)
	npmTags  = regexp.MustCompile(`^/npm/-/package/` + npmName + `/dist-tags$`)
)

// isNPMRoute reports whether a path belongs to this route rather than to Decide.
func isNPMRoute(path string) bool {
	return path == strings.TrimSuffix(NPMPrefix, "/") || strings.HasPrefix(path, NPMPrefix)
}

// DecideNPM judges one request to the npm route from its method, escaped path,
// Host and headers alone. Nothing it refuses reaches the cache or the registry.
func DecideNPM(method, escapedPath, host string, header http.Header) Verdict {
	if method != http.MethodGet && method != http.MethodHead {
		return Verdict{Status: http.StatusForbidden, Reason: npmRefused}
	}
	if !directLANHost(host) || arrivedThroughCloudflare(header) {
		return Verdict{Status: http.StatusForbidden, Reason: npmRefused}
	}
	if len(escapedPath) > npmMaxPath || (escapedPath != NPMPingPath && !npmReads.MatchString(escapedPath) && !npmTags.MatchString(escapedPath)) {
		return Verdict{Status: http.StatusNotFound, Reason: npmNotFound}
	}
	return Verdict{Allow: true, Upstream: NPM}
}

// directLANHost is the Host a guest may address the route by, which must never be a
// tunnel's hostname.
func directLANHost(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	if strings.EqualFold(host, NPMContainerHost) {
		return true
	}
	host = strings.Trim(host, "[]")
	return DirectLANAddress(host) || LoopbackAddress(host)
}

// DirectLANAddress reports whether host is an RFC 1918 or unique-local IP literal.
// A hostname is not one: a name is what a tunnel answers to, and what resolves
// differently on a network nobody proved.
func DirectLANAddress(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsPrivate()
}

// LoopbackAddress reports whether host is a loopback IP literal.
func LoopbackAddress(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// arrivedThroughCloudflare catches a tunnel configured to rewrite Host: cloudflared
// is for admin and health, and carries no cache data (ADR 0093).
func arrivedThroughCloudflare(header http.Header) bool {
	for name := range header {
		if strings.HasPrefix(name, "Cf-") || name == "Cdn-Loop" {
			return true
		}
	}
	return false
}

// NPMReady reports whether the gate's npm route answers now. It says nothing about
// who answered: Verified is the proof of that, and this is asked only after it.
func (v Verifier) NPMReady(ctx context.Context) bool {
	timeout := v.Timeout
	if timeout <= 0 {
		timeout = verifyTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, v.Gate.JoinPath(NPMPingPath).String(), nil)
	if err != nil {
		return false
	}
	response, err := directClient().Do(request) // #nosec G704 -- the URL is the node's pinned gate.
	if err != nil {
		return false
	}
	_ = response.Body.Close()
	return response.StatusCode == http.StatusOK
}

// directClient talks to the address it is given and to nothing else: no proxy, and
// no redirect. Only the address the node pinned may answer.
func directClient() *http.Client {
	return &http.Client{Transport: &http.Transport{DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
