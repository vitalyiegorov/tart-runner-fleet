package cachegate

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
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

type artifactPausedBody struct {
	entered, release chan struct{}
	once             sync.Once
}

func (b *artifactPausedBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return 0, io.EOF
}
func TestArtifactStalledBodyDoesNotLockStore(t *testing.T) {
	for _, kind := range []string{"block", "blocklist", "rpc"} {
		t.Run(kind, func(t *testing.T) {
			f := newArtifactFixture(t)
			auth := f.token("123", "r", "j")
			raw := f.create(auth, "r", "j", "stalled")
			body := &artifactPausedBody{entered: make(chan struct{}), release: make(chan struct{})}
			done := make(chan struct{})
			path := raw + "&comp=" + kind + "&blockid=YQ=="
			method := http.MethodPut
			if kind == "rpc" {
				path = artifactService + "ListArtifacts"
				method = http.MethodPost
			}
			req := httptest.NewRequest(method, path, body)
			req.Header.Set("Authorization", "Bearer "+auth)
			go func() { defer close(done); f.handler.ServeHTTP(httptest.NewRecorder(), req) }()
			defer func() { close(body.release); <-done }()
			<-body.entered
			if !f.store.mu.TryLock() {
				t.Fatal("network body holds global store lock")
			}
			f.store.mu.Unlock()
			f.budget.mu.Lock()
			reserved := f.budget.reserved
			f.budget.mu.Unlock()
			if reserved != 0 {
				t.Fatal("network body holds write reservation")
			}
			if f.rpc(auth, "ListArtifacts", artifactBody("r", "j", "")).Code != http.StatusOK {
				t.Fatal("list blocked by uploader")
			}
			f.upload(f.token("123", "other", "job"), "other", "job", "other-run", []byte("unrelated"))
		})
	}
}

func TestArtifactFinalizeReplayUsesStoredDigest(t *testing.T) {
	f := newArtifactFixture(t)
	auth := f.token("123", "r", "j")
	f.upload(auth, "r", "j", "a", []byte("bytes"))
	artifactInjectFailure(t, f, "SELECT data", false)
	body := artifactBody("r", "j", "a")
	body["size"] = "5"
	if f.rpc(auth, "FinalizeArtifact", body).Code != http.StatusOK {
		t.Fatal("finalize replay re-read blob blocks")
	}
	body["hash"] = "sha256:wrong"
	if f.rpc(auth, "FinalizeArtifact", body).Code != http.StatusBadRequest {
		t.Fatal("replay ignored hash mismatch")
	}
}

type artifactZeroReader struct{}

func (artifactZeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }
func TestArtifactCacheAccepts100MiBStreamingPUT(t *testing.T) {
	const size = 100 << 20
	budget := &writeBudget{floor: 1, probe: func() (int64, error) { return 1 << 40, nil }}
	var received int64
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		received, err = io.Copy(io.Discard, r.Body)
		if err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusCreated)
	})
	handler := withArtifacts(next, next, budget, 1<<20, &Counters{})
	req := httptest.NewRequest(http.MethodPut, "/upload/single", io.LimitReader(artifactZeroReader{}, size))
	req.ContentLength = size
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusCreated || received != size {
		t.Fatal("100 MiB cache PUT was rejected or truncated")
	}
}

