package cachegate

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// defaultNPMUpstream is the registry the cache reads through to.
	defaultNPMUpstream = "https://registry.npmjs.org"
	// npmMetadataTTL is how long a document is served before it is revalidated.
	npmMetadataTTL = 10 * time.Minute
	// npmFetchDeadline bounds one fetch whatever it is doing. bodyStallTimeout cuts one that
	// stops making progress long before this.
	npmFetchDeadline = 15 * time.Minute
	// npmHeaderLimit bounds an object's stored header, so a corrupt file costs one small read.
	npmHeaderLimit = 4096
	// npmMaxPath is well past npm's own 214-byte name limit plus a version or tarball name.
	npmMaxPath = 512
	// npmMaxConcurrent bounds what this route can hold open, so that however it fails it cannot
	// exhaust the process that also carries every job's Actions cache and artifacts.
	npmMaxConcurrent = 1024
	// npmMaxConnections bounds the connections to the registry.
	npmMaxConnections = 64
	// npmAbbreviated is the one spelling of "the abbreviated document, please" that is asked for.
	npmAbbreviated = "application/vnd.npm.install-v1+json; q=1.0, application/json; q=0.8, */*"
	npmUserAgent   = "tart-runner-fleet-cache-gate"
)

// npmMaxObject bounds what is stored. Past it an object is passed through instead, as a stream
// that holds no memory. It is a variable so that a test need not write two gigabytes.
var npmMaxObject int64 = 2 << 30

var (
	// errNPMRegistry means the registry did not deliver: stale metadata may answer instead.
	errNPMRegistry = errors.New("the registry did not deliver")
	// errNPMDisk means this object cannot be stored: the request passes straight through.
	errNPMDisk = errors.New("the object cannot be stored")
)

// npmStatus is a real answer from the registry, such as a 404, to hand on as it is.
type npmStatus int

func (s npmStatus) Error() string { return fmt.Sprintf("the registry answered %d", int(s)) }

// npmCache is the gate's npm read-through cache (ADR 0093). Tarballs are immutable. Metadata is
// kept as the registry's own bytes for npmMetadataTTL and then revalidated with its ETag. Nothing
// here parses a document, so no request can cost more than a copy buffer, and integrity stays
// with the client, which checks what it receives against its lockfile.
//
// Each object is one file in one flat directory, named for the hash of its key: a line of JSON
// with its headers, then its bytes. A file's mtime is when it was last known fresh. It appears
// whole, by rename, or not at all. A directory that cannot be written is simply a cache that does
// not grow, which is how the prune freezes it.
type npmCache struct {
	dir      string
	registry string
	client   *http.Client
	logger   *slog.Logger
	counters *Counters
	slots    chan struct{}
	mu       sync.Mutex
	flights  map[string]*npmFlight
}

// npmFlight is one upstream fetch, which every other request for that key waits on.
type npmFlight struct {
	done chan struct{}
	err  error
}

// npmObject is what one request is for. Accept and encoding are reduced to the few forms that are
// really different documents, so a guest's spelling of a header cannot multiply the cache.
type npmObject struct {
	escaped, accept, encoding, file string
	immutable                       bool
}

// npmEntry is the first line of a stored object.
type npmEntry struct {
	ContentType     string `json:"contentType,omitempty"`
	ContentEncoding string `json:"contentEncoding,omitempty"`
	ETag            string `json:"etag,omitempty"`
}

// validNPMUpstream names a registry and nothing else.
func validNPMUpstream(raw string) (*url.URL, bool) {
	parsed, ok := absoluteURL(raw)
	return parsed, ok && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.User == nil && (parsed.Path == "" || parsed.Path == "/")
}

// newNPMTransport has its own connections and no proxy, so that nothing about npmjs can touch the
// transports the Actions routes use. It leaves compression alone, because the bytes are the point.
func newNPMTransport() http.RoundTripper {
	return &http.Transport{DisableCompression: true, MaxConnsPerHost: npmMaxConnections, MaxIdleConnsPerHost: npmMaxConnections,
		IdleConnTimeout: idleTimeout, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: bodyStallTimeout}
}

