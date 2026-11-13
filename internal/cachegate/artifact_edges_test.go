package cachegate

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

func TestArtifactEnabledRepositoryNamespacesNeverOverlap(t *testing.T) {
	f := newArtifactFixture(t)
	v := &artifactVerifier{url: f.keys.URL, client: f.keys.Client(), clock: func() time.Time { return f.at }}
	f.handler = artifactHandler(f.store, v, f.origin, map[string]bool{"123": true, "456": true})
	a, b := f.token("123", "run", "job"), f.token("456", "run", "job")
	f.upload(a, "run", "job", "same-name", []byte("repository A"))
	for _, method := range []string{"FinalizeArtifact", "GetSignedArtifactURL", "DeleteArtifact"} {
		if f.rpc(b, method, artifactBody("run", "job", "same-name")).Code != http.StatusNotFound {
			t.Fatal("B reached A's namespace")
		}
	}
	if strings.Contains(f.rpc(b, "ListArtifacts", artifactBody("run", "job", "")).Body.String(), "same-name") {
		t.Fatal("B listed A")
	}
	f.upload(b, "run", "job", "same-name", []byte("repository B"))
	aa, err := f.store.named(artifactPrincipal{repo: "123", run: "run"}, "same-name")
	if err != nil {
		t.Fatal(err)
	}
	var data bytes.Buffer
	if _, err = f.store.stream(aa, &data); err != nil || data.String() != "repository A" {
		t.Fatal("B changed A's bytes")
	}
}

func TestArtifactDownloadLeaseAndExplicitFailedStream(t *testing.T) {
	f := newArtifactFixture(t)
	token := f.token("123", "r", "j")
	f.upload(token, "r", "j", "leased", []byte("bytes"))
	a, err := f.store.named(artifactPrincipal{repo: "123", run: "r"}, "leased")
	if err != nil {
		t.Fatal(err)
	}
	f.store.leases[a.ID] = 1
	f.store.cap = 1
	if err = f.store.makeRoom(1, 0); !errors.Is(err, errArtifactSpace) {
		t.Fatal("leased object evicted")
	}
	if f.rpc(token, "DeleteArtifact", artifactBody("r", "j", "leased")).Code != http.StatusServiceUnavailable {
		t.Fatal("delete broke active read")
	}
	f.at = f.at.Add(73 * time.Hour)
	f.store.prune()
	if _, err = f.store.record(a.ID); err != nil {
		t.Fatal("TTL broke active read")
	}
	delete(f.store.leases, a.ID)
	f.store.prune()
	if _, err = f.store.record(a.ID); !errors.Is(err, errArtifactMissing) {
		t.Fatal("expired lease retained after release")
	}
}

func TestArtifactOwnerMetricsAndUnavailableBudget(t *testing.T) {
	f := newArtifactFixture(t)
	f.store.metrics = &artifactMetrics{}
	token := f.token("123", "r", "j")
	f.upload(token, "r", "j", "metrics", []byte("123"))
	f.rpc("forged", "CreateArtifact", artifactBody("r", "j", "bad"))
	f.store.prune()
	counters := &Counters{artifactsEnabled: true, artifactStats: f.store.metrics}
	for _, want := range []string{"fleet_artifacts_used_bytes 3", "fleet_artifacts_staged_bytes 0", "fleet_artifacts_objects 1", "fleet_artifacts_auth_refusals_total 1"} {
		if !strings.Contains(counters.render(), want) {
			t.Fatalf("missing %s", want)
		}
	}
	f.store.cap = 1
	f.store.makeRoom(1, 0)
	f.store.observe()
	if f.store.metrics.lru.Load() != 1 {
		t.Fatal("LRU not counted")
	}
	f.create(token, "r", "j", "partial")
	f.at = f.at.Add(time.Hour)
	f.store.prune()
	if f.store.metrics.ttl.Load() != 1 {
		t.Fatal("TTL not counted")
	}
	before := counters.render()
	f.store.db.Close()
	f.store.observe()
	if counters.render() != before {
		t.Fatal("unavailable observation became empty")
	}
}

func TestArtifactForwarderRefusesRedirectsAndUnavailableOwner(t *testing.T) {
	owner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "http://attacker.invalid/")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	target, _ := url.Parse(owner.URL)
	forward := artifactForwarder(target)
	for range 2 {
		w := httptest.NewRecorder()
		forward.ServeHTTP(w, httptest.NewRequest(http.MethodPost, artifactService+"CreateArtifact", nil))
		if w.Code != http.StatusServiceUnavailable || w.Header().Get("Location") != "" {
			t.Fatal("unsafe redirect escaped")
		}
		owner.Close()
	}
}