func TestArtifactStalledCacheReleasesReservation(t *testing.T) {
	old := bodyStallTimeout
	bodyStallTimeout = 30 * time.Millisecond
	defer func() { bodyStallTimeout = old }()
	budget := &writeBudget{floor: 1, probe: func() (int64, error) { return 1 << 40, nil }}
	done := make(chan struct{})
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusBadGateway)
		close(done)
	})
	gate := httptest.NewServer(newServer(withArtifacts(next, next, budget, 1<<20, &Counters{})).Handler)
	defer gate.Close()
	parsed, _ := url.Parse(gate.URL)
	conn, err := net.Dial("tcp", parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	io.WriteString(conn, "PUT /upload/stall HTTP/1.1\r\nHost: gate\r\nContent-Length: 1024\r\n\r\nx")
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		conn.Close()
		t.Fatal("stalled cache writer never released its reservation")
	}
	// Wait for middleware return, not just the backend reader's last instruction.
	deadline := time.Now().Add(time.Second)
	for {
		budget.mu.Lock()
		remaining := budget.reserved
		budget.mu.Unlock()
		if remaining == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cache reservation leaked")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestArtifactCachePeakMustBeExplicit(t *testing.T) {
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	c := artifactFlags(flags)
	if c.cachePeak != 0 {
		t.Fatal("cache peak defaults to 60 GiB rather than the configured cache cap")
	}
}

func TestArtifactSQLiteFullEvictsAndRetries(t *testing.T) {
	f := newArtifactFixture(t)
	auth := f.token("123", "r", "j")
	data := bytes.Repeat([]byte("a"), 1<<20)
	f.upload(auth, "r", "j", "old", data)
	raw := f.create(auth, "r", "j", "new")
	var pages int64
	if err := f.store.db.QueryRow("PRAGMA page_count").Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.Exec("PRAGMA max_page_count=" + fmt.Sprint(pages)); err != nil {
		t.Fatal(err)
	}
	if f.blob(raw+"&comp=block&blockid=YQ==", http.MethodPut, data).Code != http.StatusCreated {
		t.Fatal("SQLITE_FULL returned 503 without LRU retry")
	}
	if _, err := f.store.named(artifactPrincipal{repo: "123", run: "r"}, "old"); !errors.Is(err, errArtifactMissing) {
		t.Fatal("full page store did not evict old entry")
	}
}
func TestArtifactPageHeadroomScalesWithCapacity(t *testing.T) {
	f := newArtifactFixture(t)
	const capBytes = 20 << 30
	s, err := openArtifactStore(filepath.Join(t.TempDir(), "owner", "index"), capBytes, func() time.Time { return f.at }, f.budget)
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()
	var pages int64
	s.db.QueryRow("PRAGMA max_page_count").Scan(&pages)
	if pages*4096 < capBytes+capBytes/1000 {
		t.Fatal("hard page cap does not cover SQLite overflow overhead")
	}
}

func TestArtifactWriteReservationDoesNotScaleWithDatabase(t *testing.T) {
	f := newArtifactFixture(t)
	path := filepath.Join(t.TempDir(), "large")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	file.Truncate(1 << 30)
	file.Close()
	f.store.path = path
	f.budget.floor = 1
	f.budget.probe = func() (int64, error) { return 100 << 20, nil }
	release, err := f.store.reserveWrite()
	if err != nil {
		t.Fatal("large existing database starved bounded block write")
	}
	release()
}
func TestArtifactDeleteAndPruneIgnoreWriteFloor(t *testing.T) {
	f := newArtifactFixture(t)
	auth := f.token("123", "r", "j")
	f.upload(auth, "r", "j", "a", []byte("bytes"))
	f.create(auth, "r", "j", "partial")
	f.budget.probe = func() (int64, error) { return 0, nil }
	if f.rpc(auth, "DeleteArtifact", artifactBody("r", "j", "a")).Code != http.StatusOK {
		t.Fatal("disk floor blocked reclamation")
	}
	f.at = f.at.Add(2 * time.Hour)
	f.store.prune()
	if _, err := f.store.named(artifactPrincipal{repo: "123", run: "r"}, "partial"); !errors.Is(err, errArtifactMissing) {
		t.Fatal("disk floor blocked staging prune")
	}
}

