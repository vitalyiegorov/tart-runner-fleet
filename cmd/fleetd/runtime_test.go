package main

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/credentials"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/reconcile"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/telemetry"
)

type runtimeInventory struct{}

func (runtimeInventory) Observe(context.Context) (domain.Observation[[]domain.Instance], domain.Observation[domain.Host]) {
	now := time.Now().UTC()
	return domain.Fresh([]domain.Instance(nil), now), domain.Fresh(domain.Host{Available: domain.Resources{CPU: 8, MemoryMB: 16000, Slots: 4}}, now)
}

type notifyingInventory struct {
	ready chan struct{}
	once  *sync.Once
}

func (i notifyingInventory) Observe(ctx context.Context) (domain.Observation[[]domain.Instance], domain.Observation[domain.Host]) {
	i.once.Do(func() { close(i.ready) })
	return runtimeInventory{}.Observe(ctx)
}

type keyRunner struct {
	out []byte
	err error
}

func (r keyRunner) Run(context.Context, string, ...string) ([]byte, error) { return r.out, r.err }

type fakeSource struct{ closed atomic.Int32 }

func (*fakeSource) Handle(ctx context.Context, _ func(context.Context, githubscaleset.Demand) error) error {
	<-ctx.Done()
	return ctx.Err()
}
func (f *fakeSource) Close(context.Context) error { f.closed.Add(1); return nil }

type fakeListener struct {
	done chan struct{}
	once sync.Once
}

type failingListener struct{}

func (failingListener) Accept() (net.Conn, error) { return nil, errors.New("accept") }
func (failingListener) Close() error              { return nil }
func (failingListener) Addr() net.Addr {
	return &net.UnixAddr{Name: "/private/tmp/failing.sock", Net: "unix"}
}

func newFakeListener() *fakeListener              { return &fakeListener{done: make(chan struct{})} }
func (f *fakeListener) Accept() (net.Conn, error) { <-f.done; return nil, net.ErrClosed }
func (f *fakeListener) Close() error              { f.once.Do(func() { close(f.done) }); return nil }
func (*fakeListener) Addr() net.Addr {
	return &net.UnixAddr{Name: "/private/tmp/trf-test.sock", Net: "unix"}
}

type failingCloseReader struct{ io.Reader }

func (failingCloseReader) Close() error { return errors.New("close") }

