package cachegate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func slogTo(w io.Writer) *slog.Logger { return slog.New(slog.NewTextHandler(w, nil)) }

// registryStub stands in for npmjs. Its reply is set per test, and it records what reached it.
type registryStub struct {
	*httptest.Server
	hits    atomic.Int64
	mu      sync.Mutex
	headers []http.Header
	respond func(http.ResponseWriter, *http.Request)
}

func newRegistryStub(t *testing.T, respond func(http.ResponseWriter, *http.Request)) (*registryStub, *url.URL) {
	t.Helper()
	stub := &registryStub{respond: respond}
	stub.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.hits.Add(1)
		stub.mu.Lock()
		stub.headers = append(stub.headers, r.Header.Clone())
		respond := stub.respond
		stub.mu.Unlock()
		respond(w, r)
	}))
	t.Cleanup(stub.Close)
	target, err := url.Parse(stub.URL)
	if err != nil {
		t.Fatal(err)
	}
	return stub, target
}

func (s *registryStub) set(respond func(http.ResponseWriter, *http.Request)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.respond = respond
}

func (s *registryStub) lastHeader(t *testing.T, name string) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.headers) == 0 {
		t.Fatal("nothing reached the registry")
	}
	return s.headers[len(s.headers)-1].Get(name)
}

func answer(body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
}

// npmFixture is a whole gate with an npm cache in front of a registry stub.
type npmFixture struct {
	gate     *httptest.Server
	dir      string
	counters *Counters
	cache    *npmCache
}

func newNPMFixture(t *testing.T, registry *url.URL) *npmFixture {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	counters := &Counters{}
	cache := newNPMCache(dir, registry)
	actions, _ := upstream(t)
	results, _ := upstream(t)
	gate := httptest.NewServer(countingHandler(actions, results, cache, io.Discard, counters))
	t.Cleanup(gate.Close)
	return &npmFixture{gate: gate, dir: dir, counters: counters, cache: cache}
}

func (f *npmFixture) get(t *testing.T, path string, header ...string) (int, string, http.Header) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, f.gate.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index+1 < len(header); index += 2 {
		request.Header.Set(header[index], header[index+1])
	}
	response, err := http.DefaultTransport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	body, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(body), response.Header
}

