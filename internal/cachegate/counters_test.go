package cachegate

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestCountersSeparateLANCacheBytesFromResultsBytes is what lets an operator
// subtract the LAN share from a guest's receive counter (ADR 0080): bytes the
// cache server sent a guest, and bytes a guest sent it, apart from the results
// calls the gate forwards to GitHub.
func TestCountersSeparateLANCacheBytesFromResultsBytes(t *testing.T) {
	cache, _ := upstream(t)
	results, _ := upstream(t)
	counters := &Counters{}
	gate := httptest.NewServer(countingHandler(cache, results, io.Discard, counters))
	t.Cleanup(gate.Close)
	for _, request := range []struct{ path, body string }{
		{"/download/archive", ""},
		{"/upload/archive", "0123456789"},
		{"/twirp/results.services.receiver.Receiver/X", "abc"},
		{create, "{}"}, // refused without a token: never counted
	} {
		response, err := http.Post(gate.URL+request.path, "application/octet-stream", strings.NewReader(request.body))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}
	metrics := counters.render()
	for _, want := range []string{
		`fleet_cache_gate_bytes_total{upstream="cache",direction="to_guest"} ` +
			strconv.Itoa(len("upstream:/download/archive")+len("upstream:/upload/archive")),
		`fleet_cache_gate_bytes_total{upstream="cache",direction="from_guest"} 10`,
		`fleet_cache_gate_bytes_total{upstream="results",direction="to_guest"} ` +
			strconv.Itoa(len("upstream:/twirp/results.services.receiver.Receiver/X")),
		`fleet_cache_gate_bytes_total{upstream="results",direction="from_guest"} 3`,
	} {
		if !strings.Contains(metrics, want) {
			t.Fatalf("metrics missing %q:\n%s", want, metrics)
		}
	}
}

// TestExecuteServesMetricsOnLoopbackOnly keeps the counters off the address
// guests reach: the gate's own listener answers /metrics with a 404, and the
// metrics listener refuses to bind anywhere but loopback.
func TestExecuteServesMetricsOnLoopbackOnly(t *testing.T) {
	target, _ := upstream(t)
	gateAddress, metricsAddress := freeAddress(t), freeAddress(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- Execute(ctx, []string{"--listen", gateAddress, "--upstream", target.String(), "--results-upstream",
			target.String(), "--metrics-listen", metricsAddress}, io.Discard, io.Discard)
	}()
	body := waitFor(t, "http://"+metricsAddress+"/metrics")
	if !strings.Contains(body, "fleet_cache_gate_bytes_total") {
		t.Fatalf("metrics = %q", body)
	}
	response, err := http.Get("http://" + gateAddress + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("the guest-facing listener must not serve metrics: %d", response.StatusCode)
	}
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("code=%d", code)
	}
	for _, address := range []string{"0.0.0.0:0", "gate.local:0", "nonsense"} {
		var stderr bytes.Buffer
		if code := Execute(context.Background(), []string{"--listen", "127.0.0.1:0", "--upstream", target.String(),
			"--metrics-listen", address}, io.Discard, &stderr); code != 2 {
			t.Fatalf("%s: code=%d stderr=%q", address, code, stderr.String())
		}
	}
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	if code := Execute(context.Background(), []string{"--listen", "127.0.0.1:0", "--upstream", target.String(),
		"--metrics-listen", busy.Addr().String()}, io.Discard, io.Discard); code != 1 {
		t.Fatalf("an unusable metrics address must exit 1, got %d", code)
	}
}

func freeAddress(t *testing.T) string {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	return probe.Addr().String()
}

func waitFor(t *testing.T, url string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		response, err := http.Get(url)
		if err == nil {
			body, _ := io.ReadAll(response.Body)
			_ = response.Body.Close()
			return string(body)
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never served: %v", url, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestCountedWriterUnwrapsForTheResponseController(t *testing.T) {
	recorder := httptest.NewRecorder()
	var counter atomic.Int64
	if err := http.NewResponseController(countedWriter{recorder, &counter}).Flush(); err != nil || !recorder.Flushed {
		t.Fatalf("flush through the counted writer: %v flushed=%t", err, recorder.Flushed)
	}
}
