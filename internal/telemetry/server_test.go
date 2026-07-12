package telemetry

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestServerEndpointsAndBoundedMetrics(t *testing.T) {
	health, clock := newTestHealth(t)
	health.RecordTick(true)
	for _, name := range []string{"github", "host", "tart"} {
		if err := health.RecordObservation(name, ObservationFresh); err != nil {
			t.Fatal(err)
		}
	}
	_ = health.SetMode(ModeLinux)
	_ = health.SetQueue("linux-small", 2, clock.Now().Add(-9*time.Second))
	_ = health.SetInstances("linux-small", 2, 4, 8192)
	health.SetOperations(3, 1)

	server, err := NewServer(health, ServerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	assertResponse(t, server, http.MethodGet, "/healthz", http.StatusOK, "live")
	assertResponse(t, server, http.MethodGet, "/readyz", http.StatusOK, "ready")
	response := request(t, server, http.MethodGet, "/metrics")
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	metrics := string(body)
	for _, want := range []string{
		`fleet_queue_jobs{profile="linux-small"} 2`,
		`fleet_queue_oldest_age_seconds{profile="linux-small"} 9`,
		`fleet_instances{profile="linux-small"} 2`,
		`fleet_instance_cpu{profile="linux-small"} 4`,
		`fleet_instance_memory_mib{profile="linux-small"} 8192`,
		`fleet_operation_retries 3`, `fleet_operations_dead 1`,
		`fleet_observation_fresh{observation="github"} 1`,
		`fleet_mode{mode="linux"} 1`,
		`fleet_last_successful_tick_timestamp_seconds 1700000000`,
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("metrics missing %q:\n%s", want, metrics)
		}
	}
	if strings.Contains(metrics, "repo=") || strings.Contains(metrics, "job=") {
		t.Fatalf("unbounded labels in metrics: %s", metrics)
	}
	if got := response.Header.Get("Content-Type"); got != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("content-type=%q", got)
	}
	if server.httpServer.ReadTimeout <= 0 || server.httpServer.WriteTimeout <= 0 || server.httpServer.IdleTimeout <= 0 || server.httpServer.ReadHeaderTimeout <= 0 {
		t.Fatal("server timeouts are not bounded")
	}
}

