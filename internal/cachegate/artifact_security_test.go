package cachegate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	"time"

	"github.com/golang-jwt/jwt/v4"
)

func TestArtifactJWTContract(t *testing.T) {
	f := newArtifactFixture(t)
	for name, edit := range map[string]func(jwt.MapClaims){
		"wrong issuer":    func(c jwt.MapClaims) { c["iss"] = "https://attacker.invalid" },
		"expired":         func(c jwt.MapClaims) { c["exp"] = f.at.Unix() },
		"missing expiry":  func(c jwt.MapClaims) { delete(c, "exp") },
		"future":          func(c jwt.MapClaims) { c["nbf"] = f.at.Add(time.Hour).Unix() },
		"missing repo":    func(c jwt.MapClaims) { delete(c, "repository_id") },
		"missing scope":   func(c jwt.MapClaims) { delete(c, "scp") },
		"malformed scope": func(c jwt.MapClaims) { c["scp"] = "Actions.Results:r" },
		"empty job":       func(c jwt.MapClaims) { c["scp"] = "Actions.Results:r:" },
		"ambiguous":       func(c jwt.MapClaims) { c["scp"] = "Actions.Results:r:j Actions.Results:r:k" },
		"unrelated":       func(c jwt.MapClaims) { c["scp"] = "Actions.Cache:r:j" },
	} {
		t.Run(name, func(t *testing.T) {
			c := jwt.MapClaims{"iss": artifactIssuer, "exp": f.at.Add(time.Hour).Unix(), "repository_id": "123", "scp": "Actions.Results:r:j"}
			edit(c)
			tok := jwt.NewWithClaims(jwt.SigningMethodRS256, c)
			tok.Header["kid"] = "test"
			raw, err := tok.SignedString(f.key)
			if err != nil {
				t.Fatal(err)
			}
			if w := f.rpc(raw, "CreateArtifact", artifactBody("r", "j", "denied")); w.Code != 403 {
				t.Fatalf("status %d", w.Code)
			}
		})
	}
	for _, raw := range []string{"", "bad.jwt", "a.b.c"} {
		if w := f.rpc(raw, "ListArtifacts", artifactBody("r", "j", "")); w.Code != 403 {
			t.Fatal("malformed token allowed")
		}
	}
}

func TestArtifactCapabilitiesAndProtocolErrors(t *testing.T) {
	f := newArtifactFixture(t)
	auth := f.token("123", "r", "j")
	raw := f.create(auth, "r", "j", "build")
	for _, request := range []struct {
		url, method, body string
		status            int
	}{
		{raw + "&comp=block&blockid=bad!", http.MethodPut, "x", 400},
		{raw + "&comp=blocklist", http.MethodPut, "<broken>", 400},
		{raw + "&comp=blocklist", http.MethodPut, "<BlockList><Latest>bWlzc2luZw==</Latest></BlockList>", 400},
		{raw + "&comp=unknown", http.MethodPut, "", 400},
		{strings.Replace(raw, "mode=write", "mode=read", 1), http.MethodGet, "", 403},
		{strings.Replace(raw, "/blobs/1", "/blobs/2", 1), http.MethodPut, "", 403},
		{raw, http.MethodGet, "", 403},
	} {
		w := f.blob(request.url, request.method, []byte(request.body))
		if w.Code != request.status {
			t.Fatalf("protocol refusal %d want %d", w.Code, request.status)
		}
	}
	b := artifactBody("r", "j", "build")
	b["size"] = "1"
	if w := f.rpc(auth, "FinalizeArtifact", b); w.Code == 200 {
		t.Fatal("uncommitted artifact finalized")
	}
	if w := f.rpc(auth, "GetSignedArtifactURL", b); w.Code != 404 {
		t.Fatal("unfinalized artifact readable")
	}
	f.upload(auth, "r", "j", "other", []byte("abc"))
	b = artifactBody("r", "j", "other")
	b["size"] = "4"
	if f.rpc(auth, "FinalizeArtifact", b).Code != 400 {
		t.Fatal("wrong size accepted")
	}
	b["size"] = "3"
	b["hash"] = "sha256:wrong"
	if f.rpc(auth, "FinalizeArtifact", b).Code != 400 {
		t.Fatal("wrong hash accepted")
	}
	b["hash"] = ""
	if f.rpc(auth, "FinalizeArtifact", b).Code != 200 {
		t.Fatal("idempotent finalize failed")
	}
	if f.rpc(auth, "UnknownMethod", b).Code != 400 {
		t.Fatal("unknown RPC accepted")
	}
	if f.rpc(auth, "GetSignedArtifactURL", artifactBody("r", "j", "absent")).Code != 404 {
		t.Fatal("missing artifact not explicit")
	}
	if w := f.rpc(auth, "DeleteArtifact", b); w.Code != 200 {
		t.Fatal("delete failed")
	}
	if f.rpc(auth, "GetSignedArtifactURL", b).Code != 404 {
		t.Fatal("deleted artifact readable")
	}
	f.create(auth, "r", "j", "other") // name reuse gets a new ID
}