func writeConfig(t *testing.T, shadow bool) string {
	t.Helper()
	github := ""
	if shadow {
		github = `,"github":{"configUrl":"https://github.com/owner","owner":"owner","clientId":"client","installationId":1,"keychainService":"fleet","keychainAccount":"app","scaleSets":[{"profile":"small","id":1,"maxCapacity":4},{"profile":"medium","id":2,"maxCapacity":4},{"profile":"large","id":3,"maxCapacity":2},{"profile":"builder","id":4,"maxCapacity":1},{"profile":"maestro","id":5,"maxCapacity":2}]}`
	}
	raw := `{"baseVm":"linux","vmPrefix":"gha","pollSeconds":1,"maxLinuxWhenMacosIdle":4,"maxLinuxCpu":8,"maxLinuxMemoryMb":16384,"linuxReservationAgeSeconds":300,"minFreeDiskGb":1,"linuxProfiles":[{"id":"small","label":"linux-small","cpu":1,"memoryMb":2048},{"id":"medium","label":"linux-medium","cpu":2,"memoryMb":4096},{"id":"large","label":"linux-large","cpu":4,"memoryMb":8192}],"macosBurst":{"enabled":true,"baseVm":"mac","vmPrefix":"gha-mac","builder":{"id":"builder","label":"macos-builder","cpu":8,"memoryMb":12288,"maxActive":1},"maestro":{"id":"maestro","label":"macos-maestro","cpu":4,"memoryMb":7168,"maxActive":2}},"targets":[{"type":"repo","slug":"owner/repo","maxActive":4}]` + github + `}`
	path := filepath.Join(t.TempDir(), "fleet.json")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func testDependencies(t *testing.T) dependencies {
	t.Helper()
	d := defaultDependencies()
	d.inventory = func(runtimeStore, config.Config) app.Inventory { return runtimeInventory{} }
	d.listen = func(_, _ string) (net.Listener, error) { return newFakeListener(), nil }
	d.adminListen = func(string) (net.Listener, error) { return newFakeListener(), nil }
	d.loadKey = func(ctx context.Context, _, _ string) (*credentials.Secret, error) {
		return (credentials.Keychain{Runner: keyRunner{out: []byte("key")}}).Load(ctx, "s", "a")
	}
	return d
}

func TestRunObserveAndShadow(t *testing.T) {
	for _, mode := range []reconcile.Mode{reconcile.Observe, reconcile.Shadow} {
		t.Run(string(mode), func(t *testing.T) {
			d := testDependencies(t)
			ready := make(chan struct{})
			var readyOnce sync.Once
			d.inventory = func(runtimeStore, config.Config) app.Inventory {
				return notifyingInventory{ready: ready, once: &readyOnce}
			}
			var sources []*fakeSource
			d.newScaleSet = func(context.Context, githubscaleset.GitHubAppScaleSetConfig) (scaleSetSource, error) {
				source := &fakeSource{}
				sources = append(sources, source)
				return source, nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			opts := options{ConfigPath: writeConfig(t, mode == reconcile.Shadow), DatabasePath: filepath.Join(t.TempDir(), "fleet.db"), HealthAddress: "127.0.0.1:0", Mode: mode}
			done := make(chan error, 1)
			go func() { done <- runWithDependencies(ctx, opts, d) }()
			select {
			case <-ready:
				cancel()
			case <-time.After(5 * time.Second):
				cancel()
				t.Fatal("runtime did not become ready")
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("runtime did not stop after cancellation")
			}
			if mode == reconcile.Shadow {
				if len(sources) != 5 {
					t.Fatalf("sources=%d", len(sources))
				}
				for _, source := range sources {
					if source.closed.Load() != 1 {
						t.Fatal("source not closed")
					}
				}
			}
		})
	}
}

func TestRunValidationAndDependencyErrors(t *testing.T) {
	valid := writeConfig(t, false)
	shadow := writeConfig(t, true)
	want := errors.New("down")
	tests := []struct {
		name   string
		opts   options
		mutate func(*dependencies)
	}{
		{name: "mode", opts: options{Mode: reconcile.Authority}},
		{name: "open config", opts: options{Mode: reconcile.Observe, ConfigPath: "missing"}},
		{name: "decode", opts: options{Mode: reconcile.Observe, ConfigPath: filepath.Join(t.TempDir(), "bad")}, mutate: func(*dependencies) {}},
		{name: "shadow config", opts: options{Mode: reconcile.Shadow, ConfigPath: valid}},
		{name: "open store", opts: options{Mode: reconcile.Observe, ConfigPath: valid}, mutate: func(d *dependencies) {
			d.openStore = func(context.Context, string) (runtimeStore, error) { return nil, want }
		}},
		{name: "listen", opts: options{Mode: reconcile.Observe, ConfigPath: valid, DatabasePath: filepath.Join(t.TempDir(), "x.db")}, mutate: func(d *dependencies) { d.listen = func(string, string) (net.Listener, error) { return nil, want } }},
		{name: "admin listen", opts: options{Mode: reconcile.Observe, ConfigPath: valid, DatabasePath: filepath.Join(t.TempDir(), "x.db")}, mutate: func(d *dependencies) { d.adminListen = func(string) (net.Listener, error) { return nil, want } }},
		{name: "key", opts: options{Mode: reconcile.Shadow, ConfigPath: shadow, DatabasePath: filepath.Join(t.TempDir(), "x.db")}, mutate: func(d *dependencies) {
			d.loadKey = func(context.Context, string, string) (*credentials.Secret, error) { return nil, want }
		}},
		{name: "scale", opts: options{Mode: reconcile.Shadow, ConfigPath: shadow, DatabasePath: filepath.Join(t.TempDir(), "x.db")}, mutate: func(d *dependencies) {
			d.newScaleSet = func(context.Context, githubscaleset.GitHubAppScaleSetConfig) (scaleSetSource, error) {
				return nil, want
			}
		}},
		{name: "cursor", opts: options{Mode: reconcile.Shadow, ConfigPath: shadow, DatabasePath: filepath.Join(t.TempDir(), "x.db")}, mutate: func(d *dependencies) {
			d.cursor = func(context.Context, runtimeStore, int64) (int64, error) { return 0, want }
		}},
		{name: "close config", opts: options{Mode: reconcile.Observe, ConfigPath: valid}, mutate: func(d *dependencies) {
			data, _ := os.ReadFile(valid)
			d.openConfig = func(string) (io.ReadCloser, error) {
				return failingCloseReader{Reader: strings.NewReader(string(data))}, nil
			}
		}},
	}
	if err := os.WriteFile(tests[2].opts.ConfigPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := testDependencies(t)
			if tt.mutate != nil {
				tt.mutate(&d)
			}
			if err := runWithDependencies(context.Background(), tt.opts, d); err == nil {
				t.Fatal("unexpected success")
			}
		})
	}
}