func (f *npmFixture) files(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(f.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// breakDisk leaves a file where the directory belongs: nothing can be read, created or stored.
func (f *npmFixture) breakDisk(t *testing.T) {
	t.Helper()
	if err := os.RemoveAll(f.dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.dir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *npmFixture) age(t *testing.T, by time.Duration) {
	t.Helper()
	for _, name := range f.files(t) {
		when := time.Now().Add(-by)
		if err := os.Chtimes(filepath.Join(f.dir, name), when, when); err != nil {
			t.Fatal(err)
		}
	}
}

const (
	testTarball  = "/npm/lodash/-/lodash-4.17.21.tgz"
	testMetadata = "/npm/lodash"
)

func TestATarballIsImmutable(t *testing.T) {
	registry, target := newRegistryStub(t, answer("tarball-bytes"))
	fixture := newNPMFixture(t, target)
	for range 3 {
		if status, body, _ := fixture.get(t, testTarball); status != http.StatusOK || body != "tarball-bytes" {
			t.Fatalf("status=%d body=%q", status, body)
		}
	}
	fixture.age(t, 365*24*time.Hour)
	if status, body, _ := fixture.get(t, testTarball); status != http.StatusOK || body != "tarball-bytes" {
		t.Fatalf("after a year: status=%d body=%q", status, body)
	}
	if got := registry.hits.Load(); got != 1 {
		t.Fatalf("the registry was asked %d times for an immutable tarball", got)
	}
}

func TestMetadataIsServedAsTheRegistrysOwnBytes(t *testing.T) {
	const raw = "\x1f\x8bnot-json-at-all"
	_, target := newRegistryStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.npm.install-v1+json")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("ETag", `W/"one"`)
		_, _ = io.WriteString(w, raw)
	})
	fixture := newNPMFixture(t, target)
	for range 2 {
		status, body, header := fixture.get(t, testMetadata, "Accept-Encoding", "gzip, deflate")
		if status != http.StatusOK || body != raw || header.Get("Content-Encoding") != "gzip" ||
			header.Get("Content-Type") != "application/vnd.npm.install-v1+json" || header.Get("ETag") != `W/"one"` {
			t.Fatalf("status=%d body=%q header=%v", status, body, header)
		}
	}
}

func TestMetadataIsFreshForTenMinutes(t *testing.T) {
	registry, target := newRegistryStub(t, answer("{}"))
	fixture := newNPMFixture(t, target)
	fixture.get(t, testMetadata)
	fixture.age(t, npmMetadataTTL-time.Minute)
	fixture.get(t, testMetadata)
	if got := registry.hits.Load(); got != 1 {
		t.Fatalf("fresh metadata reached the registry %d times", got)
	}
	fixture.age(t, npmMetadataTTL+time.Minute)
	fixture.get(t, testMetadata)
	if got := registry.hits.Load(); got != 2 {
		t.Fatalf("stale metadata reached the registry %d times, want a second look", got)
	}
}

// TestAnExpiredDocumentRevalidatesWithItsETag is the path the prototype never exercised.
func TestAnExpiredDocumentRevalidatesWithItsETag(t *testing.T) {
	registry, target := newRegistryStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `W/"one"`)
		_, _ = io.WriteString(w, "first")
	})
	fixture := newNPMFixture(t, target)
	fixture.get(t, testMetadata)
	if got := registry.lastHeader(t, "If-None-Match"); got != "" {
		t.Fatalf("a first fetch sent If-None-Match %q", got)
	}
	fixture.age(t, npmMetadataTTL+time.Minute)
	registry.set(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != `W/"one"` {
			t.Errorf("revalidated with %q", r.Header.Get("If-None-Match"))
		}
		w.WriteHeader(http.StatusNotModified)
	})
	before := fixture.counters.fromRegistry.Load()
	if status, body, _ := fixture.get(t, testMetadata); status != http.StatusOK || body != "first" {
		t.Fatalf("after a 304: status=%d body=%q", status, body)
	}
	if got := fixture.counters.fromRegistry.Load() - before; got != 0 {
		t.Fatalf("a 304 moved %d bytes", got)
	}
	hits := registry.hits.Load()
	if status, body, _ := fixture.get(t, testMetadata); status != http.StatusOK || body != "first" || registry.hits.Load() != hits {
		t.Fatalf("a 304 did not renew the ten minutes: status=%d body=%q hits=%d->%d", status, body, hits, registry.hits.Load())
	}
	fixture.age(t, npmMetadataTTL+time.Minute)
	registry.set(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `W/"two"`)
		_, _ = io.WriteString(w, "second")
	})
	if status, body, header := fixture.get(t, testMetadata); status != http.StatusOK || body != "second" || header.Get("ETag") != `W/"two"` {
		t.Fatalf("a changed document: status=%d body=%q etag=%q", status, body, header.Get("ETag"))
	}
}

func TestAGuestsOwnETagGetsA304FromTheCache(t *testing.T) {
	registry, target := newRegistryStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `W/"one"`)
		_, _ = io.WriteString(w, "body")
	})
	fixture := newNPMFixture(t, target)
	fixture.get(t, testMetadata)
	status, body, _ := fixture.get(t, testMetadata, "If-None-Match", `W/"one"`)
	if status != http.StatusNotModified || body != "" || registry.hits.Load() != 1 {
		t.Fatalf("status=%d body=%q hits=%d", status, body, registry.hits.Load())
	}
}