func TestArtifactOrderedBlocksFiltersAndEviction(t *testing.T) {
	f := newArtifactFixture(t)
	auth := f.token("123", "r", "j")
	raw := f.create(auth, "r", "j", "ordered")
	// Network delivery order differs from block-list order. Retried PUTs replace,
	// not append. ZIP-sized payloads remain bounded by one block in memory.
	first := bytes.Repeat([]byte("a"), 8<<20)
	second := []byte("tail")
	ids := []string{"Zmlyc3Q=", "c2Vjb25k"}
	for _, i := range []int{1, 0, 0} {
		data := first
		if i == 1 {
			data = second
		}
		if f.blob(raw+"&comp=block&blockid="+url.QueryEscape(ids[i]), http.MethodPut, data).Code != 201 {
			t.Fatal("block PUT failed")
		}
	}
	xml := "<BlockList><Latest>" + ids[0] + "</Latest><Latest>" + ids[1] + "</Latest></BlockList>"
	if f.blob(raw+"&comp=blocklist", http.MethodPut, []byte(xml)).Code != 201 {
		t.Fatal("commit failed")
	}
	b := artifactBody("r", "j", "ordered")
	b["size"] = fmt.Sprint(len(first) + len(second))
	sum := sha256.Sum256(append(first, second...))
	b["hash"] = "sha256:" + hex.EncodeToString(sum[:])
	if f.rpc(auth, "FinalizeArtifact", b).Code != 200 {
		t.Fatal("ordered finalize failed")
	}
	w := f.rpc(auth, "GetSignedArtifactURL", b)
	var res struct {
		URL string `json:"signed_url"`
	}
	json.Unmarshal(w.Body.Bytes(), &res)
	if w = f.blob(res.URL, http.MethodGet, nil); !bytes.Equal(w.Body.Bytes(), append(first, second...)) {
		t.Fatal("block order corrupted bytes")
	}
	b["name_filter"] = "missing"
	if strings.Contains(f.rpc(auth, "ListArtifacts", b).Body.String(), "ordered") {
		t.Fatal("name filter ignored")
	}
	b["name_filter"] = ""
	b["id_filter"] = "999"
	if strings.Contains(f.rpc(auth, "ListArtifacts", b).Body.String(), "ordered") {
		t.Fatal("id filter ignored")
	}
	f.store.cap = 10
	f.upload(auth, "r", "j", "small", []byte("12345"))
	if strings.Contains(f.rpc(auth, "ListArtifacts", artifactBody("r", "j", "")).Body.String(), "ordered") {
		t.Fatal("LRU did not evict")
	}
	f.store.cap = 1
	if err := f.store.makeRoom(2, 0); !errors.Is(err, errArtifactSpace) {
		t.Fatal("oversize entry admitted")
	}
}

