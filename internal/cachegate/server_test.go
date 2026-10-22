package cachegate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func upstream(t *testing.T) (*url.URL, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path+" "+string(body))
		mu.Unlock()
		_, _ = io.WriteString(w, "upstream:"+r.URL.Path)
	}))
	t.Cleanup(server.Close)
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return target, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

// TestHandlerRefusesBeforeTheServerSeesTheRequest pins the deployment
// property: a refused save never reaches the cache server, and the refusal is
// a Twirp error the cache toolkit reports as a warning rather than a failure.
func TestHandlerRefusesBeforeTheServerSeesTheRequest(t *testing.T) {
	target, seen := upstream(t)
	var logs bytes.Buffer
	gate := httptest.NewServer(Handler(target, target, &logs))
	t.Cleanup(gate.Close)

	pr := token(t, []scope{{Scope: "refs/pull/9/merge", Permission: 3}, {Scope: "refs/heads/main", Permission: 1}})
	req, _ := http.NewRequest(http.MethodPost, gate.URL+create, strings.NewReader(`{"key":"k"}`))
	req.Header.Set("Authorization", pr)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var twirp struct{ Code, Msg string }
	if err := json.NewDecoder(resp.Body).Decode(&twirp); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden || twirp.Code != "permission_denied" || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("status=%d body=%+v", resp.StatusCode, twirp)
	}
	if len(seen()) != 0 {
		t.Fatalf("refused save reached the server: %v", seen())
	}
	if !strings.Contains(logs.String(), "refs/pull/9/merge") || strings.Contains(logs.String(), strings.TrimPrefix(pr, "Bearer ")) {
		t.Fatalf("log must name the scope and never the token: %q", logs.String())
	}
}

func TestHandlerProxiesWhatItAllows(t *testing.T) {
	target, seen := upstream(t)
	results, resultsSeen := upstream(t)
	gate := httptest.NewServer(Handler(target, results, io.Discard))
	t.Cleanup(gate.Close)

	req, _ := http.NewRequest(http.MethodPost, gate.URL+create, strings.NewReader(`{"key":"k"}`))
	req.Header.Set("Authorization", token(t, []scope{{Scope: "refs/heads/main", Permission: 3}}))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "upstream:"+create {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
	if got := seen(); len(got) != 1 || got[0] != "POST "+create+` {"key":"k"}` {
		t.Fatalf("seen=%v", got)
	}

	resp, err = http.Post(gate.URL+"/twirp/github.actions.results.api.v1.ArtifactService/CreateArtifact", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resultsSeen(); resp.StatusCode != http.StatusOK || len(got) != 1 || len(seen()) != 1 {
		t.Fatalf("artifact call: status=%d results=%v cache=%v", resp.StatusCode, got, seen())
	}

	resp, err = http.Get(gate.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := seen(); resp.StatusCode != http.StatusNotFound || len(got) != 1 {
		t.Fatalf("operator surface proxied: status=%d seen=%v", resp.StatusCode, got)
	}
}

func TestExecuteRejectsBadFlags(t *testing.T) {
	for name, args := range map[string][]string{
		"unknown flag":      {"--nope"},
		"missing upstream":  {"--listen", "127.0.0.1:0"},
		"relative upstream": {"--listen", "127.0.0.1:0", "--upstream", "localhost:3000"},
		"bad url":           {"--listen", "127.0.0.1:0", "--upstream", "http://[::1"},
		"relative results":  {"--listen", "127.0.0.1:0", "--upstream", "http://127.0.0.1:1", "--results-upstream", "results"},
		"positional":        {"--listen", "127.0.0.1:0", "--upstream", "http://127.0.0.1:1", "extra"},
	} {
		var stderr bytes.Buffer
		if code := Execute(context.Background(), args, io.Discard, &stderr); code != 2 {
			t.Fatalf("%s: code=%d stderr=%q", name, code, stderr.String())
		}
	}
}

func TestExecuteReportsAnUnusableListenAddress(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	var stderr bytes.Buffer
	code := Execute(context.Background(), []string{"--listen", busy.Addr().String(), "--upstream", "http://127.0.0.1:1"}, io.Discard, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "listen") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
}

// TestExecuteServesUntilCancelled pins the service lifecycle systemd relies
// on: the gate serves until SIGTERM cancels its context, then exits 0.
func TestExecuteServesUntilCancelled(t *testing.T) {
	target, _ := upstream(t)
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := probe.Addr().String()
	probe.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	var stdout bytes.Buffer
	go func() {
		done <- Execute(ctx, []string{"--listen", address, "--upstream", target.String(), "--results-upstream", target.String()}, &stdout, io.Discard)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get("http://" + address + "/health")
		if err == nil {
			resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("gate never served: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("code=%d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("gate did not stop")
	}
}

type brokenListener struct{ net.Listener }

func (brokenListener) Accept() (net.Conn, error) { return nil, errors.New("accept failed") }

// TestExecuteExitsWhenItCanNoLongerAccept pins that a gate whose listener dies
// exits non-zero for systemd to restart, rather than idling without serving.
func TestExecuteExitsWhenItCanNoLongerAccept(t *testing.T) {
	original := listenTCP
	t.Cleanup(func() { listenTCP = original })
	listenTCP = func(network, address string) (net.Listener, error) {
		inner, err := original(network, address)
		return brokenListener{inner}, err
	}
	var stderr bytes.Buffer
	code := Execute(context.Background(), []string{"--listen", "127.0.0.1:0", "--upstream", "http://127.0.0.1:1"}, io.Discard, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "accept failed") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
}

// TestResultsCallsCarryGitHubsHost pins that a call forwarded to GitHub names
// GitHub's host, not the gate's: the runner addressed the gate, and GitHub's
// front end does not route a foreign Host header.
func TestResultsCallsCarryGitHubsHost(t *testing.T) {
	hosts := make(chan string, 1)
	results := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hosts <- r.Host }))
	t.Cleanup(results.Close)
	target, _ := url.Parse(results.URL)
	cache, _ := upstream(t)
	gate := httptest.NewServer(Handler(cache, target, io.Discard))
	t.Cleanup(gate.Close)
	resp, err := http.Get(gate.URL + "/twirp/github.actions.results.api.v1.ArtifactService/ListArtifacts")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := <-hosts; got != target.Host {
		t.Fatalf("host=%q want %q", got, target.Host)
	}
}