func TestStaleMetadataIsServedIfTheRegistryFails(t *testing.T) {
	failures := map[string]func(*registryStub){
		"a 500": func(r *registryStub) {
			r.set(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
		},
		"a 429": func(r *registryStub) {
			r.set(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTooManyRequests) })
		},
		"a redirect": func(r *registryStub) {
			r.set(func(w http.ResponseWriter, q *http.Request) {
				http.Redirect(w, q, "https://evil.example/", http.StatusFound)
			})
		},
		"a cut body": func(r *registryStub) { r.set(cutBody) },
		"it is gone": func(r *registryStub) { r.Close() },
	}
	for name, fail := range failures {
		t.Run(name, func(t *testing.T) {
			registry, target := newRegistryStub(t, answer("kept"))
			fixture := newNPMFixture(t, target)
			fixture.get(t, testMetadata)
			fixture.age(t, npmMetadataTTL+time.Minute)
			fail(registry)
			if status, body, _ := fixture.get(t, testMetadata); status != http.StatusOK || body != "kept" {
				t.Fatalf("status=%d body=%q", status, body)
			}
		})
	}
}

// cutBody promises more than it sends, which is what a dropped upstream looks like.
func cutBody(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Length", "1000")
	_, _ = io.WriteString(w, "partial")
	panic(http.ErrAbortHandler)
}

func TestAFailureWithNothingCachedIsABadGateway(t *testing.T) {
	registry, target := newRegistryStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	fixture := newNPMFixture(t, target)
	for _, path := range []string{testMetadata, testTarball} {
		if status, _, _ := fixture.get(t, path); status != http.StatusBadGateway {
			t.Fatalf("%s: status=%d", path, status)
		}
	}
	registry.set(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	if status, _, _ := fixture.get(t, testMetadata); status != http.StatusNotFound {
		t.Fatalf("a real 404: status=%d", status)
	}
	if got := fixture.files(t); len(got) != 0 {
		t.Fatalf("a failure left files: %q", got)
	}
}

func TestAFailedDiskPassesStraightThroughToTheRegistry(t *testing.T) {
	for name, path := range map[string]string{"tarball": testTarball, "metadata": testMetadata} {
		t.Run(name, func(t *testing.T) {
			registry, target := newRegistryStub(t, answer("from-the-registry"))
			fixture := newNPMFixture(t, target)
			fixture.breakDisk(t)
			for range 2 {
				if status, body, _ := fixture.get(t, path); status != http.StatusOK || body != "from-the-registry" {
					t.Fatalf("status=%d body=%q", status, body)
				}
			}
			if got := registry.hits.Load(); got < 2 {
				t.Fatalf("hits=%d", got)
			}
		})
	}
}

// TestAFrozenCacheKeepsServingAndStopsGrowing is what the prune does when it cannot hold the
// limits: it takes away write permission, which this path treats as a failed disk.
func TestAFrozenCacheKeepsServingAndStopsGrowing(t *testing.T) {
	registry, target := newRegistryStub(t, answer("body"))
	fixture := newNPMFixture(t, target)
	fixture.get(t, testTarball)
	before := fixture.files(t)
	if err := os.Chmod(fixture.dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(fixture.dir, 0o700) })
	hits := registry.hits.Load()
	if status, body, _ := fixture.get(t, testTarball); status != http.StatusOK || body != "body" || registry.hits.Load() != hits {
		t.Fatalf("a cached object: status=%d body=%q hits=%d", status, body, registry.hits.Load())
	}
	if status, body, _ := fixture.get(t, "/npm/other/-/other-1.0.0.tgz"); status != http.StatusOK || body != "body" {
		t.Fatalf("a new object: status=%d body=%q", status, body)
	}
	if got := fixture.files(t); len(got) != len(before) {
		t.Fatalf("a frozen cache grew: %q", got)
	}
}

