package cachegate

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
)

// TestTheVerifierAsksThroughItsFetch is issue #537: a macOS node must be able to
// route the proof through a client Local Network privacy lets reach the LAN.
func TestTheVerifierAsksThroughItsFetch(t *testing.T) {
	key := testKey(t)
	server := httptest.NewServer(identityHandler(key, http.NotFoundHandler()))
	defer server.Close()
	calls := 0
	verifier := verifierFor(t, server, key.Public().(ed25519.PublicKey))
	verifier.Fetch = func(ctx context.Context, target string) (int, []byte, error) {
		calls++
		return directFetch(ctx, target)
	}
	if !verifier.Verified(context.Background()) || calls != 1 {
		t.Fatalf("verified through the fetch: calls = %d", calls)
	}
}

// TestProveNamesWhyTheGateWasRefused is issue #537: a launchd agent's refused
// connection read only as "did not prove its identity", which hid the cause.
func TestProveNamesWhyTheGateWasRefused(t *testing.T) {
	key := testKey(t)
	server := httptest.NewServer(identityHandler(key, http.NotFoundHandler()))
	defer server.Close()
	verifier := verifierFor(t, server, key.Public().(ed25519.PublicKey))
	verifier.Fetch = func(context.Context, string) (int, []byte, error) {
		return 0, nil, errors.New("dial tcp 192.168.1.64:3017: connect: no route to host")
	}
	err := verifier.Prove(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no route to host") {
		t.Fatalf("err = %v", err)
	}
	if verifier.Verified(context.Background()) {
		t.Fatal("verified")
	}
	if verifierFor(t, server, testKey(t).Public().(ed25519.PublicKey)).Prove(context.Background()) == nil {
		t.Fatal("a signature from another key proved the gate")
	}
}

func curlFor(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("no curl on this host")
	}
	return path
}

func TestCurlFetchProvesTheRealGateAndNothingElse(t *testing.T) {
	key := testKey(t)
	public := key.Public().(ed25519.PublicKey)
	real := httptest.NewServer(identityHandler(key, http.NotFoundHandler()))
	defer real.Close()
	verifier := verifierFor(t, real, public)
	verifier.Fetch = CurlFetch(curlFor(t))
	if err := verifier.Prove(context.Background()); err != nil {
		t.Fatalf("the real gate was not proven through curl: %v", err)
	}
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, real.URL+r.URL.RequestURI(), http.StatusFound)
	}))
	defer redirect.Close()
	followed := verifierFor(t, redirect, public)
	followed.Fetch = CurlFetch(curlFor(t))
	if followed.Verified(context.Background()) {
		t.Fatal("curl followed a redirect to the real gate")
	}
	gone := httptest.NewServer(http.NotFoundHandler())
	unreachable := verifierFor(t, gone, public)
	gone.Close()
	unreachable.Fetch = CurlFetch(curlFor(t))
	if err := unreachable.Prove(context.Background()); err == nil {
		t.Fatal("an unreachable gate was proven")
	}
}

func TestCurlFetchReadsTheStatusOfAnEmptyAnswer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	status, body, err := CurlFetch(curlFor(t))(context.Background(), server.URL)
	if err != nil || status != http.StatusNoContent || len(body) != 0 {
		t.Fatalf("status = %d, body = %q, err = %v", status, body, err)
	}
}
