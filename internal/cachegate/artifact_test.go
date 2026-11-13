package cachegate

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"
)

type artifactFixture struct {
	t       *testing.T
	at      time.Time
	key     *rsa.PrivateKey
	store   *artifactStore
	handler http.Handler
	origin  string
	keys    *httptest.Server
	budget  *writeBudget
}

func newArtifactFixture(t *testing.T) *artifactFixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &artifactFixture{t: t, at: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC), key: key, origin: "http://gate.test"}
	f.keys = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{"kty": "RSA", "kid": "test", "alg": "RS256", "use": "sig", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}})
	}))
	t.Cleanup(f.keys.Close)
	f.budget = &writeBudget{floor: 100, probe: func() (int64, error) { return 1 << 40, nil }}
	f.store, err = openArtifactStore(filepath.Join(t.TempDir(), "owner", "artifacts.db"), 32<<20, func() time.Time { return f.at }, f.budget)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.store.db.Close() })
	v := &artifactVerifier{url: f.keys.URL, client: f.keys.Client(), clock: func() time.Time { return f.at }}
	f.handler = artifactHandler(f.store, v, f.origin, map[string]bool{"123": true})
	return f
}

func (f *artifactFixture) token(repo, run, job string) string {
	f.t.Helper()
	c := jwt.MapClaims{"iss": artifactIssuer, "exp": f.at.Add(time.Hour).Unix(), "nbf": f.at.Add(-time.Minute).Unix(), "repository_id": repo, "scp": "Actions.Results:" + run + ":" + job, "ac": "[{\"Scope\":\"refs/pull/7/merge\",\"Permission\":3}]"}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, c)
	token.Header["kid"] = "test"
	signed, err := token.SignedString(f.key)
	if err != nil {
		f.t.Fatal(err)
	}
	return signed
}
func (f *artifactFixture) rpc(auth, method string, body map[string]any) *httptest.ResponseRecorder {
	f.t.Helper()
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, artifactService+method, bytes.NewReader(b))
	r.Header.Set("Authorization", "Bearer "+auth)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}
func artifactBody(run, job, name string) map[string]any {
	return map[string]any{"workflow_run_backend_id": run, "workflow_job_run_backend_id": job, "name": name, "version": 4}
}
func (f *artifactFixture) create(auth, run, job, name string) string {
	f.t.Helper()
	w := f.rpc(auth, "CreateArtifact", artifactBody(run, job, name))
	if w.Code != 200 {
		f.t.Fatalf("create status %d", w.Code)
	}
	var res struct {
		URL string `json:"signed_upload_url"`
	}
	json.Unmarshal(w.Body.Bytes(), &res)
	if res.URL == "" {
		f.t.Fatal("missing upload URL")
	}
	return res.URL
}
func (f *artifactFixture) blob(raw, method string, body []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, raw, bytes.NewReader(body))
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}
func (f *artifactFixture) upload(auth, run, job, name string, data []byte) string {
	f.t.Helper()
	raw := f.create(auth, run, job, name)
	id := base64.StdEncoding.EncodeToString([]byte("block-1"))
	if w := f.blob(raw+"&comp=block&blockid="+url.QueryEscape(id), "PUT", data); w.Code != 201 {
		f.t.Fatalf("block %d", w.Code)
	}
	if w := f.blob(raw+"&comp=blocklist", "PUT", []byte("<BlockList><Latest>"+id+"</Latest></BlockList>")); w.Code != 201 {
		f.t.Fatalf("commit %d", w.Code)
	}
	body := artifactBody(run, job, name)
	body["size"] = fmt.Sprint(len(data))
	h := sha256.Sum256(data)
	body["hash"] = "sha256:" + hex.EncodeToString(h[:])
	w := f.rpc(auth, "FinalizeArtifact", body)
	if w.Code != 200 {
		f.t.Fatalf("finalize %d %s", w.Code, w.Body.String())
	}
	return raw
}