func TestArtifactRollbackIgnoresLeftoverFlags(t *testing.T) {
	c := artifactConfig{repos: "123", budgetPath: "left-over", cap: -1, floor: -1, cachePeak: -1}
	if !c.valid() {
		t.Fatal("removing artifact mode leaves gate unable to start")
	}
	cache, _ := upstream(t)
	listen := freeAddress(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var logs bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- Execute(ctx, []string{"--listen", listen, "--upstream", cache.String(), "--artifacts-repositories", "123", "--write-budget-path", "left-over"}, io.Discard, &logs)
	}()
	waitFor(t, "http://"+listen+"/health")
	cancel()
	if code := <-done; code != 0 {
		t.Fatal("rollback start failed")
	}
	if !strings.Contains(logs.String(), "ignored") {
		t.Fatal("missing inactive configuration warning")
	}
}
func TestArtifactMetricsOnlyRefreshOnPrune(t *testing.T) {
	f := newArtifactFixture(t)
	f.store.metrics = &artifactMetrics{}
	f.store.metrics.used.Store(77)
	auth := f.token("123", "r", "j")
	f.upload(auth, "r", "j", "a", []byte("bytes"))
	f.rpc(auth, "ListArtifacts", artifactBody("r", "j", ""))
	if f.store.metrics.used.Load() != 77 {
		t.Fatal("request scanned storage to update gauges")
	}
	f.store.prune()
	if f.store.metrics.used.Load() != 5 {
		t.Fatal("minute sweep did not refresh gauges")
	}
}
func TestArtifactDecisionHasSingleDocumentAndStore(t *testing.T) {
	for _, path := range []string{"../../docs/ARTIFACT_GATE_PLAN_509.md", "../../docs/ARTIFACT_GATE_RESEARCH_509.md", "artifact_metrics.go", "artifact_maintenance.go"} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("superseded artifact file remains: %s", path)
		}
	}
}

type artifactPausedResponse struct {
	header           http.Header
	entered, release chan struct{}
	once             sync.Once
}

func (w *artifactPausedResponse) Header() http.Header { return w.header }
func (*artifactPausedResponse) WriteHeader(int)       {}
func (w *artifactPausedResponse) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return len(p), nil
}
func TestArtifactSlowRPCResponseDoesNotLockStore(t *testing.T) {
	f := newArtifactFixture(t)
	auth := f.token("123", "r", "j")
	body, _ := json.Marshal(artifactBody("r", "j", ""))
	r := httptest.NewRequest(http.MethodPost, artifactService+"ListArtifacts", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+auth)
	w := &artifactPausedResponse{header: http.Header{}, entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan struct{})
	go func() { defer close(done); f.handler.ServeHTTP(w, r) }()
	defer func() { close(w.release); <-done }()
	<-w.entered
	if !f.store.mu.TryLock() {
		t.Fatal("slow RPC reader holds store lock")
	}
	f.store.mu.Unlock()
	f.upload(f.token("123", "other", "j"), "other", "j", "other", []byte("other"))
}
func TestArtifactOnlyListenerRejectsCacheRoutes(t *testing.T) {
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	artifactFlags(flags)
	if err := flags.Parse([]string{"--artifacts-listen", "127.0.0.1:0"}); err != nil {
		t.Fatal("owner has no artifact-only listener")
	}
	f := newArtifactFixture(t)
	cache, _ := upstream(t)
	mainAddr, onlyAddr := freeAddress(t), freeAddress(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- Execute(ctx, []string{"--listen", mainAddr, "--upstream", cache.String(), "--artifacts-dir", filepath.Join(t.TempDir(), "owner"), "--artifacts-repositories", "123", "--artifacts-listen", onlyAddr, "--write-budget-path", filepath.Dir(f.store.path), "--write-min-free-bytes", "1", "--cache-write-peak-bytes", "1048576"}, io.Discard, io.Discard)
	}()
	defer func() { cancel(); <-done }()
	waitFor(t, "http://"+mainAddr+"/health")
	for _, path := range []string{cacheService + "CreateCacheEntry", "/upload/cache", "/download/cache", "/npm/package", "/health", "/.trf/gate-identity"} {
		res, err := http.Get("http://" + onlyAddr + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			t.Fatal("artifact tunnel exposed a non-artifact route")
		}
	}
	res, err := http.Post("http://"+onlyAddr+artifactService+"CreateArtifact", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatal("artifact-only listener missing owner auth")
	}
}
func TestArtifactOversizedBlockDoesNotEvict(t *testing.T) {
	f := newArtifactFixture(t)
	auth := f.token("123", "r", "j")
	f.upload(auth, "r", "j", "keep", []byte("keep"))
	f.create(auth, "r", "j", "new")
	a, err := f.store.named(artifactPrincipal{repo: "123", run: "r"}, "new")
	if err != nil {
		t.Fatal(err)
	}
	f.store.cap = 4
	if err = f.store.putBlock(a, "YQ==", []byte("large")); !errors.Is(err, errArtifactSpace) {
		t.Fatal("oversized block not rejected")
	}
	if _, err = f.store.named(artifactPrincipal{repo: "123", run: "r"}, "keep"); err != nil {
		t.Fatal("oversized block evicted unrelated artifact")
	}
}

