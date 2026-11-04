package cachegate

import (
	"context"
	"crypto/ed25519"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	key, err := LoadOrCreateIdentity(filepath.Join(t.TempDir(), "identity.key"))
	if err != nil || key == nil {
		t.Fatalf("key = %v, err = %v", key, err)
	}
	return key
}

func verifierFor(t *testing.T, server *httptest.Server, public ed25519.PublicKey) Verifier {
	t.Helper()
	gate, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return Verifier{Gate: gate, Public: public}
}

// TestAForeignGateAnsweringHealthyIsNotVerified is issue #446: a foreign host
// holding the gate's address on someone else's 192.168.1.0/24 can answer
// anything, and a guest pointed at it hands over bearer runtime tokens.
func TestAForeignGateAnsweringHealthyIsNotVerified(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "healthy")
	}))
	defer server.Close()
	if verifierFor(t, server, testKey(t).Public().(ed25519.PublicKey)).Verified(context.Background()) {
		t.Fatal("an answer that proves nothing verified the gate")
	}
}

func TestTheGateProvesItsIdentity(t *testing.T) {
	key := testKey(t)
	server := httptest.NewServer(identityHandler(key, http.NotFoundHandler()))
	defer server.Close()
	if !verifierFor(t, server, key.Public().(ed25519.PublicKey)).Verified(context.Background()) {
		t.Fatal("the real gate was not verified")
	}
}

func TestAGateHoldingAnotherKeyIsNotVerified(t *testing.T) {
	server := httptest.NewServer(identityHandler(testKey(t), http.NotFoundHandler()))
	defer server.Close()
	if verifierFor(t, server, testKey(t).Public().(ed25519.PublicKey)).Verified(context.Background()) {
		t.Fatal("a signature from another key verified the gate")
	}
}

// TestAReplayedSignatureIsNotVerified: a recorded answer cannot pass, because
// every challenge is new.
func TestAReplayedSignatureIsNotVerified(t *testing.T) {
	key := testKey(t)
	real := httptest.NewServer(identityHandler(key, http.NotFoundHandler()))
	defer real.Close()
	response, err := real.Client().Get(real.URL + IdentityPath + "?nonce=" + strings.Repeat("ab", nonceBytes))
	if err != nil {
		t.Fatal(err)
	}
	recorded, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	replay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(recorded) }))
	defer replay.Close()
	if verifierFor(t, replay, key.Public().(ed25519.PublicKey)).Verified(context.Background()) {
		t.Fatal("a replayed signature verified the gate")
	}
}

func TestVerificationFailsClosed(t *testing.T) {
	key := testKey(t)
	public := key.Public().(ed25519.PublicKey)
	hung := make(chan struct{})
	servers := map[string]http.HandlerFunc{
		"redirect": func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/elsewhere", http.StatusFound) },
		"error":    func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) },
		"garbage":  func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "not base64 !") },
		"short":    func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"signature":"c2hvcnQ="}`) },
		"empty":    func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{}`) },
		"hung":     func(http.ResponseWriter, *http.Request) { <-hung },
	}
	for name, handler := range servers {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(handler)
			defer server.Close()
			verifier := verifierFor(t, server, public)
			verifier.Timeout = 50 * time.Millisecond
			if name == "hung" {
				defer close(hung)
			}
			if verifier.Verified(context.Background()) {
				t.Fatal("verified")
			}
		})
	}
	t.Run("unreachable", func(t *testing.T) {
		server := httptest.NewServer(http.NotFoundHandler())
		verifier := verifierFor(t, server, public)
		server.Close()
		if verifier.Verified(context.Background()) {
			t.Fatal("verified")
		}
	})
	t.Run("no entropy", func(t *testing.T) {
		server := httptest.NewServer(identityHandler(key, http.NotFoundHandler()))
		defer server.Close()
		verifier := verifierFor(t, server, public)
		verifier.Random = failingReader{}
		if verifier.Verified(context.Background()) {
			t.Fatal("verified")
		}
	})
	t.Run("no pinned key", func(t *testing.T) {
		server := httptest.NewServer(identityHandler(key, http.NotFoundHandler()))
		defer server.Close()
		if verifierFor(t, server, nil).Verified(context.Background()) {
			t.Fatal("verified")
		}
	})
	t.Run("bad url", func(t *testing.T) {
		verifier := Verifier{Gate: &url.URL{Scheme: "http", Host: "bad host"}, Public: public}
		if verifier.Verified(context.Background()) {
			t.Fatal("verified")
		}
	})
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestAVerifierNeverFollowsARedirectToARealGate(t *testing.T) {
	key := testKey(t)
	real := httptest.NewServer(identityHandler(key, http.NotFoundHandler()))
	defer real.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, real.URL+r.URL.RequestURI(), http.StatusFound)
	}))
	defer redirect.Close()
	gate, _ := url.Parse(redirect.URL)
	if (Verifier{Gate: gate, Public: key.Public().(ed25519.PublicKey)}).Verified(context.Background()) {
		t.Fatal("a redirect verified the gate")
	}
}

func TestTheIdentityEndpointRefusesABadChallenge(t *testing.T) {
	server := httptest.NewServer(identityHandler(testKey(t), http.NotFoundHandler()))
	defer server.Close()
	for _, query := range []string{"", "?nonce=zz", "?nonce=abcd"} {
		response, err := server.Client().Get(server.URL + IdentityPath + query)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("%q = %d", query, response.StatusCode)
		}
	}
}