func TestRunDaemonAndInvalidBinding(t *testing.T) {
	if err := runDaemon(context.Background(), options{Mode: reconcile.Authority}); err == nil {
		t.Fatal("authority armed")
	}
	path := writeConfig(t, false)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	invalid := strings.TrimSuffix(string(data), "}") + `,"github":{"scaleSets":[{"profile":"missing","id":1}]}}`
	if err := os.WriteFile(path, []byte(invalid), 0o600); err != nil {
		t.Fatal(err)
	}
	d := testDependencies(t)
	if err := runWithDependencies(context.Background(), options{Mode: reconcile.Observe, ConfigPath: path, DatabasePath: filepath.Join(t.TempDir(), "x.db")}, d); err == nil {
		t.Fatal("invalid binding accepted")
	}
}

func TestRunCleansPartialScaleSetsAndDetectsHealthDeath(t *testing.T) {
	d := testDependencies(t)
	var first *fakeSource
	calls := 0
	d.newScaleSet = func(context.Context, githubscaleset.GitHubAppScaleSetConfig) (scaleSetSource, error) {
		calls++
		if calls == 1 {
			first = &fakeSource{}
			return first, nil
		}
		return nil, errors.New("open")
	}
	opts := options{Mode: reconcile.Shadow, ConfigPath: writeConfig(t, true), DatabasePath: filepath.Join(t.TempDir(), "x.db")}
	if err := runWithDependencies(context.Background(), opts, d); err == nil || first.closed.Load() != 1 {
		t.Fatalf("err=%v closed=%d", err, first.closed.Load())
	}

	d = testDependencies(t)
	d.listen = func(string, string) (net.Listener, error) { return failingListener{}, nil }
	opts = options{Mode: reconcile.Observe, ConfigPath: writeConfig(t, false), DatabasePath: filepath.Join(t.TempDir(), "y.db")}
	if err := runWithDependencies(context.Background(), opts, d); err == nil || !strings.Contains(err.Error(), "health server") {
		t.Fatalf("err=%v", err)
	}
	if serverFailure(nil) == nil || serverFailure(errors.New("x")) == nil {
		t.Fatal("server failures lost")
	}
}