func TestArtifactKeyRotationGetsOneImmediateRefresh(t *testing.T) {
	f := newArtifactFixture(t)
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		keys := []any{map[string]string{"kty": "RSA", "kid": "test", "n": base64.RawURLEncoding.EncodeToString(f.key.N.Bytes()), "e": "AQAB"}}
		if n > 1 {
			keys = append(keys, map[string]string{"kty": "RSA", "kid": "rotated", "n": base64.RawURLEncoding.EncodeToString(f.key.N.Bytes()), "e": "AQAB"})
		}
		json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	}))
	defer server.Close()
	v := &artifactVerifier{url: server.URL, client: server.Client(), clock: func() time.Time { return f.at }}
	if _, err := v.key(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := v.key(context.Background(), "rotated"); err != nil {
		t.Fatal("new signing kid blocked by refresh cooldown")
	}
	for _, kid := range []string{"flood-1", "flood-2"} {
		if _, err := v.key(context.Background(), kid); err == nil {
			t.Fatal("unknown key authorized")
		}
	}
	if hits.Load() != 2 {
		t.Fatal("unseen kid flood bypassed refresh bound")
	}
	f.at = f.at.Add(31 * time.Second)
	v.key(context.Background(), "flood-3")
	if hits.Load() != 3 {
		t.Fatal("normal refresh did not resume")
	}
}

func TestArtifactFinalizationYieldsStoreLock(t *testing.T) {
	f := newArtifactFixture(t)
	auth := f.token("123", "r", "j")
	f.upload(auth, "r", "j", "a", []byte("bytes"))
	f.store.db.Exec("UPDATE artifacts SET final=0")
	f.store.db.Close()
	pause := &artifactPausedBody{entered: make(chan struct{}), release: make(chan struct{})}
	name := fmt.Sprintf("artifact-hash-pause-%d", artifactFaultSerial.Add(1))
	sql.Register(name, &artifactFaultDriver{match: "\x00", pause: pause})
	db, err := sql.Open(name, f.store.path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	f.store.db = db
	done := make(chan struct{})
	status := 0
	go func() {
		defer close(done)
		body := artifactBody("r", "j", "a")
		body["size"] = "5"
		status = f.rpc(auth, "FinalizeArtifact", body).Code
	}()
	defer func() {
		close(pause.release)
		<-done
		if status != http.StatusNotFound {
			t.Error("finalize ignored expiry during hashing")
		}
	}()
	<-pause.entered
	if !f.store.mu.TryLock() {
		t.Fatal("finalization hashes under global mutex")
	}
	leased := len(f.store.leases)
	f.store.mu.Unlock()
	if leased != 1 {
		t.Fatal("hash has no eviction lease")
	}
	f.at = f.at.Add(73 * time.Hour)
}
func TestArtifactUploadRechecksStateAfterBody(t *testing.T) {
	f := newArtifactFixture(t)
	auth := f.token("123", "r", "j")
	raw := f.create(auth, "r", "j", "a")
	body := &artifactPausedBody{entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan int, 1)
	go func() {
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, httptest.NewRequest(http.MethodPut, raw+"&comp=block&blockid=YQ==", body))
		done <- w.Code
	}()
	<-body.entered
	if !f.store.mu.TryLock() {
		close(body.release)
		<-done
		t.Fatal("body holds store lock")
	}
	f.store.mu.Unlock()
	if f.rpc(auth, "DeleteArtifact", artifactBody("r", "j", "a")).Code != http.StatusOK {
		close(body.release)
		<-done
		t.Fatal("delete while receiving failed")
	}
	close(body.release)
	if code := <-done; code != http.StatusNotFound {
		t.Fatal("upload did not recheck deleted record")
	}
}

