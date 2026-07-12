package adminapi

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUnixClientReadsBoundedStatusAndMetrics(t *testing.T) {
	path := shortSocketPath(t)
	listener, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case StatusPath:
			_, _ = w.Write([]byte(`{"apiVersion":"fleet.v1","kind":"Status","data":{"controllerVersion":"v1","controllerMode":"shadow","hostMode":"linux","live":{"ok":true},"ready":{"ok":true},"queues":[],"instances":[],"observations":[],"operations":{"retrying":0,"dead":0}}}`))
		case MetricsPath:
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("fleet_mode 1\n"))
		default:
			http.NotFound(w, r)
		}
	})}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		<-done
	})

	client, err := NewClient("unix://"+path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	status, err := client.Status(context.Background())
	if err != nil || status.Data.ControllerMode != "shadow" || !status.Data.Ready.OK {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	metrics, err := client.Metrics(context.Background())
	if err != nil || metrics != "fleet_mode 1\n" {
		t.Fatalf("metrics=%q err=%v", metrics, err)
	}
	if _, err := client.get(context.Background(), "/missing", "application/json"); !errors.Is(err, ErrResponse) {
		t.Fatalf("missing error=%v", err)
	}
}

func TestClientRejectsUnsafeOrInvalidEndpoints(t *testing.T) {
	for _, endpoint := range []string{"", "%", "https://127.0.0.1:1", "http://192.0.2.1:1", "http://127.0.0.1/path", "unix://relative.sock", "unix:///tmp/x?query=1", "unix://host/tmp/x"} {
		if _, err := NewClient(endpoint, time.Second); err == nil {
			t.Fatalf("accepted %q", endpoint)
		}
	}
	if _, err := NewClient("unix:///tmp/x", 0); err == nil {
		t.Fatal("accepted zero timeout")
	}
	if _, err := NewClient("http://127.0.0.1:9876", time.Second); err != nil {
		t.Fatalf("loopback rejected: %v", err)
	}
}

func TestHTTPClientProbesAndValidatesWireContract(t *testing.T) {
	state := "ready"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case HealthPath:
			_, _ = w.Write([]byte(`{"status":"live"}`))
		case ReadyPath:
			if state == "broken" {
				_, _ = w.Write([]byte(`{`))
				return
			}
			if state == "not_ready" {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"status":"not_ready","reasons":["stale"]}`))
				return
			}
			_, _ = w.Write([]byte(`{"status":"ready"}`))
		case StatusPath:
			if state == "bad_status" {
				_, _ = w.Write([]byte(`{"apiVersion":"future","kind":"Status"}`))
				return
			}
			_, _ = w.Write([]byte(`{"apiVersion":"fleet.v1","kind":"Status","warnings":[],"data":{"queues":[],"instances":[],"observations":[]}}`))
		}
	}))
	defer server.Close()
	client, err := NewClient(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	live, err := client.Probe(context.Background(), false)
	if err != nil || !live.OK || live.Reasons == nil {
		t.Fatalf("live=%+v err=%v", live, err)
	}
	state = "not_ready"
	ready, err := client.Probe(context.Background(), true)
	if err != nil || ready.OK || len(ready.Reasons) != 1 {
		t.Fatalf("ready=%+v err=%v", ready, err)
	}
	state = "broken"
	if _, err := client.Probe(context.Background(), true); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("broken probe=%v", err)
	}
	state = "bad_status"
	if _, err := client.Status(context.Background()); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("bad status=%v", err)
	}
}

func TestClientRejectsWrongContentType(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(`{"apiVersion":"fleet.v1","kind":"Status"}`))
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, time.Second)
	if _, err := client.Status(context.Background()); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("content-type error=%v", err)
	}
}

func TestClientBoundsResponsesAndHonorsCancellation(t *testing.T) {
	path := shortSocketPath(t)
	listener, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == StatusPath {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(strings.Repeat("x", MaxResponseBytes+1)))
		}
	})}
	go server.Serve(listener) //nolint:errcheck -- closed by cleanup
	t.Cleanup(func() { _ = server.Close() })
	client, _ := NewClient("unix://"+path, time.Second)
	if _, err := client.Status(context.Background()); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("large response error=%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Status(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error=%v", err)
	}
}

func TestListenCreatesPrivateSocketAndRefusesFiles(t *testing.T) {
	dir := filepath.Dir(shortSocketPath(t))
	path := filepath.Join(dir, "run", "fleetd.sock")
	listener, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("socket mode=%v", info.Mode())
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket remains: %v", err)
	}
	file := filepath.Join(dir, "regular")
	if err := os.WriteFile(file, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(file); err == nil {
		t.Fatal("regular file replaced")
	}
	if got, _ := os.ReadFile(file); string(got) != "keep" {
		t.Fatalf("file changed: %q", got)
	}
}

func TestListenValidationAndStaleSocketRecovery(t *testing.T) {
	for _, path := range []string{"", "relative.sock", "/tmp/../tmp/fleet.sock", "/tmp/" + strings.Repeat("x", 101)} {
		if _, err := Listen(path); err == nil {
			t.Fatalf("accepted %q", path)
		}
	}
	path := shortSocketPath(t)
	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	if unix, ok := stale.(*net.UnixListener); ok {
		unix.SetUnlinkOnClose(false)
	}
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}
	listener, err := Listen(path)
	if err != nil {
		t.Fatalf("recover stale: %v", err)
	}
	_ = listener.Close()

	parentFile := filepath.Join(filepath.Dir(shortSocketPath(t)), "parent-file")
	if err := os.WriteFile(parentFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(filepath.Join(parentFile, "fleet.sock")); err == nil {
		t.Fatal("accepted file as parent directory")
	}
	insecure := filepath.Join(filepath.Dir(shortSocketPath(t)), "insecure")
	if err := os.Mkdir(insecure, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(insecure, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(filepath.Join(insecure, "fleet.sock")); err == nil {
		t.Fatal("accepted insecure socket directory")
	}
}

func TestDefaultEndpointIsAbsoluteUnixSocket(t *testing.T) {
	if path, endpoint := DefaultSocketPath(), DefaultEndpoint(); !filepath.IsAbs(path) || endpoint != "unix://"+path {
		t.Fatalf("path=%q endpoint=%q", path, endpoint)
	}
}

func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "trf-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "fleetd.sock")
}