// TestAGateWithoutAKeyDoesNotForwardTheChallenge keeps the identity path off the
// results proxy: a gate that has no key says so, and GitHub is never asked.
func TestAGateWithoutAKeyDoesNotForwardTheChallenge(t *testing.T) {
	forwarded := false
	server := httptest.NewServer(identityHandler(nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { forwarded = true })))
	defer server.Close()
	response, err := server.Client().Get(server.URL + IdentityPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound || forwarded {
		t.Fatalf("status = %d, forwarded = %v", response.StatusCode, forwarded)
	}
}

func TestOtherPathsPassThrough(t *testing.T) {
	server := httptest.NewServer(identityHandler(testKey(t), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})))
	defer server.Close()
	response, err := server.Client().Get(server.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusTeapot {
		t.Fatalf("status = %d", response.StatusCode)
	}
}

func TestTheIdentityKeyPersistsAndStaysPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.key")
	first, err := LoadOrCreateIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("info = %v, err = %v", info, err)
	}
	second, err := LoadOrCreateIdentity(path)
	if err != nil || !first.Equal(second) {
		t.Fatalf("second = %v, err = %v", second, err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateIdentity(path); err == nil {
		t.Fatal("a group-readable key was accepted")
	}
}

func TestABadIdentityKeyStopsTheGate(t *testing.T) {
	dir := t.TempDir()
	corrupt := filepath.Join(dir, "corrupt.key")
	if err := os.WriteFile(corrupt, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateIdentity(corrupt); err == nil {
		t.Fatal("a corrupt key was accepted")
	}
	short := filepath.Join(dir, "short.key")
	if err := os.WriteFile(short, []byte("c2hvcnQ=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateIdentity(short); err == nil {
		t.Fatal("a short key was accepted")
	}
	if _, err := LoadOrCreateIdentity(filepath.Join(dir, "missing", "identity.key")); err == nil {
		t.Fatal("an uncreatable key was accepted")
	}
	if _, err := LoadOrCreateIdentity(dir); err == nil {
		t.Fatal("a directory was accepted")
	}
}

func TestExecuteServesTheIdentityAndLogsOnlyThePublicKey(t *testing.T) {
	target, _ := upstream(t)
	address := freeAddress(t)
	path := filepath.Join(t.TempDir(), "identity.key")
	ctx, cancel := context.WithCancel(context.Background())
	var stdout syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- Execute(ctx, []string{"--listen", address, "--upstream", target.String(), "--results-upstream",
			target.String(), "--identity-key", path}, &stdout, io.Discard)
	}()
	waitFor(t, "http://"+address+"/health")
	key, err := LoadOrCreateIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	gate, _ := url.Parse("http://" + address)
	if !(Verifier{Gate: gate, Public: key.Public().(ed25519.PublicKey)}).Verified(context.Background()) {
		t.Fatal("the running gate was not verified")
	}
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("exit = %d", code)
	}
	raw, _ := os.ReadFile(path)
	if log := stdout.String(); !strings.Contains(log, PublicKeyString(key.Public().(ed25519.PublicKey))) ||
		strings.Contains(log, strings.TrimSpace(string(raw))) {
		t.Fatalf("log = %q", log)
	}
}

func TestExecuteStopsOnABadIdentityKey(t *testing.T) {
	target, _ := upstream(t)
	path := filepath.Join(t.TempDir(), "identity.key")
	if err := os.WriteFile(path, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	if code := Execute(context.Background(), []string{"--listen", freeAddress(t), "--upstream", target.String(),
		"--identity-key", path}, io.Discard, &stderr); code != 1 || !strings.Contains(stderr.String(), "identity") {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
}

func TestParsePublicKey(t *testing.T) {
	key := testKey(t)
	encoded := PublicKeyString(key.Public().(ed25519.PublicKey))
	parsed, err := ParsePublicKey(encoded)
	if err != nil || !parsed.Equal(key.Public()) {
		t.Fatalf("parsed = %v, err = %v", parsed, err)
	}
	for _, bad := range []string{"", "!!", "c2hvcnQ="} {
		if _, err := ParsePublicKey(bad); err == nil {
			t.Fatalf("%q was accepted", bad)
		}
	}
}

// syncBuffer lets the test read what the gate's goroutine logged.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestTheIdentityAnswerIsTypedAndEchoesNothing keeps a request-controlled value
// out of a body a browser could render: the answer is declared JSON, is never
// sniffed, and holds a signature rather than any byte of the challenge.
func TestTheIdentityAnswerIsTypedAndEchoesNothing(t *testing.T) {
	server := httptest.NewServer(identityHandler(testKey(t), http.NotFoundHandler()))
	defer server.Close()
	nonce := strings.Repeat("ab", nonceBytes)
	response, err := server.Client().Get(server.URL + IdentityPath + "?nonce=" + nonce)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	body, _ := io.ReadAll(response.Body)
	if got := response.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := response.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q", got)
	}
	if strings.Contains(string(body), nonce) || !strings.HasPrefix(string(body), `{"signature":"`) {
		t.Fatalf("body = %q", body)
	}
}