// TestPassThroughFailsSafely: the store fails and the second request, the pass-through itself,
// gets a redirect or a dead connection. The guest gets a bad gateway and no address to follow.
func TestPassThroughFailsSafely(t *testing.T) {
	for name, second := range map[string]func(http.ResponseWriter, *http.Request){
		"a redirect": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://evil.example/", http.StatusFound)
		},
		"a dead connection": func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) },
	} {
		t.Run(name, func(t *testing.T) {
			registry, target := newRegistryStub(t, nil)
			registry.set(func(w http.ResponseWriter, r *http.Request) {
				if registry.hits.Load() > 1 {
					second(w, r)
					return
				}
				_, _ = io.WriteString(w, "body")
			})
			fixture := newNPMFixture(t, target)
			fixture.breakDisk(t)
			if status, _, header := fixture.get(t, testTarball); status != http.StatusBadGateway || header.Get("Location") != "" {
				t.Fatalf("status=%d location=%q", status, header.Get("Location"))
			}
		})
	}
}

func TestARealAnswerFromTheRegistryIsHandedOn(t *testing.T) {
	if got := npmStatus(http.StatusNotFound).Error(); !strings.Contains(got, "404") {
		t.Fatalf("error = %q", got)
	}
}

func TestABurstMakesOneUpstreamFetchPerKey(t *testing.T) {
	const burst = 16
	release := make(chan struct{})
	registry, target := newRegistryStub(t, func(w http.ResponseWriter, _ *http.Request) {
		<-release
		_, _ = io.WriteString(w, "body")
	})
	fixture := newNPMFixture(t, target)
	var wait sync.WaitGroup
	wait.Add(burst)
	bodies := make([]string, burst)
	for index := range burst {
		go func() {
			defer wait.Done()
			_, bodies[index], _ = fixture.get(t, testTarball)
		}()
	}
	for fixture.cache.inFlight() != 1 || len(fixture.cache.slots) != burst {
		time.Sleep(time.Millisecond)
	}
	close(release)
	wait.Wait()
	if got := registry.hits.Load(); got != 1 {
		t.Fatalf("%d upstream fetches for one key", got)
	}
	for index, body := range bodies {
		if body != "body" {
			t.Fatalf("request %d got %q", index, body)
		}
	}
}

// TestDifferentRepresentationsAreDifferentKeysButNotAFlood: an abbreviated and a full document
// are not interchangeable, while the spellings of a header must not multiply the cache.
func TestDifferentRepresentationsAreDifferentKeysButNotAFlood(t *testing.T) {
	registry, target := newRegistryStub(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, r.Header.Get("Accept")) })
	fixture := newNPMFixture(t, target)
	_, abbreviated, _ := fixture.get(t, testMetadata, "Accept", "application/vnd.npm.install-v1+json; q=1.0, application/json; q=0.8, */*")
	_, full, _ := fixture.get(t, testMetadata, "Accept", "application/json")
	if !strings.Contains(abbreviated, "install-v1") || strings.Contains(full, "install-v1") {
		t.Fatalf("abbreviated=%q full=%q", abbreviated, full)
	}
	hits := registry.hits.Load()
	fixture.get(t, testMetadata, "Accept", "application/vnd.npm.install-v1+json")
	fixture.get(t, testMetadata, "Accept", "*/*, flood-1")
	if got := registry.hits.Load(); got != hits {
		t.Fatalf("a respelled header reached the registry: %d -> %d", hits, got)
	}
	// The test's own transport asks for gzip unless told otherwise, so that is already stored.
	if got := registry.lastHeader(t, "Accept-Encoding"); got != "gzip" {
		t.Fatalf("asked the registry for %q", got)
	}
	fixture.get(t, testMetadata, "Accept-Encoding", "gzip, deflate")
	fixture.get(t, testMetadata, "Accept-Encoding", "deflate, gzip;q=0.5")
	if got := registry.hits.Load(); got != hits {
		t.Fatalf("respelled encodings: %d -> %d", hits, got)
	}
	fixture.get(t, testMetadata, "Accept-Encoding", "identity")
	if got := registry.hits.Load(); got != hits+1 || registry.lastHeader(t, "Accept-Encoding") != "" {
		t.Fatalf("identity: %d -> %d, asked %q", hits, got, registry.lastHeader(t, "Accept-Encoding"))
	}
}