func TestArtifactExpiryAndStagingBounds(t *testing.T) {
	f := newArtifactFixture(t)
	auth := f.token("123", "r", "j")
	for _, offset := range []time.Duration{-time.Hour, time.Hour, 100 * time.Hour} {
		b := artifactBody("r", "j", fmt.Sprint(offset))
		b["expires_at"] = f.at.Add(offset).Format(time.RFC3339)
		w := f.rpc(auth, "CreateArtifact", b)
		if offset < 0 {
			if w.Code != 400 {
				t.Fatal("expired create allowed")
			}
			continue
		}
		if w.Code != 200 {
			t.Fatal("expiry create failed")
		}
		a, err := f.store.named(artifactPrincipal{repo: "123", run: "r"}, fmt.Sprint(offset))
		if err != nil {
			t.Fatal(err)
		}
		if a.Expires != f.at.Add(min(offset, 72*time.Hour)).Unix() {
			t.Fatal("expiry clamp wrong")
		}
	}
	raw := f.create(auth, "r", "j", "staged")
	_ = raw
	f.at = f.at.Add(time.Hour)
	if err := f.store.makeRoom(0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.named(artifactPrincipal{repo: "123", run: "r"}, "staged"); !errors.Is(err, errArtifactMissing) {
		t.Fatal("stale partial retained")
	}
}

func TestArtifactConfiguredModesAndSharedGuard(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	base := artifactConfig{dir: dir, origin: "http://host.containers.internal:3017", repos: "123", budgetPath: dir, cap: 1 << 20, floor: 1, cachePeak: 1024}
	if !base.valid() {
		t.Fatal("valid owner refused")
	}
	h, b, closeOwner, err := base.open(nil, &Counters{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeOwner()
	if h == nil || b == nil {
		t.Fatal("owner disabled")
	}
	for _, edit := range []func(*artifactConfig){func(c *artifactConfig) { c.upstream = "http://127.0.0.1:3020" }, func(c *artifactConfig) { c.repos = "bad" }, func(c *artifactConfig) { c.origin = "http://x/a" }, func(c *artifactConfig) { c.floor = 0 }, func(c *artifactConfig) { c.budgetPath = "relative" }} {
		c := base
		edit(&c)
		if c.valid() {
			t.Fatal("invalid config accepted")
		}
	}
	for _, raw := range []string{"http://example.com:3020", "https://127.0.0.1:3020", "http://127.0.0.1:3020/path", "http://user@127.0.0.1:3020"} {
		c := artifactConfig{upstream: raw, budgetPath: dir, cap: 1, floor: 1, cachePeak: 1}
		if c.valid() {
			t.Fatal("unsafe forwarding target accepted")
		}
	}
	forwarding := artifactConfig{upstream: "http://127.0.0.1:3020", budgetPath: dir, cap: 1, floor: 1, cachePeak: 1}
	if !forwarding.valid() {
		t.Fatal("SSH forward refused")
	}
	_, _, closeForward, err := forwarding.open(nil, &Counters{})
	if err != nil {
		t.Fatal(err)
	}
	closeForward()
	if !artifactForwardLoop("0.0.0.0:3020", forwarding.upstream) {
		t.Fatal("proxy loop not detected")
	}
	count := 0
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	})
	budget := &writeBudget{floor: 10, probe: func() (int64, error) { return 0, io.ErrUnexpectedEOF }}
	handler := withArtifacts(next, next, budget, 1024, &Counters{})
	for _, path := range []string{"/upload/id?comp=block", "/devstoreaccount1/upload/id?comp=blocklist", cacheService + "FinalizeCacheEntryUpload"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodPut, path, strings.NewReader("x")))
		if w.Code != 503 {
			t.Fatal("cache bypassed shared guard")
		}
	}
	if count != 0 {
		t.Fatal("refused write reached backend")
	}
	budget.probe = func() (int64, error) { return 1 << 40, nil }
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/upload/id?comp=block", strings.NewReader("x")))
	if w.Code != 200 || count != 1 {
		t.Fatal("guard refused safe cache write")
	}
	var output bytes.Buffer
	writer := &budgetWriter{Writer: &output, budget: budget}
	if _, err = writer.Write([]byte("npm")); err != nil {
		t.Fatal(err)
	}
	writer.close()
	if budget.reserved != 0 {
		t.Fatal("npm reservation leaked")
	}
	budget.probe = func() (int64, error) { return 0, nil }
	if _, err = writer.Write([]byte("npm")); err == nil {
		t.Fatal("npm bypassed disk floor")
	}
	f := newArtifactFixture(t)
	auth := f.token("123", "r", "j")
	f.upload(auth, "r", "j", "readable", []byte("x"))
	f.budget.probe = func() (int64, error) { return 0, nil }
	if f.rpc(auth, "CreateArtifact", artifactBody("r", "j", "blocked")).Code != 503 {
		t.Fatal("owner bypassed disk floor")
	}
	if f.rpc(auth, "ListArtifacts", artifactBody("r", "j", "")).Code != 200 {
		t.Fatal("disk floor blocked reads")
	}
}