func TestServerUnhealthyMethodAndUnknownPath(t *testing.T) {
	health, _ := newTestHealth(t)
	server, err := NewServer(health, ServerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	assertResponse(t, server, http.MethodGet, "/readyz", http.StatusServiceUnavailable, "not_ready")

	response := request(t, server, http.MethodPost, "/healthz")
	response.Body.Close()
	if response.StatusCode != http.StatusMethodNotAllowed || response.Header.Get("Allow") != http.MethodGet {
		t.Fatalf("method response=%d allow=%q", response.StatusCode, response.Header.Get("Allow"))
	}
	assertResponse(t, server, http.MethodGet, "/missing", http.StatusNotFound, "404")
}

func TestServerGracefulShutdownAndServeErrors(t *testing.T) {
	health, _ := newTestHealth(t)
	server, err := NewServer(health, ServerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	listener := newBlockingListener()
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	<-listener.accepting

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Serve after shutdown=%v", err)
	}
	if err := server.Serve(listener); err == nil || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("unsafe/absent serve error=%v", err)
	}
	if err := server.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestServerSanitizesListenerFailureAndRejectsNonLocalTCP(t *testing.T) {
	health, _ := newTestHealth(t)
	server, err := NewServer(health, ServerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	secret := "ghp_do-not-leak"
	err = server.Serve(&errorListener{err: errors.New(secret), address: testAddr("local")})
	if !errors.Is(err, errServerFailed) || strings.Contains(err.Error(), secret) {
		t.Fatalf("unsafe error=%v", err)
	}

	server, _ = NewServer(health, ServerConfig{})
	err = server.Serve(&errorListener{err: net.ErrClosed, address: &net.TCPAddr{IP: net.IPv4zero, Port: 8080}})
	if !errors.Is(err, errUnsafeListener) {
		t.Fatalf("non-local listener error=%v", err)
	}
}

func TestMetricsMethodAndEmptyStateRendering(t *testing.T) {
	health, clock := newTestHealth(t)
	server, err := NewServer(health, ServerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	response := request(t, server, http.MethodPost, "/metrics")
	response.Body.Close()
	if response.StatusCode != http.StatusMethodNotAllowed || response.Header.Get("Allow") != http.MethodGet {
		t.Fatalf("metrics method response=%d allow=%q", response.StatusCode, response.Header.Get("Allow"))
	}

	_ = health.SetQueue("builder", 0, time.Time{})
	_ = health.SetInstances("builder", 0, 0, 0)
	metrics := renderMetrics(health.Snapshot())
	for _, want := range []string{
		`fleet_queue_oldest_age_seconds{profile="builder"} 0`,
		`fleet_observation_age_seconds{observation="github"} 0`,
		`fleet_mode{mode="idle"} 1`,
		`fleet_last_successful_tick_timestamp_seconds 0`,
	} {
		if !strings.Contains(metrics, want) {
			t.Fatalf("missing %q:\n%s", want, metrics)
		}
	}
	if err := health.RecordObservation("github", ObservationFresh); err != nil {
		t.Fatal(err)
	}
	clock.Advance(21 * time.Second)
	if metrics := renderMetrics(health.Snapshot()); !strings.Contains(metrics, `fleet_observation_fresh{observation="github"} 0`) {
		t.Fatalf("expired observation reported fresh:\n%s", metrics)
	}
}

func TestServerRejectsUnsafeConfiguration(t *testing.T) {
	health, _ := newTestHealth(t)
	if _, err := NewServer(nil, ServerConfig{}); err == nil {
		t.Fatal("nil health accepted")
	}
	for _, cfg := range []ServerConfig{
		{ReadTimeout: -1},
		{ReadTimeout: 31 * time.Second},
		{WriteTimeout: 31 * time.Second},
		{IdleTimeout: 31 * time.Second},
		{ReadHeaderTimeout: 31 * time.Second},
		{MaxHeaderBytes: -1},
		{MaxHeaderBytes: 2 << 20},
	} {
		if _, err := NewServer(health, cfg); err == nil {
			t.Fatalf("accepted %+v", cfg)
		}
	}
}

func request(t *testing.T, server *Server, method, path string) *http.Response {
	t.Helper()
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(method, "http://localhost"+path, nil)
	server.httpServer.Handler.ServeHTTP(recorder, req)
	return recorder.Result()
}

func assertResponse(t *testing.T, server *Server, method, path string, status int, containsText string) {
	t.Helper()
	response := request(t, server, method, path)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != status || !strings.Contains(string(body), containsText) {
		t.Fatalf("%s %s = %d %q", method, path, response.StatusCode, body)
	}
}

type blockingListener struct {
	accepting chan struct{}
	closed    chan struct{}
	once      sync.Once
}

func newBlockingListener() *blockingListener {
	return &blockingListener{accepting: make(chan struct{}), closed: make(chan struct{})}
}

func (l *blockingListener) Accept() (net.Conn, error) {
	l.once.Do(func() { close(l.accepting) })
	<-l.closed
	return nil, net.ErrClosed
}

func (l *blockingListener) Close() error {
	select {
	case <-l.closed:
	default:
		close(l.closed)
	}
	return nil
}

func (l *blockingListener) Addr() net.Addr { return testAddr("local") }

type testAddr string

func (a testAddr) Network() string { return string(a) }
func (a testAddr) String() string  { return string(a) }

type errorListener struct {
	err     error
	address net.Addr
}

func (l *errorListener) Accept() (net.Conn, error) { return nil, l.err }
func (l *errorListener) Close() error              { return nil }
func (l *errorListener) Addr() net.Addr            { return l.address }