func TestAScopesSpellingsShareOneObject(t *testing.T) {
	registry, target := newRegistryStub(t, answer("body"))
	fixture := newNPMFixture(t, target)
	for _, path := range []string{"/npm/@types/node", "/npm/@types%2fnode", "/npm/@types%2Fnode"} {
		if status, body, _ := fixture.get(t, path); status != http.StatusOK || body != "body" {
			t.Fatalf("%s: status=%d body=%q", path, status, body)
		}
	}
	if got := registry.hits.Load(); got != 1 {
		t.Fatalf("%d fetches", got)
	}
}

func TestACutDownloadLeavesNoFileAtAll(t *testing.T) {
	registry, target := newRegistryStub(t, cutBody)
	fixture := newNPMFixture(t, target)
	if status, _, _ := fixture.get(t, testTarball); status != http.StatusBadGateway {
		t.Fatalf("status=%d", status)
	}
	if got := fixture.files(t); len(got) != 0 {
		t.Fatalf("a cut download left %q", got)
	}
	registry.set(answer("whole"))
	if status, body, _ := fixture.get(t, testTarball); status != http.StatusOK || body != "whole" {
		t.Fatalf("status=%d body=%q", status, body)
	}
}

// TestAStalledDownloadIsCutAndReleasesItsWaiters: a registry that stops sending must not hold
// a key forever.
func TestAStalledDownloadIsCutAndReleasesItsWaiters(t *testing.T) {
	restore := bodyStallTimeout
	bodyStallTimeout = 100 * time.Millisecond
	t.Cleanup(func() { bodyStallTimeout = restore })
	release := make(chan struct{})
	_, target := newRegistryStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = io.WriteString(w, "partial")
		w.(http.Flusher).Flush()
		<-release
	})
	t.Cleanup(func() { close(release) })
	fixture := newNPMFixture(t, target)
	started := time.Now()
	if status, _, _ := fixture.get(t, testTarball); status != http.StatusBadGateway {
		t.Fatalf("status=%d", status)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("held for %v", elapsed)
	}
	if got := fixture.cache.inFlight(); got != 0 {
		t.Fatalf("%d flights left", got)
	}
}

func TestAnObjectPastTheBoundIsPassedThroughAndNotStored(t *testing.T) {
	restore := npmMaxObject
	npmMaxObject = 8
	t.Cleanup(func() { npmMaxObject = restore })
	chunked := func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "0123456789")
		w.(http.Flusher).Flush()
	}
	for name, respond := range map[string]func(http.ResponseWriter, *http.Request){"declared": answer("0123456789"), "undeclared": chunked} {
		t.Run(name, func(t *testing.T) {
			_, target := newRegistryStub(t, respond)
			fixture := newNPMFixture(t, target)
			if status, body, _ := fixture.get(t, testTarball); status != http.StatusOK || body != "0123456789" {
				t.Fatalf("status=%d body=%q", status, body)
			}
			if got := fixture.files(t); len(got) != 0 {
				t.Fatalf("stored %q", got)
			}
		})
	}
}

func TestAPathPastTheBoundIsRefused(t *testing.T) {
	path := "/npm/" + strings.Repeat("a", npmMaxPath)
	if verdict := DecideNPM(http.MethodGet, path, lanGate, http.Header{}); verdict.Allow || verdict.Status != http.StatusNotFound {
		t.Fatalf("%+v", verdict)
	}
}