func TestArtifactSecurityAndSiblingHandoff(t *testing.T) {
	f := newArtifactFixture(t)
	owner := f.token("123", "run-a", "job-a")
	otherJob := f.token("123", "run-a", "job-b")
	f.upload(owner, "run-a", "job-a", "build", []byte("zip bytes"))
	for _, bad := range []string{owner[:len(owner)-8] + "forgedxx", f.token("456", "run-a", "job-a"), f.token("123", "run-b", "job-a")} {
		for _, method := range []string{"CreateArtifact", "FinalizeArtifact", "ListArtifacts", "GetSignedArtifactURL", "DeleteArtifact"} {
			w := f.rpc(bad, method, artifactBody("run-a", "job-a", "build"))
			if w.Code == 200 {
				t.Fatalf("%s authorized wrong principal", method)
			}
		}
	}
	w := f.rpc(otherJob, "ListArtifacts", artifactBody("run-a", "job-b", ""))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "build") {
		t.Fatal("sibling list failed")
	}
	w = f.rpc(otherJob, "GetSignedArtifactURL", artifactBody("run-a", "job-a", "build"))
	var got struct {
		URL string `json:"signed_url"`
	}
	json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != 200 || f.blob(got.URL, "GET", nil).Body.String() != "zip bytes" {
		t.Fatal("sibling download failed")
	}
	if w = f.rpc(otherJob, "FinalizeArtifact", artifactBody("run-a", "job-a", "build")); w.Code == 200 {
		t.Fatal("sibling finalized producer upload")
	}
	f.at = f.at.Add(73 * time.Hour)
	if w = f.blob(got.URL, "GET", nil); w.Code == 200 {
		t.Fatal("expired capability succeeded")
	}
}

func TestArtifactDurabilityAndRetention(t *testing.T) {
	f := newArtifactFixture(t)
	auth := f.token("123", "r", "j")
	f.upload(auth, "r", "j", "a", []byte("persisted"))
	path := f.store.path
	f.store.db.Close()
	s, err := openArtifactStore(path, 32<<20, func() time.Time { return f.at }, f.budget)
	if err != nil {
		t.Fatal(err)
	}
	f.store = s
	v := &artifactVerifier{url: f.keys.URL, client: f.keys.Client(), clock: func() time.Time { return f.at }}
	f.handler = artifactHandler(s, v, f.origin, map[string]bool{"123": true})
	if w := f.rpc(auth, "ListArtifacts", artifactBody("r", "j", "")); w.Code != 200 || !strings.Contains(w.Body.String(), "persisted") && !strings.Contains(w.Body.String(), "\"a\"") {
		t.Fatal("restart lost artifact")
	}
	f.at = f.at.Add(72 * time.Hour)
	auth = f.token("123", "r", "j")
	w := f.rpc(auth, "ListArtifacts", artifactBody("r", "j", ""))
	if w.Code != 200 || strings.Contains(w.Body.String(), "\"a\"") {
		t.Fatal("TTL did not expire")
	}
}

func TestArtifactWriteBudgetRefusesAllWriters(t *testing.T) {
	b := &writeBudget{floor: 100, probe: func() (int64, error) { return 120, nil }}
	release, err := b.reserve(15)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.reserve(6); err == nil {
		t.Fatal("concurrent reservation spent reserve")
	}
	release()
	b.probe = func() (int64, error) { return 0, io.ErrUnexpectedEOF }
	if _, err = b.reserve(1); err == nil {
		t.Fatal("unknown disk allowed write")
	}
}

func TestArtifactForwardingIsTransparent(t *testing.T) {
	var seen int
	owner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen++
		if r.Header.Get("Authorization") != "Bearer opaque" {
			t.Error("authorization changed")
		}
		io.WriteString(w, "owner")
	}))
	defer owner.Close()
	target, _ := url.Parse(owner.URL)
	h := artifactForwarder(target)
	for _, path := range []string{artifactService + "CreateArtifact", artifactBlob + "42?sig=opaque"} {
		r := httptest.NewRequest(http.MethodPut, path, strings.NewReader("block"))
		r.Header.Set("Authorization", "Bearer opaque")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Body.String() != "owner" {
			t.Fatal("not forwarded")
		}
	}
	if seen != 2 {
		t.Fatal("missing traffic")
	}
}