func TestArtifactConfigurationIsDefaultOffAndStartupFailsClosed(t *testing.T) {
	disabled := &artifactConfig{}
	if !disabled.valid() {
		t.Fatal("default invalid")
	}
	h, b, closeDisabled, err := disabled.open(nil, &Counters{})
	closeDisabled()
	if err != nil || h != nil || b != nil {
		t.Fatal("default enabled artifacts")
	}
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	if withArtifacts(next, nil, nil, 0, &Counters{}) == nil {
		t.Fatal("disabled handler missing")
	}
	dir := t.TempDir()
	c := &artifactConfig{listen: "127.0.0.1:0", dir: filepath.Join(dir, "owner"), origin: "http://gate.test", repos: "123", budgetPath: filepath.Join(dir, "absent"), cap: 1 << 20, floor: 1, cachePeak: 1}
	if _, _, _, err = c.open(nil, &Counters{}); err == nil {
		t.Fatal("missing budget accepted")
	}
	c.budgetPath = dir
	c.floor = 1 << 60
	if _, _, _, err = c.open(nil, &Counters{}); err == nil {
		t.Fatal("unavailable write reserve accepted")
	}
	c.floor = 1
	c.dir = filepath.Join(dir, "not-directory")
	os.WriteFile(c.dir, []byte("file"), 0600)
	if _, _, _, err = c.open(nil, &Counters{}); err == nil {
		t.Fatal("unusable storage accepted")
	}
	for _, args := range [][]string{{"--artifacts-upstream", "http://public.invalid:3020"}, {"--artifacts-dir", dir}, {"--artifacts-upstream", "http://127.0.0.1:3017", "--write-budget-path", dir, "--listen", "0.0.0.0:3017"}} {
		if Execute(context.Background(), args, io.Discard, io.Discard) != 2 {
			t.Fatal("invalid flags accepted")
		}
	}
	if _, err = artifactFreeSpace(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("stat failure hidden")
	}
	if sameArtifactFilesystem(dir, filepath.Join(dir, "missing")) {
		t.Fatal("unknown filesystem treated as shared")
	}
}

func TestArtifactBlobFailureBoundaries(t *testing.T) {
	f := newArtifactFixture(t)
	auth := f.token("123", "r", "j")
	raw := f.create(auth, "r", "j", "a")
	req := httptest.NewRequest(http.MethodGet, artifactService+"CreateArtifact", nil)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatal("RPC GET accepted")
	}
	path := artifactBlob + "invalid-id"
	expiry := fmt.Sprint(f.at.Add(time.Minute).Unix())
	signed := f.origin + path + "?mode=write&expires=" + expiry + "&sig=" + f.store.signature(path, "write", expiry)
	if f.blob(signed, http.MethodPut, nil).Code != 403 {
		t.Fatal("nonnumeric capability ID accepted")
	}
	f.budget.probe = func() (int64, error) { return 0, nil }
	if f.blob(raw+"&comp=block&blockid=YQ==", http.MethodPut, nil).Code != 503 {
		t.Fatal("block bypassed write budget")
	}
	f.budget.probe = func() (int64, error) { return 1 << 40, nil }
	req = httptest.NewRequest(http.MethodPut, raw+"&comp=block&blockid=YQ==", iotest.ErrReader(io.ErrUnexpectedEOF))
	w = httptest.NewRecorder()
	f.handler.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatal("interrupted PUT acknowledged")
	}
	f.rpc(auth, "DeleteArtifact", artifactBody("r", "j", "a"))
	if f.blob(raw+"&comp=block&blockid=YQ==", http.MethodPut, nil).Code != 404 {
		t.Fatal("deleted capability survived")
	}
	raw = f.create(auth, "r", "j", "expired")
	f.store.db.Exec("UPDATE artifacts SET expires=?", f.at.Unix())
	if f.blob(raw+"&comp=block&blockid=YQ==", http.MethodPut, nil).Code != 404 {
		t.Fatal("expired object survived capability")
	}
	if f.rpc(auth, "GetSignedArtifactURL", artifactBody("r", "other-job", "expired")).Code != 404 {
		t.Fatal("wrong producer accepted")
	}
}

type artifactBrokenResponse struct{ header http.Header }

