package cachegate

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// The replay models jobs already placed on different sites. No placement
// preference is assumed: every gate reaches one owner and stores no bytes.
func TestArtifactIssue509SplitSiteReplay(t *testing.T) {
	f := newArtifactFixture(t)
	owner := httptest.NewServer(f.handler)
	defer owner.Close()
	target, _ := url.Parse(owner.URL)
	gates := []http.Handler{artifactForwarder(target), artifactForwarder(target)}
	original := f.handler
	f.handler = gates[0]
	f.upload(f.token("123", "run", "build"), "run", "build", "next-build", []byte("identical-build"))
	for lane := range 7 {
		f.handler = gates[lane%2]
		consumer := f.token("123", "run", "lane")
		list := f.rpc(consumer, "ListArtifacts", artifactBody("run", "lane", ""))
		if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), "next-build") {
			t.Fatal("cross-site lane lost build")
		}
		signed := f.rpc(consumer, "GetSignedArtifactURL", artifactBody("run", "build", "next-build"))
		var res struct {
			URL string `json:"signed_url"`
		}
		json.Unmarshal(signed.Body.Bytes(), &res)
		if got := f.blob(res.URL, http.MethodGet, nil); got.Body.String() != "identical-build" {
			t.Fatal("cross-site bytes changed")
		}
		if f.rpc(f.token("123", "other-run", "lane"), "GetSignedArtifactURL", artifactBody("run", "build", "next-build")).Code != http.StatusForbidden {
			t.Fatal("PR read another run")
		}
	}
	owner.Close()
	if f.rpc(f.token("123", "run", "lane"), "ListArtifacts", artifactBody("run", "lane", "")).Code != http.StatusServiceUnavailable {
		t.Fatal("owner outage became miss/GitHub fallback")
	}
	f.handler = original
}

func TestArtifactMaintenanceStopsAndPrunesIdleStore(t *testing.T) {
	f := newArtifactFixture(t)
	f.create(f.token("123", "r", "j"), "r", "j", "idle")
	f.at = f.at.Add(2 * time.Hour)
	old := artifactSweepInterval
	artifactSweepInterval = time.Millisecond
	defer func() { artifactSweepInterval = old }()
	stop := artifactMaintenance(f.store)
	deadline := time.Now().Add(time.Second)
	for {
		f.store.mu.Lock()
		_, err := f.store.named(artifactPrincipal{repo: "123", run: "r"}, "idle")
		f.store.mu.Unlock()
		if err != nil {
			break
		}
		if time.Now().After(deadline) {
			stop()
			t.Fatal("idle sweep never fired")
		}
		time.Sleep(time.Millisecond)
	}
	stop()
	f.budget.probe = func() (int64, error) { return 0, nil }
	f.store.prune() // low disk never authorizes maintenance writes
}

func TestArtifactFullExecuteOwnerAndForwardingLifecycle(t *testing.T) {
	for _, mode := range []string{"owner", "forward"} {
		t.Run(mode, func(t *testing.T) {
			cache, _ := upstream(t)
			dir := t.TempDir()
			os.Chmod(dir, 0700)
			listen := freeAddress(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			args := []string{"--listen", listen, "--upstream", cache.String(), "--results-upstream", cache.String(), "--write-budget-path", dir, "--write-min-free-bytes", "1", "--artifacts-max-bytes", "1048576"}
			if mode == "owner" {
				args = append(args, "--artifacts-dir", dir, "--artifacts-repositories", "123")
			} else {
				args = append(args, "--artifacts-upstream", cache.String())
			}
			done := make(chan int, 1)
			go func() { done <- Execute(ctx, args, io.Discard, io.Discard) }()
			waitFor(t, "http://"+listen+"/health")
			// A malformed runtime token is rejected without any external JWKS fetch.
			res, err := http.Post("http://"+listen+artifactService+"CreateArtifact", "application/json", strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if mode == "owner" && res.StatusCode != http.StatusForbidden {
				t.Fatal("owner not installed")
			}
			cancel()
			if code := <-done; code != 0 {
				t.Fatalf("exit %d", code)
			}
		})
	}
}
