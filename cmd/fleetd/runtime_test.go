package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/tart"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/credentials"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/lifecycle"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
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
func (f *fakeSource) Close(context.Context) error                    { f.closed.Add(1); return nil }
func (*fakeSource) Registered(context.Context, string) (bool, error) { return true, nil }
func (*fakeSource) AcquireAndGenerateJIT(context.Context, int64, string, string) (*githubscaleset.JITSecret, error) {
	return githubscaleset.NewJITSecret("jit"), nil
}
func (*fakeSource) Deregister(context.Context, string) error { return nil }

type brokenSessionSource struct {
	fakeSource
	handled chan<- struct{}
}

func (s *brokenSessionSource) Handle(context.Context, func(context.Context, githubscaleset.Demand) error) error {
	select {
	case s.handled <- struct{}{}:
	default:
	}
	return errors.New("terminal broker session")
}

type blockingCloseSource struct {
	fakeSource
	started chan<- struct{}
	release <-chan struct{}
}

func (s *blockingCloseSource) Close(ctx context.Context) error {
	s.started <- struct{}{}
	select {
	case <-s.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type contextIgnoringCloseSource struct {
	fakeSource
	started chan<- struct{}
	release <-chan struct{}
}

type failingCloseSource struct {
	fakeSource
	err error
}

func (s *failingCloseSource) Close(context.Context) error {
	s.closed.Add(1)
	return s.err
}

func (s *contextIgnoringCloseSource) Close(context.Context) error {
	s.started <- struct{}{}
	<-s.release
	return nil
}

type trackedCloseSource struct {
	fakeSource
	active  *atomic.Int32
	maximum *atomic.Int32
	started chan<- struct{}
	release <-chan struct{}
	err     error
}

func (s *trackedCloseSource) Close(ctx context.Context) error {
	active := s.active.Add(1)
	defer s.active.Add(-1)
	for {
		maximum := s.maximum.Load()
		if active <= maximum || s.maximum.CompareAndSwap(maximum, active) {
			break
		}
	}
	s.started <- struct{}{}
	select {
	case <-s.release:
		return s.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestScaleSetSourcesCloseConcurrentlyWithinShutdownBudget(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	sources := []scaleSetSource{
		&blockingCloseSource{started: started, release: release},
		&blockingCloseSource{started: started, release: release},
	}
	done := make(chan struct{})
	go func() {
		closeScaleSetSources(context.Background(), sources)
		close(done)
	}()

	for range 2 {
		select {
		case <-started:
		case <-time.After(100 * time.Millisecond):
			t.Fatal("scale-set session cleanup ran sequentially")
		}
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scale-set session cleanup did not complete")
	}
}

func TestScaleSetSourcesReturnWhenCloseIgnoresCancellation(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- closeScaleSetSources(ctx, []scaleSetSource{
			&contextIgnoringCloseSource{started: started, release: release},
		})
	}()

	select {
	case <-started:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("scale-set session cleanup did not start")
	}
	select {
	case err := <-done:
		if !errors.Is(err, errScaleSetClose) {
			t.Fatalf("cleanup error=%v", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("scale-set session cleanup exceeded its context budget")
	}
	close(release)
}

// Regression: closing every Actions scale-set session at once can overload the
// broker and leave every session active even though fleetd exits on deadline.
// Cleanup must use a small bounded worker set and report incomplete deletion.
func TestScaleSetSourcesBoundConcurrencyAndReportFailures(t *testing.T) {
	const sourceCount = 6
	var active atomic.Int32
	var maximum atomic.Int32
	started := make(chan struct{}, sourceCount)
	release := make(chan struct{})
	sources := make([]scaleSetSource, 0, sourceCount)
	for range sourceCount {
		sources = append(sources, &trackedCloseSource{
			active: &active, maximum: &maximum, started: started, release: release,
		})
	}
	done := make(chan error, 1)
	go func() { done <- closeScaleSetSources(context.Background(), sources) }()

	for range scaleSetCloseConcurrency {
		select {
		case <-started:
		case <-time.After(100 * time.Millisecond):
			t.Fatal("bounded scale-set cleanup did not fill its worker set")
		}
	}
	select {
	case <-started:
		t.Fatal("scale-set cleanup exceeded its concurrency bound")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := maximum.Load(); got != scaleSetCloseConcurrency {
		t.Fatalf("maximum concurrent closes=%d", got)
	}

	failure := errors.New("secret broker response must not escape")
	if err := closeScaleSetSources(context.Background(), []scaleSetSource{
		&trackedCloseSource{active: &active, maximum: &maximum, started: started, release: closedChannel(), err: failure},
	}); err == nil || !strings.Contains(err.Error(), "scale-set session cleanup failed") || strings.Contains(err.Error(), failure.Error()) {
		t.Fatalf("cleanup error=%v", err)
	}
}

func closedChannel() <-chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}

type recordingTartRunner struct {
	mu    sync.Mutex
	calls [][]string
	fails int
}

func (r *recordingTartRunner) Run(_ context.Context, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, append([]string(nil), args...))
	if r.fails > 0 {
		r.fails--
		return nil, errors.New("not ready")
	}
	return nil, nil
}
func (*recordingTartRunner) Start(context.Context, ...string) error { return nil }

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
	raw := `{"baseVm":"linux","vmPrefix":"gha","pollSeconds":1,"maxLinuxWhenMacosIdle":4,"maxLinuxCpu":8,"maxLinuxMemoryMb":16384,"linuxReservationAgeSeconds":300,"minFreeDiskGb":1,"linuxProfiles":[{"id":"small","label":"linux-small","cpu":1,"memoryMb":2048,"diskGb":50},{"id":"medium","label":"linux-medium","cpu":2,"memoryMb":4096,"diskGb":50},{"id":"large","label":"linux-large","cpu":4,"memoryMb":8192,"diskGb":50}],"macosBurst":{"enabled":true,"baseVm":"mac","vmPrefix":"gha-mac","builder":{"id":"builder","label":"macos-builder","cpu":8,"memoryMb":12288,"maxActive":1},"maestro":{"id":"maestro","label":"macos-maestro","cpu":4,"memoryMb":7168,"maxActive":2}},"targets":[{"type":"repo","slug":"owner/repo","maxActive":4}]` + github + `}`
	path := filepath.Join(t.TempDir(), "fleet.json")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeMultiScopeConfig(t *testing.T) string {
	t.Helper()
	cfg := config.Default()
	cfg.GitHub = config.GitHub{
		SessionOwner:  "fleet-session",
		App:           config.GitHubApp{ClientID: "client", KeychainService: "fleet", KeychainAccount: "app"},
		Installations: []config.GitHubInstallation{{Name: "personal", InstallationID: 101}},
		Scopes: []config.GitHubScope{{Name: "personal-repo", Kind: config.ScopeRepository,
			ConfigURL: "https://github.com/owner/repo", Installation: "personal", Targets: []string{"owner/repo"},
			ScaleSets: []config.ScaleSet{
				{Profile: "small", Name: "repo-small", ID: 11, MaxCapacity: 4, Labels: []string{"self-hosted", "linux-small"}},
				{Profile: "medium", Name: "repo-medium", ID: 12, MaxCapacity: 4, Labels: []string{"self-hosted", "linux-medium"}},
				{Profile: "large", Name: "repo-large", ID: 13, MaxCapacity: 2, Labels: []string{"self-hosted", "linux-large"}},
				{Profile: "builder", Name: "repo-builder", ID: 14, MaxCapacity: 1, Labels: []string{"self-hosted", "macos-builder"}},
				{Profile: "maestro", Name: "repo-maestro", ID: 15, MaxCapacity: 2, Labels: []string{"self-hosted", "macos-maestro"}},
			}}},
	}
	var encoded bytes.Buffer
	if err := config.Encode(&encoded, cfg); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "fleet.json")
	if err := os.WriteFile(path, encoded.Bytes(), 0o600); err != nil {
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
	d.loadKey = func(ctx context.Context, _, _, _ string) (*credentials.Secret, error) {
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

func TestRunReportsRedactedScaleSetCleanupFailure(t *testing.T) {
	d := testDependencies(t)
	ready := make(chan struct{})
	var readyOnce sync.Once
	d.inventory = func(runtimeStore, config.Config) app.Inventory {
		return notifyingInventory{ready: ready, once: &readyOnce}
	}
	brokerFailure := errors.New("private broker response")
	d.newScaleSet = func(context.Context, githubscaleset.GitHubAppScaleSetConfig) (scaleSetSource, error) {
		return &failingCloseSource{err: brokerFailure}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runWithDependencies(ctx, options{
			ConfigPath: writeConfig(t, true), DatabasePath: filepath.Join(t.TempDir(), "fleet.db"),
			HealthAddress: "127.0.0.1:0", Mode: reconcile.Shadow,
		}, d)
	}()
	select {
	case <-ready:
		cancel()
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("runtime did not become ready")
	}
	err := <-done
	if !errors.Is(err, errScaleSetClose) || strings.Contains(err.Error(), brokerFailure.Error()) {
		t.Fatalf("cleanup error=%v", err)
	}
}

// Regression: an official Actions message session can become permanently
// unusable after startup while every other scale set stays healthy. Reusing the
// same failed source forever marks one critical observation unavailable and
// fail-closes the entire scheduler until an operator restarts fleetd.
func TestRunRecreatesBrokenScaleSetSessionWithoutDaemonRestart(t *testing.T) {
	d := testDependencies(t)
	brokenHandled := make(chan struct{}, 1)
	recreated := make(chan struct{})
	var recreatedOnce sync.Once
	broken := &brokenSessionSource{handled: brokenHandled}
	var smallCreations atomic.Int32
	d.newScaleSet = func(_ context.Context, cfg githubscaleset.GitHubAppScaleSetConfig) (scaleSetSource, error) {
		if cfg.ScaleSetID != 1 {
			return &fakeSource{}, nil
		}
		if smallCreations.Add(1) == 1 {
			return broken, nil
		}
		recreatedOnce.Do(func() { close(recreated) })
		return &fakeSource{}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runWithDependencies(ctx, options{
			ConfigPath: writeConfig(t, true), DatabasePath: filepath.Join(t.TempDir(), "fleet.db"),
			HealthAddress: "127.0.0.1:0", Mode: reconcile.Shadow,
		}, d)
	}()
	select {
	case <-brokenHandled:
	case err := <-done:
		cancel()
		t.Fatalf("runtime stopped before session failure: %v", err)
	case <-time.After(time.Second):
		cancel()
		t.Fatal("broken source was not polled")
	}
	select {
	case <-recreated:
		cancel()
	case err := <-done:
		cancel()
		t.Fatalf("runtime stopped instead of recreating session: %v", err)
	case <-time.After(time.Second):
		cancel()
		t.Fatal("broken scale-set session was reused instead of recreated")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if broken.closed.Load() != 1 || smallCreations.Load() != 2 {
		t.Fatalf("broken closes=%d small creations=%d", broken.closed.Load(), smallCreations.Load())
	}
}

// Regression: the released controller accepts the mutation modes in the
// domain model, but the production runtime rejects them before loading its
// validated authority configuration. That makes the documented
// observe -> shadow -> canary -> authority migration impossible.
func TestRunCanaryAndAuthorityAreArmed(t *testing.T) {
	for _, mode := range []reconcile.Mode{reconcile.Canary, reconcile.Authority} {
		t.Run(string(mode), func(t *testing.T) {
			d := testDependencies(t)
			ready := make(chan struct{})
			var readyOnce sync.Once
			d.inventory = func(runtimeStore, config.Config) app.Inventory {
				return notifyingInventory{ready: ready, once: &readyOnce}
			}
			d.newScaleSet = func(context.Context, githubscaleset.GitHubAppScaleSetConfig) (scaleSetSource, error) {
				return &fakeSource{}, nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				opts := options{
					ConfigPath:    writeConfig(t, true),
					DatabasePath:  filepath.Join(t.TempDir(), "fleet.db"),
					HealthAddress: "127.0.0.1:0",
					Mode:          mode,
				}
				if mode == reconcile.Canary {
					opts.CanaryScope, opts.CanaryProfile = legacyScopeName, "small"
				}
				done <- runWithDependencies(ctx, opts, d)
			}()

			select {
			case <-ready:
				cancel()
			case err := <-done:
				cancel()
				t.Fatalf("%s rejected before runtime startup: %v", mode, err)
			case <-time.After(5 * time.Second):
				cancel()
				t.Fatalf("%s runtime did not become ready", mode)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("%s runtime did not stop", mode)
			}
		})
	}
}

// Regression: mutation modes currently persist scheduler operations but never
// run the durable worker. An unknown operation is intentionally used here: a
// live worker must claim it and fail it closed as dead instead of leaving it
// pending forever.
func TestRunAuthorityStartsDurableOperationWorker(t *testing.T) {
	d := testDependencies(t)
	ready := make(chan struct{})
	var readyOnce sync.Once
	d.inventory = func(runtimeStore, config.Config) app.Inventory {
		return notifyingInventory{ready: ready, once: &readyOnce}
	}
	d.newScaleSet = func(context.Context, githubscaleset.GitHubAppScaleSetConfig) (scaleSetSource, error) {
		return &fakeSource{}, nil
	}
	baseOpen := d.openStore
	var opened runtimeStore
	d.openStore = func(ctx context.Context, path string) (runtimeStore, error) {
		store, err := baseOpen(ctx, path)
		if err != nil {
			return nil, err
		}
		now := time.Now().UTC()
		_, err = store.ApplyPlan(ctx, operations.Plan{
			ID: "worker-regression", ExpectedSchedulerVersion: 0, CreatedAt: now,
			Scheduler: operations.SchedulerState{Version: 1},
			Operations: []operations.Operation{{
				ID: "unsupported-operation", IdempotencyKey: "unsupported-operation", EffectKey: "unsupported:resource",
				Kind: "unsupported", ResourceID: "resource", AvailableAt: now,
			}},
		})
		if err != nil {
			_ = store.Close()
			return nil, err
		}
		opened = store
		return store, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runWithDependencies(ctx, options{
			ConfigPath: writeConfig(t, true), DatabasePath: filepath.Join(t.TempDir(), "fleet.db"),
			HealthAddress: "127.0.0.1:0", Mode: reconcile.Authority,
		}, d)
	}()
	select {
	case <-ready:
	case err := <-done:
		cancel()
		t.Fatalf("runtime stopped before worker probe: %v", err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("runtime did not become ready")
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, dead, err := opened.OperationCounts(context.Background())
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		if dead == 1 {
			cancel()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	t.Fatal("durable operation remained pending because no worker was running")
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
		{name: "mode", opts: options{Mode: reconcile.Mode("invalid")}},
		{name: "open config", opts: options{Mode: reconcile.Observe, ConfigPath: "missing"}},
		{name: "decode", opts: options{Mode: reconcile.Observe, ConfigPath: filepath.Join(t.TempDir(), "bad")}, mutate: func(*dependencies) {}},
		{name: "shadow config", opts: options{Mode: reconcile.Shadow, ConfigPath: valid}},
		{name: "open store", opts: options{Mode: reconcile.Observe, ConfigPath: valid}, mutate: func(d *dependencies) {
			d.openStore = func(context.Context, string) (runtimeStore, error) { return nil, want }
		}},
		{name: "listen", opts: options{Mode: reconcile.Observe, ConfigPath: valid, DatabasePath: filepath.Join(t.TempDir(), "x.db")}, mutate: func(d *dependencies) { d.listen = func(string, string) (net.Listener, error) { return nil, want } }},
		{name: "admin listen", opts: options{Mode: reconcile.Observe, ConfigPath: valid, DatabasePath: filepath.Join(t.TempDir(), "x.db")}, mutate: func(d *dependencies) { d.adminListen = func(string) (net.Listener, error) { return nil, want } }},
		{name: "key", opts: options{Mode: reconcile.Shadow, ConfigPath: shadow, DatabasePath: filepath.Join(t.TempDir(), "x.db")}, mutate: func(d *dependencies) {
			d.loadKey = func(context.Context, string, string, string) (*credentials.Secret, error) { return nil, want }
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

func TestRunRejectsUnselectedCanaryAndHeldAuthority(t *testing.T) {
	d := testDependencies(t)
	if err := runWithDependencies(context.Background(), options{Mode: reconcile.Canary}, d); err == nil {
		t.Fatal("selector-free canary accepted")
	}
	if err := runWithDependencies(context.Background(), options{Mode: reconcile.Canary, CanaryScope: "missing", CanaryProfile: "small",
		ConfigPath: writeConfig(t, true), DatabasePath: filepath.Join(t.TempDir(), "canary.db")}, d); err == nil {
		t.Fatal("unresolved canary selector accepted")
	}

	database := filepath.Join(t.TempDir(), "held.db")
	store, err := d.openStore(context.Background(), database)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	lease, err := store.AcquireLease(context.Background(), authorityLeaseName, "other-controller", now, authorityLeaseTTL)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	err = runWithDependencies(context.Background(), options{Mode: reconcile.Authority, ConfigPath: writeConfig(t, true), DatabasePath: database}, d)
	if !errors.Is(err, operations.ErrLeaseHeld) {
		t.Fatalf("held authority err=%v", err)
	}
	cleanup, err := d.openStore(context.Background(), database)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup.Close()
	if err := cleanup.ReleaseLease(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
}

func TestRunDaemonAndInvalidBinding(t *testing.T) {
	if err := runDaemon(context.Background(), options{Mode: reconcile.Mode("invalid")}); err == nil {
		t.Fatal("invalid mode armed")
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
	if _, err := d.loadKey(ctx, "tart-runner-fleet-test-missing", "none", ""); err == nil {
		t.Fatal("missing key found")
	}
	path := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(path, []byte("file-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	fileKey, err := d.loadKey(ctx, "", "", path)
	if err != nil || fileKey == nil {
		t.Fatalf("default file loader = %v, %v", fileKey, err)
	}
	fileKey.Destroy()
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

func TestGitHubCredentialSelectsMultiScopeFileAndLegacyKeychain(t *testing.T) {
	cfg := config.Default()
	cfg.GitHub = config.GitHub{Scopes: []config.GitHubScope{{Name: "scope"}}, App: config.GitHubApp{
		KeychainService: "service", KeychainAccount: "account", PrivateKeyFile: "/secure/app.pem",
	}}
	service, account, path := githubCredential(cfg)
	if service != "service" || account != "account" || path != "/secure/app.pem" {
		t.Fatalf("multi-scope credential = %q/%q/%q", service, account, path)
	}
	cfg.GitHub = config.GitHub{KeychainService: "legacy-service", KeychainAccount: "legacy-account"}
	service, account, path = githubCredential(cfg)
	if service != "legacy-service" || account != "legacy-account" || path != "" {
		t.Fatalf("legacy credential = %q/%q/%q", service, account, path)
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
	health, _ := telemetry.NewHealth(wallClock{}, telemetry.HealthConfig{CriticalObservations: []string{"operations", "scheduler"}})
	if err := (engineTicker{engine: engine, health: health, operationCounts: func(context.Context) (int, int, error) { return 2, 1, nil }}).Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if health.Snapshot().Observations["scheduler"].Freshness != telemetry.ObservationStale ||
		health.Snapshot().Observations["operations"].Freshness != telemetry.ObservationFresh ||
		health.Snapshot().OperationRetries != 2 || health.Snapshot().DeadOperations != 1 {
		t.Fatalf("snapshot=%#v", health.Snapshot())
	}
}

func TestEngineTickerMarksOperationSummaryUnavailable(t *testing.T) {
	d := testDependencies(t)
	store, err := d.openStore(context.Background(), filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	engine := app.Engine{Store: store, Inventory: runtimeInventory{}, Config: app.BuildSchedulerConfig(config.Default()), ControllerID: "c", Mode: reconcile.Observe, Now: func() time.Time { return now }}
	health, _ := telemetry.NewHealth(wallClock{}, telemetry.HealthConfig{CriticalObservations: []string{"operations", "scheduler"}})
	ticker := engineTicker{engine: engine, health: health, operationCounts: func(context.Context) (int, int, error) { return 0, 0, errors.New("db unavailable") }}
	if err := ticker.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if health.Snapshot().Observations["operations"].Freshness != telemetry.ObservationUnavailable {
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

func TestCanaryBindingSelectionIsExactAndLegacyCompatible(t *testing.T) {
	bindings := []app.Binding{
		{StoreKey: 11, ScaleSetID: 7, Scope: "personal-repo", Profile: domain.Profile{ID: "small"}},
		{StoreKey: 12, ScaleSetID: 8, Scope: "personal-repo", Profile: domain.Profile{ID: "medium"}},
		{StoreKey: 13, ScaleSetID: 7, Scope: "organization", Profile: domain.Profile{ID: "small"}},
	}
	selected, err := selectRuntimeBindings(bindings, options{Mode: reconcile.Canary, CanaryScope: "personal-repo", CanaryProfile: "small"})
	if err != nil || len(selected) != 1 || selected[0].StoreKey != 11 {
		t.Fatalf("selected=%#v err=%v", selected, err)
	}
	if len(selected[0].RequiredLabels) != 1 || selected[0].RequiredLabels[0] != canaryDemandLabel {
		t.Fatalf("canary binding does not require dedicated demand label: %#v", selected[0])
	}
	if _, err := selectRuntimeBindings(bindings, options{Mode: reconcile.Canary, CanaryScope: "missing", CanaryProfile: "small"}); err == nil {
		t.Fatal("missing canary binding accepted")
	}
	legacy := []app.Binding{{StoreKey: 7, ScaleSetID: 7, Profile: domain.Profile{ID: "small"}}}
	selected, err = selectRuntimeBindings(legacy, options{Mode: reconcile.Canary, CanaryScope: legacyScopeName, CanaryProfile: "small"})
	if err != nil || len(selected) != 1 {
		t.Fatalf("legacy selected=%#v err=%v", selected, err)
	}
}

func TestSourceSettingsUseScopedInstallationAndDurableCursorKey(t *testing.T) {
	cfg := config.Default()
	cfg.GitHub = config.GitHub{
		SessionOwner:  "fleet-session",
		App:           config.GitHubApp{ClientID: "client", KeychainService: "service", KeychainAccount: "account"},
		Installations: []config.GitHubInstallation{{Name: "personal", InstallationID: 101}, {Name: "org", InstallationID: 202}},
		Scopes: []config.GitHubScope{
			{Name: "personal-repo", Kind: config.ScopeRepository, ConfigURL: "https://github.com/owner/repo", Installation: "personal", Targets: []string{"owner/repo"}},
			{Name: "organization", Kind: config.ScopeOrganization, ConfigURL: "https://github.com/budgie-at", Installation: "org", Targets: []string{"budgie-at/budgie"}},
		},
	}
	binding := app.Binding{StoreKey: 9988, ScaleSetID: 42, Scope: "organization", Targets: []string{"budgie-at/budgie"}, Profile: domain.Profile{ID: "small"}}
	scale := config.ScaleSet{Profile: "small", ID: 42, MaxCapacity: 4}
	cfg.GitHub.Scopes[1].ScaleSets = []config.ScaleSet{scale}
	settings, err := sourceSettingsFor(cfg, binding)
	if err != nil {
		t.Fatal(err)
	}
	if settings.configURL != "https://github.com/budgie-at" || settings.installationID != 202 || settings.owner != "fleet-session" ||
		settings.scaleSet.ID != scale.ID || settings.scaleSet.Profile != scale.Profile || settings.cursorKey != binding.StoreKey {
		t.Fatalf("settings=%#v", settings)
	}
}

func TestSourceSettingsRejectIncompleteOrMismatchedBindings(t *testing.T) {
	legacy := config.Default()
	legacy.GitHub = config.GitHub{ConfigURL: "https://github.com/owner", ClientID: "client", InstallationID: 1, Owner: "owner",
		ScaleSets: []config.ScaleSet{{Profile: "small", ID: 7}}}
	if _, err := sourceSettingsFor(legacy, app.Binding{}); err == nil {
		t.Fatal("incomplete binding accepted")
	}
	if _, err := sourceSettingsFor(legacy, app.Binding{StoreKey: 7, ScaleSetID: 8, Profile: domain.Profile{ID: "small"}}); err == nil {
		t.Fatal("unknown legacy scale set accepted")
	}
	multi := legacy
	multi.GitHub = config.GitHub{SessionOwner: "owner", App: config.GitHubApp{ClientID: "client"},
		Installations: []config.GitHubInstallation{{Name: "known", InstallationID: 11}},
		Scopes:        []config.GitHubScope{{Name: "scope", Installation: "missing", ScaleSets: []config.ScaleSet{{Profile: "small", ID: 7}}}}}
	binding := app.Binding{StoreKey: 77, ScaleSetID: 7, Scope: "scope", Profile: domain.Profile{ID: "small"}}
	if _, err := sourceSettingsFor(multi, binding); err == nil {
		t.Fatal("unknown installation accepted")
	}
	multi.GitHub.Scopes[0].Installation = "known"
	binding.ScaleSetID = 8
	if _, err := sourceSettingsFor(multi, binding); err == nil {
		t.Fatal("unknown scoped scale set accepted")
	}
	binding.Scope = "unknown"
	if _, err := sourceSettingsFor(multi, binding); err == nil {
		t.Fatal("unknown scope accepted")
	}
}

func TestRunMultiScopeUsesCorrectInstallationAndStoreKeys(t *testing.T) {
	d := testDependencies(t)
	ready := make(chan struct{})
	var once sync.Once
	d.inventory = func(runtimeStore, config.Config) app.Inventory { return notifyingInventory{ready: ready, once: &once} }
	var mu sync.Mutex
	var opened []githubscaleset.GitHubAppScaleSetConfig
	var cursorKeys []int64
	d.newScaleSet = func(_ context.Context, cfg githubscaleset.GitHubAppScaleSetConfig) (scaleSetSource, error) {
		mu.Lock()
		opened = append(opened, cfg)
		mu.Unlock()
		return &fakeSource{}, nil
	}
	d.cursor = func(_ context.Context, _ runtimeStore, key int64) (int64, error) {
		mu.Lock()
		cursorKeys = append(cursorKeys, key)
		mu.Unlock()
		return 0, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runWithDependencies(ctx, options{ConfigPath: writeMultiScopeConfig(t), DatabasePath: filepath.Join(t.TempDir(), "fleet.db"),
			HealthAddress: "127.0.0.1:0", Mode: reconcile.Shadow}, d)
	}()
	select {
	case <-ready:
		cancel()
	case err := <-done:
		cancel()
		t.Fatalf("runtime stopped: %v", err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("runtime not ready")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(opened) != 5 || len(cursorKeys) != 5 {
		t.Fatalf("sources=%d cursors=%#v", len(opened), cursorKeys)
	}
	for index, source := range opened {
		if source.GitHubConfigURL != "https://github.com/owner/repo" || source.InstallationID != 101 || source.Owner != "fleet-session" {
			t.Fatalf("source[%d]=%#v", index, source)
		}
		if cursorKeys[index] == int64(source.ScaleSetID) || cursorKeys[index] <= 0 {
			t.Fatalf("cursor key %d reused server ID %d", cursorKeys[index], source.ScaleSetID)
		}
	}
}

func TestTartReadinessUsesFixedBoundedArgumentVector(t *testing.T) {
	recorder := &recordingTartRunner{fails: 1}
	probe := tartReadiness{Runner: recorder, Timeout: time.Second, AttemptTimeout: 100 * time.Millisecond, RetryInterval: time.Millisecond}
	instance := operations.Instance{ID: "trf-small-ready"}
	if err := probe.Wait(context.Background(), instance); err != nil {
		t.Fatal(err)
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.calls) != 2 {
		t.Fatalf("calls=%#v", recorder.calls)
	}
	for _, call := range recorder.calls {
		if strings.Join(call, " ") != "exec trf-small-ready true" {
			t.Fatalf("unsafe readiness command: %#v", call)
		}
	}
	if err := (tartReadiness{Runner: &recordingTartRunner{}, Timeout: time.Second}).Wait(context.Background(), operations.Instance{ID: "bad name"}); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("invalid name err=%v", err)
	}
}

func TestTartReadinessDefaultsRemainBounded(t *testing.T) {
	recorder := &recordingTartRunner{fails: 100}
	probe := tartReadiness{Runner: recorder, Timeout: 2 * time.Millisecond}
	if err := probe.Wait(context.Background(), operations.Instance{ID: "trf-timeout"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout err=%v", err)
	}
	if _, err := selectRuntimeBindings(nil, options{Mode: reconcile.Canary}); err == nil {
		t.Fatal("empty canary selector accepted")
	}
}

func TestAuthorityLeaseRecoversExpiredWorkAndFencesAnotherController(t *testing.T) {
	d := testDependencies(t)
	store, err := d.openStore(context.Background(), filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	base := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	_, err = store.ApplyPlan(context.Background(), operations.Plan{ID: "lease-recovery", ExpectedSchedulerVersion: 0, CreatedAt: base,
		Scheduler: operations.SchedulerState{Version: 1}, Operations: []operations.Operation{{ID: "expired", IdempotencyKey: "expired", EffectKey: "x:1", Kind: "x", ResourceID: "1", AvailableAt: base}}})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.Claim(context.Background(), "dead-owner", 1, base, time.Second)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claimed=%#v err=%v", claimed, err)
	}
	now := func() time.Time { return base.Add(2 * time.Second) }
	guard, err := startAuthorityLease(context.Background(), store, "controller-a", now, func(time.Duration) <-chan time.Time { return make(chan time.Time) })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcquireLease(context.Background(), authorityLeaseName, "controller-b", now(), authorityLeaseTTL); !errors.Is(err, operations.ErrLeaseHeld) {
		guard.Close()
		t.Fatalf("second controller was not fenced: %v", err)
	}
	recovered, err := store.Claim(context.Background(), "controller-a", 1, now(), time.Second)
	if err != nil || len(recovered) != 1 || recovered[0].ID != "expired" {
		guard.Close()
		t.Fatalf("recovered=%#v err=%v", recovered, err)
	}
	guard.Close()
	lease, err := store.AcquireLease(context.Background(), authorityLeaseName, "controller-b", now(), authorityLeaseTTL)
	if err != nil {
		t.Fatalf("released authority remained fenced: %v", err)
	}
	if err := store.ReleaseLease(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
}

type recoverFailStore struct{ operations.Store }

func (recoverFailStore) RecoverExpired(context.Context, time.Time) (int64, error) {
	return 0, errors.New("recover failed")
}

func TestAuthorityLeaseDefaultsRecoveryFailureRenewalAndLoss(t *testing.T) {
	d := testDependencies(t)
	store, err := d.openStore(context.Background(), filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := startAuthorityLease(context.Background(), nil, "owner", nil, nil); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("nil store err=%v", err)
	}
	if _, err := startAuthorityLease(context.Background(), store, "", nil, nil); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("empty owner err=%v", err)
	}
	guard, err := startAuthorityLease(context.Background(), store, "defaults", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	guard.Close()
	guard.Close()
	(*authorityLease)(nil).Close()

	base := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	if _, err := startAuthorityLease(context.Background(), recoverFailStore{Store: store}, "recovery", func() time.Time { return base }, nil); err == nil {
		t.Fatal("recovery failure ignored")
	}

	var clock atomic.Int64
	clock.Store(base.UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	ticks := make(chan time.Time, 2)
	guard, err = startAuthorityLease(context.Background(), store, "renewing", now, func(time.Duration) <-chan time.Time { return ticks })
	if err != nil {
		t.Fatal(err)
	}
	clock.Store(base.Add(time.Second).UnixNano())
	ticks <- time.Time{}
	deadline := time.Now().Add(time.Second)
	for {
		guard.mu.Lock()
		expiry := guard.lease.ExpiresAt
		current := guard.lease
		guard.mu.Unlock()
		if expiry.Equal(base.Add(time.Second + authorityLeaseTTL)) {
			if err := store.ReleaseLease(context.Background(), current); err != nil {
				guard.Close()
				t.Fatal(err)
			}
			break
		}
		if time.Now().After(deadline) {
			guard.Close()
			t.Fatal("authority lease did not renew")
		}
		time.Sleep(time.Millisecond)
	}
	clock.Store(base.Add(2 * time.Second).UnixNano())
	ticks <- time.Time{}
	select {
	case err := <-guard.errors:
		if !errors.Is(err, operations.ErrLeaseLost) {
			t.Fatalf("renewal err=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("lease loss was not reported")
	}
	guard.Close()
}

type renewFailRuntimeStore struct{ runtimeStore }

func (renewFailRuntimeStore) RenewLease(context.Context, operations.Lease, time.Time, time.Duration) (operations.Lease, error) {
	return operations.Lease{}, errors.New("renew failed")
}

func TestRunStopsWhenAuthorityLeaseIsLost(t *testing.T) {
	d := testDependencies(t)
	baseOpen := d.openStore
	d.openStore = func(ctx context.Context, path string) (runtimeStore, error) {
		store, err := baseOpen(ctx, path)
		if err != nil {
			return nil, err
		}
		return renewFailRuntimeStore{runtimeStore: store}, nil
	}
	d.newScaleSet = func(context.Context, githubscaleset.GitHubAppScaleSetConfig) (scaleSetSource, error) {
		return &fakeSource{}, nil
	}
	d.after = func(time.Duration) <-chan time.Time {
		ready := make(chan time.Time, 1)
		ready <- time.Now()
		return ready
	}
	err := runWithDependencies(context.Background(), options{ConfigPath: writeConfig(t, true), DatabasePath: filepath.Join(t.TempDir(), "fleet.db"),
		HealthAddress: "127.0.0.1:0", Mode: reconcile.Authority}, d)
	if err == nil || !strings.Contains(err.Error(), "authority lost") {
		t.Fatalf("err=%v", err)
	}
}

func TestCanaryWorkerConcurrencyIsOne(t *testing.T) {
	cfg := config.Default()
	if got := workerConcurrency(reconcile.Canary, cfg); got != 1 {
		t.Fatalf("canary concurrency=%d", got)
	}
	if got := workerConcurrency(reconcile.Authority, cfg); got != cfg.Linux.MaxInstances {
		t.Fatalf("authority concurrency=%d", got)
	}
}

func TestProfileDiskFloorsPreserveProfileIdentity(t *testing.T) {
	cfg := config.Default()
	cfg.MacOS.Builder.DiskGiB = 160
	cfg.MacOS.Maestro.DiskGiB = 150
	got := profileDiskFloors(cfg)
	want := map[domain.ProfileID]int{"small": 50, "medium": 50, "large": 50, "builder": 160, "maestro": 150}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("disk floors = %#v, want %#v", got, want)
	}
}

type lifecycleVM struct {
	clone atomic.Int32
	start atomic.Int32
}

func (v *lifecycleVM) Clone(context.Context, tart.Request) error {
	v.clone.Add(1)
	return nil
}
func (v *lifecycleVM) Start(context.Context, string, operations.Ownership) error {
	v.start.Add(1)
	return nil
}
func (*lifecycleVM) Stop(context.Context, string, operations.Ownership) error   { return nil }
func (*lifecycleVM) Delete(context.Context, string, operations.Ownership) error { return nil }

type readyProbe struct{}

func (readyProbe) Wait(context.Context, operations.Instance) error { return nil }

type bootstrapProbe struct{}

func (bootstrapProbe) Bootstrap(context.Context, string, *githubscaleset.JITSecret) error { return nil }

func TestRuntimeExecutorsDriveProvisionToAssigned(t *testing.T) {
	d := testDependencies(t)
	vm := &lifecycleVM{}
	d.newVM = func(runtimeStore, config.Config, lifecycle.DrainControl) lifecycle.VMControl { return vm }
	d.readiness = func(config.Config) lifecycle.Readiness { return readyProbe{} }
	d.bootstrap = func(config.Config) lifecycle.Bootstrapper { return bootstrapProbe{} }
	d.newScaleSet = func(context.Context, githubscaleset.GitHubAppScaleSetConfig) (scaleSetSource, error) {
		return &fakeSource{}, nil
	}
	baseOpen := d.openStore
	var opened runtimeStore
	d.openStore = func(ctx context.Context, path string) (runtimeStore, error) {
		store, err := baseOpen(ctx, path)
		if err != nil {
			return nil, err
		}
		now := time.Now().UTC()
		ownership := operations.Ownership{ControllerID: "controller", ResourceID: "demand", OperationID: "provision-op"}
		instance := operations.Instance{ID: "trf-small-runtime", Repo: "owner/repo", Platform: domain.PlatformLinux, Profile: "small", Route: "linux-small",
			Resources: domain.Resources{CPU: 1, MemoryMB: 2048, Slots: 1}, Demand: domain.DemandKey{Repo: "owner/repo", RunID: 1, JobID: 2, Attempt: 1},
			State: operations.StatePlanned, Ownership: ownership, CreatedAt: now, UpdatedAt: now}
		_, err = store.ApplyPlan(ctx, operations.Plan{ID: "runtime-provision-plan", ExpectedSchedulerVersion: 0, CreatedAt: now,
			Scheduler: operations.SchedulerState{Version: 1}, Instances: []operations.InstanceIntent{{ExpectedVersion: -1, Instance: instance}},
			Operations: []operations.Operation{{ID: "provision-op", IdempotencyKey: "provision-op", EffectKey: "clone:" + instance.ID,
				Kind: lifecycle.OperationProvision, ResourceID: instance.ID, AvailableAt: now}}})
		if err != nil {
			_ = store.Close()
			return nil, err
		}
		opened = store
		return store, nil
	}

	ready := make(chan struct{})
	var once sync.Once
	d.inventory = func(runtimeStore, config.Config) app.Inventory { return notifyingInventory{ready: ready, once: &once} }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runWithDependencies(ctx, options{ConfigPath: writeConfig(t, true), DatabasePath: filepath.Join(t.TempDir(), "fleet.db"), HealthAddress: "127.0.0.1:0", Mode: reconcile.Authority}, d)
	}()
	select {
	case <-ready:
	case err := <-done:
		cancel()
		t.Fatalf("runtime stopped: %v", err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("runtime not ready")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		instance, err := opened.Instance(context.Background(), "trf-small-runtime")
		if err == nil && instance.State == operations.StateAssigned {
			cancel()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if vm.clone.Load() != 1 || vm.start.Load() != 1 {
				t.Fatalf("clone=%d start=%d", vm.clone.Load(), vm.start.Load())
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	t.Fatal("provision operation did not reach assigned")
}