func (w artifactBrokenResponse) Header() http.Header     { return w.header }
func (artifactBrokenResponse) WriteHeader(int)           {}
func (artifactBrokenResponse) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func TestArtifactInterruptedDownloadAbortsAndReleasesLease(t *testing.T) {
	f := newArtifactFixture(t)
	auth := f.token("123", "r", "j")
	f.upload(auth, "r", "j", "a", []byte("data"))
	a, _ := f.store.named(artifactPrincipal{repo: "123", run: "r"}, "a")
	signed := f.store.signedURL(f.origin, a, "read")
	defer func() {
		if err, ok := recover().(error); !ok || !errors.Is(err, http.ErrAbortHandler) {
			t.Error("broken download did not abort")
		}
		if len(f.store.leases) != 0 {
			t.Error("download lease leaked")
		}
	}()
	f.handler.ServeHTTP(artifactBrokenResponse{header: http.Header{}}, httptest.NewRequest(http.MethodGet, signed, nil))
}

func TestArtifactMalformedKeysAndUnknownKeyAreRefused(t *testing.T) {
	f := newArtifactFixture(t)
	for _, body := range []string{"not JSON", `{"keys":[]}`, `{"keys":[{"kty":"EC","kid":"test"}]}`, fmt.Sprintf(`{"keys":[{"kty":"RSA","kid":"test","n":%q,"e":"Ag"}]}`, base64.RawURLEncoding.EncodeToString(f.key.N.Bytes()))} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
		v := &artifactVerifier{url: server.URL, client: server.Client(), clock: func() time.Time { return f.at }}
		if _, err := v.key(context.Background(), "test"); err == nil {
			t.Fatal("unusable signing key accepted")
		}
		server.Close()
	}
	v := &artifactVerifier{url: "%", client: http.DefaultClient, clock: func() time.Time { return f.at }}
	if _, err := v.key(context.Background(), "test"); err == nil {
		t.Fatal("invalid key endpoint accepted")
	}
	v = &artifactVerifier{url: f.keys.URL, client: f.keys.Client(), clock: func() time.Time { return f.at }}
	if _, err := v.key(context.Background(), "unknown"); err == nil {
		t.Fatal("unknown key allowed")
	}
}

func TestArtifactGuardedNPMConfigurationAndBrokenCacheBody(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	registry, _ := url.Parse("http://registry.test")
	npm := newNPMCache(dir, registry)
	config := &artifactConfig{listen: "127.0.0.1:0", dir: dir, origin: "http://gate.test", repos: "123", budgetPath: dir, cap: 1024, floor: 1, cachePeak: 1}
	_, _, closeOwner, err := config.open(npm, &Counters{})
	if err != nil {
		t.Fatal(err)
	}
	closeOwner()
	if npm.budget == nil {
		t.Fatal("npm not guarded")
	}
	badNPM := newNPMCache(filepath.Join(dir, "npm"), registry)
	os.Remove(badNPM.dir)
	if _, _, _, err = config.open(badNPM, &Counters{}); err == nil {
		t.Fatal("unknown npm filesystem allowed")
	}
	b := &writeBudget{floor: 1, probe: func() (int64, error) { return 1 << 40, nil }}
	counters := &Counters{}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err == nil {
			t.Error("broken stream became successful")
		}
		w.WriteHeader(http.StatusBadGateway)
	})
	handler := withArtifacts(next, next, b, 1, counters)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/upload/id", iotest.ErrReader(io.ErrUnexpectedEOF)))
	if w.Code != http.StatusBadGateway {
		t.Fatal("broken cache upload acknowledged")
	}
	if !strings.Contains(counters.render(), "fleet_cache_gate_write_reserved_bytes 0") {
		t.Fatal("budget metric missing or leaked")
	}
}

func TestArtifactSharedReservationPreventsNPMPublication(t *testing.T) {
	_, registry := newRegistryStub(t, answer("bytes"))
	fixture := newNPMFixture(t, registry)
	budget := &writeBudget{floor: 10, probe: func() (int64, error) { return 100000, nil }}
	fixture.cache.budget = budget
	release, err := budget.reserve(99980)
	if err != nil {
		t.Fatal(err)
	}
	if status, body, _ := fixture.get(t, testTarball); status != http.StatusOK || body != "bytes" {
		t.Fatal("read-through failed while writes gated")
	}
	if len(fixture.files(t)) != 0 {
		t.Fatal("npm published through another writer's reservation")
	}
	release()
	fixture.get(t, testTarball)
	if len(fixture.files(t)) != 1 {
		t.Fatal("npm failed to publish when reserve returned")
	}
	budget.mu.Lock()
	reserved := budget.reserved
	budget.mu.Unlock()
	if reserved != 0 {
		t.Fatal("publication leaked budget")
	}
}