func TestDefaultDependenciesAndHelpers(t *testing.T) {
	d := defaultDependencies()
	ctx := context.Background()
	store, err := d.openStore(ctx, filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if d.inventory(store, config.Default()) == nil {
		t.Fatal("nil inventory")
	}
	if _, err := d.loadKey(ctx, "tart-runner-fleet-test-missing", "none"); err == nil {
		t.Fatal("missing key found")
	}
	if _, err := d.newScaleSet(ctx, githubscaleset.GitHubAppScaleSetConfig{}); err == nil {
		t.Fatal("invalid scale set opened")
	}
	if (wallClock{}).Now().IsZero() {
		t.Fatal("wall clock")
	}
	if err := (boundIngester{}).Ingest(ctx); err == nil {
		t.Fatal("invalid ingester")
	}
}

func TestEngineTickerRecordsFailureWithoutReflectingIt(t *testing.T) {
	// A nil engine store forces a bounded error; telemetry receives only state.
	health, _ := telemetryHealthForTest()
	err := (engineTicker{engine: app.Engine{}, health: health}).Tick(context.Background())
	if err == nil || health.Snapshot().Observations["scheduler"].Freshness == "fresh" {
		t.Fatalf("err=%v snapshot=%#v", err, health.Snapshot())
	}
}

func TestEngineTickerRecordsBlockedAsStale(t *testing.T) {
	d := testDependencies(t)
	store, err := d.openStore(context.Background(), filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	stale := staleInventory{now: now}
	engine := app.Engine{Store: store, Inventory: stale, Config: app.BuildSchedulerConfig(config.Default()), ControllerID: "c", Mode: reconcile.Observe, Now: func() time.Time { return now }}
	health, _ := telemetryHealthForTest()
	if err := (engineTicker{engine: engine, health: health}).Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if health.Snapshot().Observations["scheduler"].Freshness != telemetry.ObservationStale {
		t.Fatalf("snapshot=%#v", health.Snapshot())
	}
}

func TestEngineTickerRecordsBoundedMetricsAndModes(t *testing.T) {
	health, err := telemetry.NewHealth(wallClock{}, telemetry.HealthConfig{Profiles: []string{"small", "maestro"}, CriticalObservations: []string{"scheduler"}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	ticker := engineTicker{health: health, profiles: []string{"small", "maestro"}}
	ticker.recordMetrics(app.TickResult{HostMode: domain.HostLinux, Demands: []domain.Demand{
		{Profile: "small", CreatedAt: now.Add(-time.Minute)}, {Profile: "small", CreatedAt: now.Add(-2 * time.Minute)},
	}, Instances: []domain.Instance{
		{Profile: "small", State: domain.InstanceRunning, Resources: domain.Resources{CPU: 2, MemoryMB: 4096}},
		{Profile: "maestro", State: domain.InstanceDeleted, Resources: domain.Resources{CPU: 4, MemoryMB: 7168}},
	}})
	snapshot := health.Snapshot()
	if snapshot.Mode != telemetry.ModeLinux || snapshot.Queues["small"].Count != 2 || snapshot.Queues["small"].OldestEnqueuedAt != now.Add(-2*time.Minute) || snapshot.Instances["small"].CPU != 2 || snapshot.Instances["maestro"].Count != 0 {
		t.Fatalf("snapshot=%#v", snapshot)
	}
	ticker.recordMetrics(app.TickResult{HostMode: domain.HostMacOS})
	if health.Snapshot().Mode != telemetry.ModeMacOS {
		t.Fatalf("mode=%s", health.Snapshot().Mode)
	}
}

type staleInventory struct{ now time.Time }

func (s staleInventory) Observe(context.Context) (domain.Observation[[]domain.Instance], domain.Observation[domain.Host]) {
	return domain.Fresh([]domain.Instance(nil), s.now), domain.Stale(domain.Host{}, s.now, "stale")
}

func telemetryHealthForTest() (*telemetry.Health, error) {
	return telemetry.NewHealth(wallClock{}, telemetry.HealthConfig{CriticalObservations: []string{"scheduler"}})
}