func TestArtifactOnlyListenerRejectsNonLoopbackBind(t *testing.T) {
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	c := artifactFlags(flags)
	c.dir = t.TempDir()
	c.repos = "123"
	c.budgetPath = c.dir
	c.cachePeak = 1
	for _, addr := range []string{"", "0.0.0.0:3021", "gate.example:3021"} {
		c.listen = addr
		if c.valid() {
			t.Fatalf("artifact-only listener accepts non-loopback bind %q", addr)
		}
	}
}

func TestArtifactCouncilFailureBoundaries(t *testing.T) {
	f := newArtifactFixture(t)
	auth := f.token("123", "r", "j")
	raw := f.upload(auth, "r", "j", "a", []byte("data"))
	if f.blob(raw+"&comp=block&blockid=YQ==", http.MethodPut, []byte("replacement")).Code != http.StatusForbidden {
		t.Fatal("finalized blob mutated")
	}
	a, _ := f.store.named(artifactPrincipal{repo: "123", run: "r"}, "a")
	readURL := f.store.signedURL(f.origin, a, "read")
	f.store.db.Exec("UPDATE artifacts SET final=0")
	if f.blob(readURL, http.MethodGet, nil).Code != http.StatusNotFound {
		t.Fatal("unfinalized object readable")
	}
	body := artifactBody("r", "j", "a")
	body["size"] = "5"
	if f.rpc(auth, "FinalizeArtifact", body).Code != http.StatusBadRequest {
		t.Fatal("fresh finalize accepted wrong size")
	}
	body["size"] = "4"
	body["hash"] = "wrong"
	if f.rpc(auth, "FinalizeArtifact", body).Code != http.StatusBadRequest {
		t.Fatal("fresh finalize accepted wrong digest")
	}
	body["hash"] = ""
	f.budget.probe = func() (int64, error) { return 0, nil }
	if f.rpc(auth, "FinalizeArtifact", body).Code != http.StatusServiceUnavailable {
		t.Fatal("fresh finalize bypassed write reserve")
	}
	f.budget.probe = func() (int64, error) { return 1 << 40, nil }
	f.rpc(auth, "DeleteArtifact", artifactBody("r", "j", "a"))
	if f.blob(readURL, http.MethodGet, nil).Code != http.StatusNotFound {
		t.Fatal("deleted read capability survived")
	}
	f.store.db.Close()
	if f.blob(readURL, http.MethodGet, nil).Code != http.StatusServiceUnavailable {
		t.Fatal("unavailable storage became a missing artifact")
	}
	v := &artifactVerifier{clock: func() time.Time { return f.at }}
	if _, err := v.key(context.Background(), ""); !errors.Is(err, errArtifactAuth) {
		t.Fatal("empty signing kid allowed")
	}
}
func TestArtifactCacheLengthReservationBoundaries(t *testing.T) {
	budget := &writeBudget{floor: 1, probe: func() (int64, error) { return 1 << 40, nil }}
	for _, test := range []struct {
		length, expected int64
		path             string
	}{{5, 3*5 + 4096, "/upload/a"}, {-1, 3*(128<<20) + 4096, "/upload/a"}, {5, 3*5 + 4096 + 1234, cacheService + "FinalizeCacheEntryUpload"}} {
		called := false
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			if r.ContentLength != test.length {
				t.Error("stream framing changed")
			}
			budget.mu.Lock()
			reserved := budget.reserved
			budget.mu.Unlock()
			if reserved != test.expected {
				t.Error("reservation does not match length/commit cap")
			}
			io.Copy(io.Discard, r.Body)
		})
		req := httptest.NewRequest(http.MethodPut, test.path, strings.NewReader("bytes"))
		req.ContentLength = test.length
		withArtifacts(next, next, budget, 1234, &Counters{}).ServeHTTP(httptest.NewRecorder(), req)
		if !called {
			t.Fatal("stream not forwarded")
		}
	}
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("overflowing reservation reached cache") })
	r := httptest.NewRequest(http.MethodPut, "/upload/a", nil)
	r.ContentLength = math.MaxInt64
	w := httptest.NewRecorder()
	withArtifacts(next, next, budget, 1, &Counters{}).ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatal("overflow length admitted")
	}
}
func TestArtifactSQLiteFullWithoutVictimRemainsExplicit(t *testing.T) {
	f := newArtifactFixture(t)
	f.create(f.token("123", "r", "j"), "r", "j", "only-staging")
	a, _ := f.store.named(artifactPrincipal{repo: "123", run: "r"}, "only-staging")
	var pages int64
	f.store.db.QueryRow("PRAGMA page_count").Scan(&pages)
	f.store.db.Exec("PRAGMA max_page_count=" + fmt.Sprint(pages))
	if err := f.store.putBlock(a, "YQ==", bytes.Repeat([]byte("x"), 1<<20)); !errors.Is(err, errArtifactSpace) {
		t.Fatal("full staging store did not report unavailable capacity")
	}
}
func TestArtifactCapCannotEvictLeasedData(t *testing.T) {
	f := newArtifactFixture(t)
	auth := f.token("123", "r", "j")
	f.upload(auth, "r", "j", "leased", []byte("a"))
	f.create(auth, "r", "j", "new")
	old, _ := f.store.named(artifactPrincipal{repo: "123", run: "r"}, "leased")
	a, _ := f.store.named(artifactPrincipal{repo: "123", run: "r"}, "new")
	f.store.leases[old.ID] = 1
	f.store.cap = 1
	if err := f.store.putBlock(a, "YQ==", []byte("b")); !errors.Is(err, errArtifactSpace) {
		t.Fatal("write crossed cap while all victims were leased")
	}
}
func TestArtifactOnlyListenerFailureClosesMainGate(t *testing.T) {
	f := newArtifactFixture(t)
	cache, _ := upstream(t)
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	mainAddr := freeAddress(t)
	args := []string{"--listen", mainAddr, "--upstream", cache.String(), "--artifacts-dir", filepath.Join(t.TempDir(), "owner"), "--artifacts-repositories", "123", "--artifacts-listen", busy.Addr().String(), "--write-budget-path", filepath.Dir(f.store.path), "--write-min-free-bytes", "1", "--cache-write-peak-bytes", "1234"}
	if code := Execute(context.Background(), args, io.Discard, io.Discard); code != 1 {
		t.Fatal("artifact listener failure ignored")
	}
	if c, err := net.Dial("tcp", mainAddr); err == nil {
		c.Close()
		t.Fatal("failed startup left main gate listening")
	}
	file := filepath.Join(t.TempDir(), "file")
	os.WriteFile(file, nil, 0600)
	args[5] = file // artifacts-dir; storage failure must not start any listener
	if code := Execute(context.Background(), args, io.Discard, io.Discard); code != 1 {
		t.Fatal("storage initialization failure ignored")
	}
	if _, err := openArtifactStore(filepath.Join(file, "db"), 1024, func() time.Time { return f.at }, f.budget); err == nil {
		t.Fatal("database under a file was accepted")
	}
}
