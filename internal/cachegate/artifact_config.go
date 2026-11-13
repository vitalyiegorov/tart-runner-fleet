package cachegate

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type artifactConfig struct {
	dir, origin, upstream, repos, budgetPath string
	cap, floor, cachePeak                    int64
}

func artifactFlags(f *flag.FlagSet) *artifactConfig {
	c := &artifactConfig{}
	f.StringVar(&c.dir, "artifacts-dir", "", "private artifact owner directory; empty disables owner")
	f.StringVar(&c.origin, "artifacts-origin", "http://host.containers.internal:3017", "guest-facing gate origin for signed artifact URLs")
	f.StringVar(&c.upstream, "artifacts-upstream", "", "loopback SSH local forward to the single artifact owner")
	f.StringVar(&c.repos, "artifacts-repositories", "", "comma-separated allowed numeric repository IDs (owner only)")
	f.StringVar(&c.budgetPath, "write-budget-path", "", "existing filesystem shared by local cache, npm and artifacts")
	f.Int64Var(&c.cap, "artifacts-max-bytes", 20<<30, "artifact logical byte cap (includes staged blocks)")
	f.Int64Var(&c.floor, "write-min-free-bytes", 60<<30, "physical disk reserve; must meet node minFreeDiskGb")
	f.Int64Var(&c.cachePeak, "cache-write-peak-bytes", 60<<30, "maximum cache commit copy peak; must cover cache byte cap")
	return c
}
func originURL(raw string) (*url.URL, bool) {
	u, err := url.Parse(raw)
	return u, err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil && (u.Path == "" || u.Path == "/") && u.RawQuery == "" && u.Fragment == ""
}
func (c *artifactConfig) valid() bool {
	if c.dir == "" && c.upstream == "" {
		return c.repos == "" && c.budgetPath == ""
	}
	if c.dir != "" && c.upstream != "" || c.cap <= 0 || c.cap > 1<<50 || c.floor <= 0 || c.cachePeak <= 0 || !filepath.IsAbs(c.budgetPath) {
		return false
	}
	if c.dir != "" {
		_, ok := originURL(c.origin)
		return ok && filepath.IsAbs(c.dir) && len(c.repositoryIDs()) > 0
	}
	target, ok := originURL(c.upstream)
	if !ok || target.Scheme != "http" || !loopbackAddress(target.Host) || target.Port() == "0" || c.repos != "" {
		return false
	}
	return true
}
func (c *artifactConfig) repositoryIDs() map[string]bool {
	result := map[string]bool{}
	for _, id := range strings.Split(c.repos, ",") {
		n, err := strconv.ParseUint(id, 10, 64)
		if err != nil || n == 0 {
			return nil
		}
		result[id] = true
	}
	return result
}
func (c *artifactConfig) open(npm *npmCache, counters *Counters) (http.Handler, *writeBudget, func(), error) {
	closeStore := func() {}
	if c.dir == "" && c.upstream == "" {
		return nil, nil, closeStore, nil
	}
	budget := &writeBudget{floor: c.floor, probe: func() (int64, error) { return artifactFreeSpace(c.budgetPath) }}
	if _, err := os.Stat(c.budgetPath); err != nil {
		return nil, nil, closeStore, err
	}
	if npm != nil {
		if !sameArtifactFilesystem(c.budgetPath, npm.dir) {
			return nil, nil, closeStore, errArtifactInput
		}
		npm.budget = budget
	}
	if c.upstream != "" {
		target, _ := url.Parse(c.upstream)
		return artifactForwarder(target), budget, closeStore, nil
	}
	release, err := budget.reserve(8 << 20)
	if err != nil {
		return nil, nil, closeStore, err
	}
	defer release()
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return nil, nil, closeStore, err
	}
	if !sameArtifactFilesystem(c.budgetPath, c.dir) {
		return nil, nil, closeStore, errArtifactInput
	}
	s, err := openArtifactStore(filepath.Join(c.dir, "artifacts.db"), c.cap, time.Now, budget)
	if err != nil {
		return nil, nil, closeStore, err
	}
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: &http.Transport{Proxy: nil, TLSHandshakeTimeout: 5 * time.Second, IdleConnTimeout: idleTimeout}}
	s.metrics = &artifactMetrics{}
	counters.artifactStats = s.metrics
	s.observe()
	stopPruning := artifactMaintenance(s)
	closeStore = func() { stopPruning(); _ = s.db.Close() }
	v := &artifactVerifier{url: artifactIssuer + "/.well-known/jwks", client: client, clock: time.Now}
	return artifactHandler(s, v, strings.TrimSuffix(c.origin, "/"), c.repositoryIDs()), budget, closeStore, nil
}

func withArtifacts(next, artifacts http.Handler, budget *writeBudget, cachePeak int64, counters *Counters) http.Handler {
	if artifacts == nil {
		return next
	}
	counters.artifactsEnabled = true
	counters.writeBudget = budget
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if artifactRoute(r.URL.Path) {
			r.Body = &progressBody{ReadCloser: countedBody{r.Body, &counters.fromGuest[Artifacts]}, control: http.NewResponseController(w)}
			artifacts.ServeHTTP(countedWriter{w, &counters.toGuest[Artifacts]}, r)
			return
		}
		// Cache signed URLs bypass bearer auth just as artifact capabilities do. Guard
		// every mutating cache route, including unknown-length block PUTs.
		if writePaths[r.URL.Path] || hasPrefix(r.URL.Path, []string{"/upload/", "/devstoreaccount1/"}) && r.Method != http.MethodGet && r.Method != http.MethodHead {
			const limit = 64 << 20
			peak := int64(limit * 3)
			if r.URL.Query().Get("comp") == "blocklist" || r.URL.Path == cacheService+"FinalizeCacheEntryUpload" {
				peak += cachePeak
			}
			release, err := budget.reserve(peak)
			if err != nil {
				artifactError(w, 503)
				return
			}
			defer release()
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
			if err != nil {
				artifactError(w, 400)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
		}
		next.ServeHTTP(w, r)
	})
}

// Reject accidentally pointing the SSH forward back into this gate.
func artifactForwardLoop(listen, upstream string) bool {
	if upstream == "" {
		return false
	}
	u, _ := url.Parse(upstream)
	_, localPort, err := net.SplitHostPort(listen)
	return err == nil && u != nil && u.Port() == localPort
}

func artifactConfigError(w io.Writer) {
	_, _ = fmt.Fprintln(w, "cache-gate: invalid artifact configuration (owner or loopback SSH forward, with write budget)")
}
