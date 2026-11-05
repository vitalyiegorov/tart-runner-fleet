package cachegate

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
)

// NPMPrefix is the one prefix a gate serves its npm read-through cache under
// (ADR 0092). A guest's registry setting and the tarball URLs Verdaccio
// advertises both end here.
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
// no percent-encoding except a scope's slash can reach the backend, so the gate and
// Verdaccio cannot read one request two ways. A name starts with a letter, digit or
// `~`, which keeps out `-`, `_session`, and every dot segment.
var (
	npmName    = `(?:@[A-Za-z0-9~][A-Za-z0-9._~-]*(?:/|%2[fF]))?[A-Za-z0-9~][A-Za-z0-9._~-]*`
	npmReads   = regexp.MustCompile(`^/npm/` + npmName + `(?:/-/[A-Za-z0-9~][A-Za-z0-9._~-]*\.tgz)?$`)
	npmTags    = regexp.MustCompile(`^/npm/-/package/` + npmName + `/dist-tags$`)
	npmHeaders = []string{"Accept", "Accept-Encoding", "If-Modified-Since", "If-None-Match", "If-Range", "Range", "User-Agent"}
)

// isNPMRoute reports whether a path belongs to this route rather than to Decide.
func isNPMRoute(path string) bool {
	return path == strings.TrimSuffix(NPMPrefix, "/") || strings.HasPrefix(path, NPMPrefix)
}

// DecideNPM judges one request to the npm route from its method, escaped path,
// Host and headers alone. It decides independently of Verdaccio's own config, so a
// mistake in that config cannot open a write path.
func DecideNPM(method, escapedPath, host string, header http.Header) Verdict {
	if method != http.MethodGet && method != http.MethodHead {
		return Verdict{Status: http.StatusForbidden, Reason: npmRefused}
	}
	if !directLANHost(host) || arrivedThroughCloudflare(header) {
		return Verdict{Status: http.StatusForbidden, Reason: npmRefused}
	}
	if escapedPath != NPMPingPath && !npmReads.MatchString(escapedPath) && !npmTags.MatchString(escapedPath) {
		return Verdict{Status: http.StatusNotFound, Reason: npmNotFound}
	}
	return Verdict{Allow: true, Upstream: NPM}
}

// directLANHost is the Host a guest may address the route by. It is what Verdaccio
// builds tarball URLs from, so it must never be a tunnel's hostname.
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
// is for admin and health, and carries no cache data (ADR 0092).
func arrivedThroughCloudflare(header http.Header) bool {
	for name := range header {
		if strings.HasPrefix(name, "Cf-") || name == "Cdn-Loop" {
			return true
		}
	}
	return false
}

// npmProxy forwards an allowed read to Verdaccio. It forwards only the headers a
// registry read needs, no query and no body, so a credential a client attached under
// any name stops here. It keeps the Host the guest used, which is how one Verdaccio
// advertises tarballs correctly to Tart guests at the LAN address and to Podman
// guests at host.containers.internal.
func npmProxy(target *url.URL) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			escaped := strings.TrimPrefix(r.In.URL.EscapedPath(), strings.TrimSuffix(NPMPrefix, "/"))
			r.Out.URL.Scheme, r.Out.URL.Host = target.Scheme, target.Host
			// DecideNPM admitted only unreserved bytes and %2f, so this cannot fail.
			r.Out.URL.Path, _ = url.PathUnescape(escaped)
			r.Out.URL.RawPath, r.Out.URL.RawQuery = escaped, ""
			r.Out.Body, r.Out.ContentLength = http.NoBody, 0
			r.Out.Header = http.Header{"X-Forwarded-Host": {r.In.Host}, "X-Forwarded-Proto": {"http"}}
			for _, name := range npmHeaders {
				if values, ok := r.In.Header[name]; ok {
					r.Out.Header[name] = values
				}
			}
			r.Out.Host = r.In.Host
		},
		// No proxy, and no silent decompression of what the guest asked to receive
		// compressed. Tarball bursts reuse connections to the loopback backend.
		Transport: &http.Transport{DisableCompression: true, MaxIdleConnsPerHost: 64, IdleConnTimeout: idleTimeout},
		ModifyResponse: func(response *http.Response) error {
			if response.StatusCode >= http.StatusMultipleChoices && response.StatusCode < http.StatusBadRequest &&
				response.StatusCode != http.StatusNotModified {
				return errors.New("the npm cache answered with a redirect")
			}
			response.Header.Del("Set-Cookie")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]string{"code": "unavailable", "msg": "the npm cache is unavailable"})
		},
	}
}

// validNPMUpstream is a plain http URL on a literal loopback address, naming nothing
// but a host and port: the backend must stay unreachable from guests, and the gate
// is the only thing that talks to it.
func validNPMUpstream(raw string) (*url.URL, bool) {
	parsed, ok := absoluteURL(raw)
	if !ok || parsed.Scheme != "http" || parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || !loopbackAddress(parsed.Host) {
		return nil, false
	}
	return parsed, true
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