// newNPMCache does not fail: a directory it cannot create is a failed disk, which passes through.
func newNPMCache(dir string, registry *url.URL) *npmCache {
	_ = os.MkdirAll(dir, 0o700)
	return &npmCache{dir: dir, registry: strings.TrimSuffix(registry.String(), "/"), flights: map[string]*npmFlight{},
		slots: make(chan struct{}, npmMaxConcurrent), client: &http.Client{Transport: newNPMTransport(),
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (c *npmCache) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	default:
		w.Header().Set("Retry-After", "5")
		npmFail(w, http.StatusServiceUnavailable)
		return
	}
	// A panic here must stay in this request. It is also caught explicitly, so the slot and any
	// waiters are released and the guest gets an answer it will retry.
	defer func() {
		if p := recover(); p != nil {
			if p == http.ErrAbortHandler { //nolint:errorlint,err113 // net/http's own sentinel, compared as it documents.
				panic(p)
			}
			c.logger.Error("npm cache request panicked", "type", fmt.Sprintf("%T", p))
			npmFail(w, http.StatusBadGateway)
		}
	}()
	if r.URL.EscapedPath() == NPMPingPath {
		_, _ = io.WriteString(w, "{}\n")
		return
	}
	o := c.object(r)
	if c.serveStored(w, r, o, true) {
		return
	}
	err := c.fetchOnce(r.Context(), o)
	var status npmStatus
	switch {
	case errors.As(err, &status):
		npmFail(w, int(status))
	case errors.Is(err, errNPMDisk):
		c.passThrough(w, r, o)
	case c.serveStored(w, r, o, false): // What was just fetched, or a stale copy if that failed.
	case err == nil: // Evicted, or unreadable, in the instant since it was stored.
		c.passThrough(w, r, o)
	default:
		npmFail(w, http.StatusBadGateway)
	}
}

func (c *npmCache) object(r *http.Request) npmObject {
	escaped := strings.TrimPrefix(r.URL.EscapedPath(), strings.TrimSuffix(NPMPrefix, "/"))
	key := strings.NewReplacer("%2f", "/", "%2F", "/").Replace(escaped)
	o := npmObject{escaped: escaped, immutable: strings.Contains(key, "/-/") && strings.HasSuffix(key, ".tgz")}
	if !o.immutable {
		o.accept = "application/json"
		if strings.Contains(r.Header.Get("Accept"), "install-v1") {
			o.accept = npmAbbreviated
		}
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			o.encoding = "gzip"
		}
	}
	sum := sha256.Sum256([]byte(key + "\x00" + o.accept + "\x00" + o.encoding))
	o.file = filepath.Join(c.dir, hex.EncodeToString(sum[:]))
	return o
}

// readNPMEntry reads a stored object's header. Anything unreadable is simply not there.
func readNPMEntry(f io.Reader) (npmEntry, int64, bool) {
	line, err := bufio.NewReaderSize(f, npmHeaderLimit).ReadSlice('\n')
	var entry npmEntry
	if err != nil || json.Unmarshal(line, &entry) != nil {
		return npmEntry{}, 0, false
	}
	return entry, int64(len(line)), true
}

// serveStored answers from disk if it can. An object reopened after its name was reused, or
// evicted while being read, keeps its bytes for this reader, so neither can corrupt a response.
func (c *npmCache) serveStored(w http.ResponseWriter, r *http.Request, o npmObject, mustBeFresh bool) bool {
	f, err := os.Open(o.file)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	entry, offset, ok := readNPMEntry(f)
	info, err := f.Stat()
	if !ok || err != nil || (mustBeFresh && !o.immutable && now().Sub(info.ModTime()) >= npmMetadataTTL) {
		return false
	}
	header := w.Header()
	header.Set("Content-Type", entry.ContentType)
	if entry.ContentEncoding != "" {
		header.Set("Content-Encoding", entry.ContentEncoding)
	}
	if entry.ETag != "" {
		header.Set("ETag", entry.ETag)
	}
	http.ServeContent(w, r, "", time.Time{}, io.NewSectionReader(f, offset, info.Size()-offset))
	return true
}

// fetchOnce makes one upstream fetch per key however many requests want it. The fetch runs in the
// request that started it, so a panic stays in that request, and the deferred release means its
// waiters always wake: they find no fresh object and answer as if the registry had failed.
func (c *npmCache) fetchOnce(ctx context.Context, o npmObject) error {
	c.mu.Lock()
	if flight, ok := c.flights[o.file]; ok {
		c.mu.Unlock()
		select {
		case <-flight.done:
			return flight.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	flight := &npmFlight{done: make(chan struct{}), err: errNPMRegistry}
	c.flights[o.file] = flight
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.flights, o.file)
		c.mu.Unlock()
		close(flight.done)
	}()
	flight.err = c.fetch(ctx, o)
	return flight.err
}

func (c *npmCache) inFlight() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.flights)
}