func TestACorruptFileIsAMissAndIsReplaced(t *testing.T) {
	for name, content := range map[string]string{"not json": "garbage\nbody", "no header end": strings.Repeat("x", npmHeaderLimit+1), "empty": ""} {
		t.Run(name, func(t *testing.T) {
			registry, target := newRegistryStub(t, answer("right"))
			fixture := newNPMFixture(t, target)
			fixture.get(t, testTarball)
			files := fixture.files(t)
			if len(files) != 1 {
				t.Fatalf("files=%q", files)
			}
			if err := os.WriteFile(filepath.Join(fixture.dir, files[0]), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if status, body, _ := fixture.get(t, testTarball); status != http.StatusOK || body != "right" {
				t.Fatalf("status=%d body=%q", status, body)
			}
			if got := registry.hits.Load(); got != 2 {
				t.Fatalf("hits=%d", got)
			}
		})
	}
}

// TestAnObjectWhoseHeadersDoNotFitIsNotStored keeps the header a small, bounded read.
func TestAnObjectWhoseHeadersDoNotFitIsNotStored(t *testing.T) {
	_, target := newRegistryStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", strings.Repeat("e", npmHeaderLimit))
		_, _ = io.WriteString(w, "body")
	})
	fixture := newNPMFixture(t, target)
	if status, body, _ := fixture.get(t, testMetadata); status != http.StatusOK || body != "body" {
		t.Fatalf("status=%d body=%q", status, body)
	}
	if got := fixture.files(t); len(got) != 0 {
		t.Fatalf("stored %q", got)
	}
}

// TestAFailedRenameLeavesNoTempAndPassesThrough: a directory sits at the object's name, so the
// last step of a store fails after every byte was written.
func TestAFailedRenameLeavesNoTempAndPassesThrough(t *testing.T) {
	_, target := newRegistryStub(t, answer("body"))
	fixture := newNPMFixture(t, target)
	fixture.get(t, testTarball)
	files := fixture.files(t)
	if len(files) != 1 {
		t.Fatalf("files=%q", files)
	}
	file := filepath.Join(fixture.dir, files[0])
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(file, "occupied"), 0o700); err != nil {
		t.Fatal(err)
	}
	if status, body, _ := fixture.get(t, testTarball); status != http.StatusOK || body != "body" {
		t.Fatalf("status=%d body=%q", status, body)
	}
	if got := fixture.files(t); len(got) != 1 || got[0] != files[0] {
		t.Fatalf("a failed rename left %q", got)
	}
}

// TestAnObjectEvictedAsItIsStoredPassesThrough: a 304 for a file the prune just removed.
func TestAnObjectEvictedAsItIsStoredPassesThrough(t *testing.T) {
	registry, target := newRegistryStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `"one"`)
		_, _ = io.WriteString(w, "body")
	})
	fixture := newNPMFixture(t, target)
	fixture.get(t, testMetadata)
	fixture.age(t, npmMetadataTTL+time.Minute)
	files := fixture.files(t)
	registry.set(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" {
			if err := os.Remove(filepath.Join(fixture.dir, files[0])); err != nil {
				t.Error(err)
			}
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = io.WriteString(w, "whole")
	})
	if status, body, _ := fixture.get(t, testMetadata); status != http.StatusOK || body != "whole" {
		t.Fatalf("status=%d body=%q", status, body)
	}
}

func TestTheRouteAnswersItsOwnPingWithoutTheRegistry(t *testing.T) {
	registry, target := newRegistryStub(t, answer("body"))
	fixture := newNPMFixture(t, target)
	if status, _, _ := fixture.get(t, NPMPingPath); status != http.StatusOK || registry.hits.Load() != 0 {
		t.Fatalf("status=%d hits=%d", status, registry.hits.Load())
	}
}

func TestAHeadRequestGetsNoBody(t *testing.T) {
	_, target := newRegistryStub(t, answer("body"))
	fixture := newNPMFixture(t, target)
	response, err := http.Head(fixture.gate.URL + testTarball)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || len(body) != 0 || response.ContentLength != 4 {
		t.Fatalf("status=%d body=%q length=%d", response.StatusCode, body, response.ContentLength)
	}
}

