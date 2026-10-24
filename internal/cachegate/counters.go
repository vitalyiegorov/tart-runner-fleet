package cachegate

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
)

// Counters are the bytes the gate moved, by upstream and direction (ADR 0080).
// A guest's own receive counter includes what it read from the LAN cache, so
// `cache/to_guest` is what an operator subtracts from guest receive bytes to
// estimate the WAN share. They live for the process, like any Prometheus
// counter.
type Counters struct {
	toGuest, fromGuest [2]atomic.Int64
}

func (c *Counters) render() string {
	var output strings.Builder
	output.WriteString("# HELP fleet_cache_gate_bytes_total Bytes the cache gate moved, by upstream and direction.\n" +
		"# TYPE fleet_cache_gate_bytes_total counter\n")
	for _, upstream := range []struct {
		name  string
		index Upstream
	}{{"cache", CacheServer}, {"results", Results}} {
		fmt.Fprintf(&output, "fleet_cache_gate_bytes_total{upstream=%q,direction=\"to_guest\"} %d\n",
			upstream.name, c.toGuest[upstream.index].Load())
		fmt.Fprintf(&output, "fleet_cache_gate_bytes_total{upstream=%q,direction=\"from_guest\"} %d\n",
			upstream.name, c.fromGuest[upstream.index].Load())
	}
	return output.String()
}

// countedBody counts the request body bytes a guest sent through the gate.
type countedBody struct {
	io.ReadCloser
	counter *atomic.Int64
}

func (b countedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.counter.Add(int64(n))
	return n, err
}

// countedWriter counts the response bytes the gate sent a guest. Unwrap keeps
// http.ResponseController — which the reverse proxy flushes through — able to
// reach the connection.
type countedWriter struct {
	http.ResponseWriter
	counter *atomic.Int64
}

func (w countedWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.counter.Add(int64(n))
	return n, err
}

func (w countedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// metricsHandler serves the counters. It is mounted only on the loopback
// metrics listener, never on the address guests reach.
func metricsHandler(counters *Counters) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = io.WriteString(w, counters.render())
	})
	return mux
}

// loopbackAddress accepts only a literal loopback IP and an explicit port. An
// empty port would bind an ephemeral one nobody is told about, leaving the
// configured endpoint unscrapeable; `:0` stays available on purpose.
func loopbackAddress(address string) bool {
	host, port, err := net.SplitHostPort(address)
	ip := net.ParseIP(host)
	return err == nil && port != "" && ip != nil && ip.IsLoopback()
}