func TestArtifactKeyRefreshFailureCannotExtendTrust(t *testing.T) {
	f := newArtifactFixture(t)
	v := &artifactVerifier{url: f.keys.URL, client: f.keys.Client(), clock: func() time.Time { return f.at }}
	raw := f.token("123", "r", "j")
	if _, err := v.verify(context.Background(), "Bearer "+raw); err != nil {
		t.Fatal(err)
	}
	f.keys.Close()
	f.at = f.at.Add(11 * time.Minute)
	if _, err := v.verify(context.Background(), "Bearer "+raw); err == nil {
		t.Fatal("stale JWKS allowed")
	}
	if _, err := v.verify(context.Background(), "Bearer "+raw); err == nil {
		t.Fatal("failed refresh renewed old keys")
	}
}

func TestArtifactStoreFailuresAreExplicit(t *testing.T) {
	f := newArtifactFixture(t)
	auth := f.token("123", "r", "j")
	f.store.db.Close()
	if w := f.rpc(auth, "ListArtifacts", artifactBody("r", "j", "")); w.Code != 503 {
		t.Fatal("unavailable database became empty list")
	}
	if _, err := openArtifactStore(filepath.Join(t.TempDir(), "bad"), 0, func() time.Time { return f.at }, f.budget); err == nil {
		t.Fatal("zero cap allowed")
	}
	private := t.TempDir()
	os.Chmod(private, 0700)
	path := filepath.Join(private, "file")
	os.WriteFile(path, []byte("not a database"), 0600)
	if _, err := openArtifactStore(path, 1<<20, func() time.Time { return f.at }, f.budget); err == nil {
		t.Fatal("corrupt database accepted")
	}
}

func TestArtifactSlowSiteAUploadCanContinuePastFifteenMinutes(t *testing.T) {
	f := newArtifactFixture(t)
	raw := f.create(f.token("123", "r", "j"), "r", "j", "slow")
	f.at = f.at.Add(16 * time.Minute)
	if w := f.blob(raw+"&comp=block&blockid=YQ==", http.MethodPut, []byte("block")); w.Code != http.StatusCreated {
		t.Fatal("site-A upload expired before a 20 GiB transfer could finish")
	}
	f.at = f.at.Add(6 * time.Hour)
	if w := f.blob(raw+"&comp=block&blockid=Yg==", http.MethodPut, []byte("block")); w.Code != http.StatusForbidden {
		t.Fatal("write capability never expired")
	}
}

func TestArtifactDeletionReservesItsSQLiteJournal(t *testing.T) {
	f := newArtifactFixture(t)
	auth := f.token("123", "r", "j")
	f.upload(auth, "r", "j", "a", []byte("bytes"))
	f.budget.probe = func() (int64, error) { return 0, nil }
	if f.rpc(auth, "DeleteArtifact", artifactBody("r", "j", "a")).Code != http.StatusServiceUnavailable {
		t.Fatal("deletion journal bypassed disk floor")
	}
	if _, err := f.store.named(artifactPrincipal{repo: "123", run: "r"}, "a"); err != nil {
		t.Fatal("refused deletion changed artifact")
	}
	if f.rpc(auth, "ListArtifacts", artifactBody("r", "j", "")).Code != http.StatusOK {
		t.Fatal("write floor blocked a read")
	}
}