func TestAWaiterWhoLeavesLeavesAlone(t *testing.T) {
	release := make(chan struct{})
	registry, target := newRegistryStub(t, func(w http.ResponseWriter, _ *http.Request) {
		<-release
		_, _ = io.WriteString(w, "body")
	})
	fixture := newNPMFixture(t, target)
	done := make(chan string, 1)
	go func() {
		_, body, _ := fixture.get(t, testTarball)
		done <- body
	}()
	for fixture.cache.inFlight() != 1 {
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, fixture.gate.URL+testTarball, nil)
	if response, err := http.DefaultClient.Do(request); err == nil {
		_ = response.Body.Close()
		t.Fatal("the waiter was answered while the fetch was still running")
	}
	close(release)
	if body := <-done; body != "body" || registry.hits.Load() != 1 {
		t.Fatalf("body=%q hits=%d", body, registry.hits.Load())
	}
}

// TestALeaderWhoLeavesStillFinishesTheDownload: the next job wants those bytes too.
func TestALeaderWhoLeavesStillFinishesTheDownload(t *testing.T) {
	release := make(chan struct{})
	registry, target := newRegistryStub(t, func(w http.ResponseWriter, _ *http.Request) {
		<-release
		_, _ = io.WriteString(w, "body")
	})
	fixture := newNPMFixture(t, target)
	ctx, cancel := context.WithCancel(context.Background())
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, fixture.gate.URL+testTarball, nil)
	done := make(chan error, 1)
	go func() {
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			_ = response.Body.Close()
		}
		done <- err
	}()
	for fixture.cache.inFlight() != 1 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	close(release)
	for fixture.cache.inFlight() != 0 {
		time.Sleep(time.Millisecond)
	}
	if status, body, _ := fixture.get(t, testTarball); status != http.StatusOK || body != "body" || registry.hits.Load() != 1 {
		t.Fatalf("status=%d body=%q hits=%d", status, body, registry.hits.Load())
	}
}

type panickingTransport struct{ value any }

func (p panickingTransport) RoundTrip(*http.Request) (*http.Response, error) { panic(p.value) }

// TestAPanicInThisPathDoesNotReachTheGate covers the fetch, waiters queued behind it, and the
// routes beside it.
func TestAPanicInThisPathDoesNotReachTheGate(t *testing.T) {
	_, target := newRegistryStub(t, answer("body"))
	fixture := newNPMFixture(t, target)
	fixture.cache.client.Transport = panickingTransport{"boom"}
	var logs strings.Builder
	fixture.cache.logger = slogTo(&logs)
	if status, _, _ := fixture.get(t, testTarball); status != http.StatusBadGateway {
		t.Fatalf("status=%d", status)
	}
	if !strings.Contains(logs.String(), "npm cache request panicked") {
		t.Fatalf("logs=%q", logs.String())
	}
	if got := fixture.cache.inFlight(); got != 0 {
		t.Fatalf("a panic left %d flights", got)
	}
	if got := len(fixture.cache.slots); got != 0 {
		t.Fatalf("a panic leaked %d slots", got)
	}
	if status, _, _ := fixture.get(t, "/download/x"); status != http.StatusOK {
		t.Fatalf("Actions cache after an npm panic: status=%d", status)
	}
	fixture.cache.client.Transport = newNPMTransport()
	if status, body, _ := fixture.get(t, testTarball); status != http.StatusOK || body != "body" {
		t.Fatalf("recovery: status=%d body=%q", status, body)
	}
}

func TestAnAbortStaysAnAbort(t *testing.T) {
	_, target := newRegistryStub(t, answer("body"))
	fixture := newNPMFixture(t, target)
	fixture.cache.client.Transport = panickingTransport{http.ErrAbortHandler}
	request, _ := http.NewRequest(http.MethodGet, fixture.gate.URL+testTarball, nil)
	response, err := http.DefaultTransport.RoundTrip(request)
	if err == nil {
		_ = response.Body.Close()
		t.Fatal("an aborted handler answered")
	}
	if status, _, _ := fixture.get(t, "/download/x"); status != http.StatusOK {
		t.Fatalf("status=%d", status)
	}
}