func (c *npmCache) request(ctx context.Context, method string, o npmObject) *http.Request {
	// Built from scratch, so no guest header, query or body can reach the registry. The URL is a
	// validated registry and a path DecideNPM admitted, so this cannot fail.
	request, _ := http.NewRequestWithContext(ctx, method, c.registry+o.escaped, nil)
	request.Header.Set("User-Agent", npmUserAgent)
	if o.accept != "" {
		request.Header.Set("Accept", o.accept)
	}
	if o.encoding != "" {
		request.Header.Set("Accept-Encoding", o.encoding)
	}
	return request
}

// fetch brings one object up to date. It outlives the guest that asked, because the next job wants
// those bytes too. It is bounded by npmFetchDeadline and by the stall timer.
func (c *npmCache) fetch(ctx context.Context, o npmObject) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), npmFetchDeadline)
	defer cancel()
	stalled := time.AfterFunc(bodyStallTimeout, cancel)
	defer stalled.Stop()
	request := c.request(ctx, http.MethodGet, o)
	if f, err := os.Open(o.file); err == nil {
		entry, _, _ := readNPMEntry(f)
		_ = f.Close()
		if entry.ETag != "" {
			request.Header.Set("If-None-Match", entry.ETag)
		}
	}
	response, err := c.client.Do(request)
	if err != nil {
		return errNPMRegistry
	}
	defer func() { _ = response.Body.Close() }()
	switch code := response.StatusCode; {
	case code == http.StatusOK:
		return c.store(o, response, stalled)
	case code == http.StatusNotModified:
		// A failure only means the next request asks again.
		_ = os.Chtimes(o.file, now(), now())
		return nil
	case code < http.StatusBadRequest || code == http.StatusTooManyRequests || code >= http.StatusInternalServerError:
		return errNPMRegistry
	default:
		return npmStatus(code)
	}
}

// npmProgress counts what the registry delivers, renews the stall timer, and remembers a read
// error, because io.Copy cannot say which side of it failed.
type npmProgress struct {
	body    io.Reader
	stalled *time.Timer
	counted *Counters
	err     error
}

func (p *npmProgress) Read(b []byte) (int, error) {
	p.stalled.Reset(bodyStallTimeout)
	n, err := p.body.Read(b)
	p.counted.fromRegistry.Add(int64(n))
	if err != nil && !errors.Is(err, io.EOF) {
		p.err = err
	}
	return n, err
}

// store writes one object atomically: to a temp file, durably, and then by rename, so that a crash
// at any point leaves the whole object or none of it.
func (c *npmCache) store(o npmObject, response *http.Response, stalled *time.Timer) error {
	line, _ := json.Marshal(npmEntry{response.Header.Get("Content-Type"), response.Header.Get("Content-Encoding"), response.Header.Get("ETag")})
	if response.ContentLength > npmMaxObject || len(line) >= npmHeaderLimit {
		return errNPMDisk
	}
	tmp, err := os.CreateTemp(c.dir, ".tmp-")
	if err != nil {
		return errNPMDisk
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()
	body := &npmProgress{body: response.Body, stalled: stalled, counted: c.counters}
	written, err := io.Copy(tmp, io.MultiReader(bytes.NewReader(append(line, '\n')), io.LimitReader(body, npmMaxObject+1)))
	if body.err != nil {
		return errNPMRegistry
	}
	if err != nil || written > int64(len(line))+1+npmMaxObject {
		return errNPMDisk
	}
	for _, step := range []func() error{tmp.Sync, tmp.Close, func() error { return os.Rename(tmp.Name(), o.file) }} {
		if step() != nil {
			return errNPMDisk
		}
	}
	return nil
}

// passThrough answers straight from the registry when the cache cannot help, as a stream, tied to
// the guest's own request. It carries the guest's method, so a HEAD stays one.
func (c *npmCache) passThrough(w http.ResponseWriter, r *http.Request, o npmObject) {
	response, err := c.client.Do(c.request(r.Context(), r.Method, o))
	if err != nil {
		npmFail(w, http.StatusBadGateway)
		return
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode >= http.StatusMultipleChoices && response.StatusCode < http.StatusBadRequest {
		npmFail(w, http.StatusBadGateway)
		return
	}
	for _, name := range []string{"Content-Type", "Content-Encoding", "Content-Length", "ETag"} {
		if value := response.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, countedBody{response.Body, &c.counters.fromRegistry})
}

func npmFail(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": "unavailable", "msg": "the npm cache could not answer: " + http.StatusText(status)})
}