// TestNpmFailuresDoNotReachTheActionsRoutes is the blast radius: however this path fails, the
// routes every job's cache and artifacts depend on keep answering.
func TestNpmFailuresDoNotReachTheActionsRoutes(t *testing.T) {
	failures := map[string]func(*testing.T, *npmFixture, *registryStub){
		"the registry hangs": func(t *testing.T, f *npmFixture, r *registryStub) {
			t.Helper()
			release := make(chan struct{})
			t.Cleanup(func() { close(release) })
			r.set(func(http.ResponseWriter, *http.Request) { <-release })
			for index := range 8 {
				go func() {
					response, err := http.Get(fmt.Sprintf("%s/npm/pkg%d/-/pkg-1.0.0.tgz", f.gate.URL, index))
					if err == nil {
						_ = response.Body.Close()
					}
				}()
			}
			for f.cache.inFlight() != 8 {
				time.Sleep(time.Millisecond)
			}
		},
		"every npm slot is taken": func(t *testing.T, f *npmFixture, _ *registryStub) {
			t.Helper()
			for len(f.cache.slots) < cap(f.cache.slots) {
				f.cache.slots <- struct{}{}
			}
			if status, _, header := f.get(t, testTarball); status != http.StatusServiceUnavailable || header.Get("Retry-After") == "" {
				t.Fatalf("a full route: status=%d", status)
			}
		},
		"the disk cache is unusable": func(t *testing.T, f *npmFixture, _ *registryStub) {
			t.Helper()
			f.breakDisk(t)
		},
		"the registry is gone": func(t *testing.T, _ *npmFixture, r *registryStub) {
			t.Helper()
			r.Close()
		},
		"the fetch panics": func(t *testing.T, f *npmFixture, _ *registryStub) {
			t.Helper()
			f.cache.client.Transport = panickingTransport{"boom"}
			f.cache.logger = slogTo(io.Discard)
			f.get(t, testTarball)
		},
	}
	for name, fail := range failures {
		t.Run(name, func(t *testing.T) {
			registry, target := newRegistryStub(t, answer("body"))
			fixture := newNPMFixture(t, target)
			fail(t, fixture, registry)
			for _, path := range []string{"/download/x", "/twirp/results.services.receiver.Receiver/X", "/health"} {
				client := http.Client{Timeout: 2 * time.Second}
				response, err := client.Get(fixture.gate.URL + path)
				if err != nil {
					t.Fatalf("%s beside a failing npm route: %v", path, err)
				}
				_ = response.Body.Close()
				if response.StatusCode != http.StatusOK {
					t.Fatalf("%s: status=%d", path, response.StatusCode)
				}
			}
		})
	}
}

func TestCountersSeparateWhatGuestsReadFromWhatCameFromTheRegistry(t *testing.T) {
	_, target := newRegistryStub(t, answer("0123456789"))
	fixture := newNPMFixture(t, target)
	for range 3 {
		fixture.get(t, testTarball)
	}
	metrics := fixture.counters.render()
	for _, want := range []string{
		`fleet_cache_gate_bytes_total{upstream="npm",direction="to_guest"} 30`,
		`fleet_cache_gate_bytes_total{upstream="npm",direction="from_registry"} 10`,
	} {
		if !strings.Contains(metrics, want) {
			t.Fatalf("metrics missing %q:\n%s", want, metrics)
		}
	}
	fixture.breakDisk(t)
	fixture.get(t, testTarball)
	if !strings.Contains(fixture.counters.render(), `direction="from_registry"} 20`) {
		t.Fatalf("a pass-through was not counted:\n%s", fixture.counters.render())
	}
}
