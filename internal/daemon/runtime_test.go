package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/macos"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/sqlite"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/tart"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/credentials"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/executor"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/lifecycle"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/reconcile"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/telemetry"
)

type runtimeDoerFunc func(*http.Request) (*http.Response, error)

func (f runtimeDoerFunc) Do(request *http.Request) (*http.Response, error) { return f(request) }

func runtimeResponse(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestRESTQueueIngesterPollsPersistsWaitsAndReportsFailures(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	doer := runtimeDoerFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/repos/budgie-at/budgie/actions/runs":
			return runtimeResponse(`{"workflow_runs":[{"id":10,"run_attempt":2,"status":"in_progress","created_at":"2026-07-17T09:47:50Z"}]}`), nil
		case "/repos/budgie-at/budgie/actions/runs/10/jobs":
			return runtimeResponse(`{"jobs":[{"id":101,"name":"Build iOS E2E app","status":"queued","labels":["self-hosted","macos-builder"],"started_at":"2026-07-17T09:47:52Z"}]}`), nil
		case "/repos/budgie-at/budgie/actions/runners":
			return runtimeResponse(`{"runners":[]}`), nil
		default:
			t.Fatalf("unexpected REST path %q", request.URL.Path)
			return nil, nil
		}
	})
	observer, err := githubscaleset.NewObserver(githubscaleset.ObserverConfig{BaseURL: "https://api.test",
		Repositories: []githubscaleset.Repository{{Owner: "budgie-at", Name: "budgie"}}, HTTP: doer,
		Tokens: githubscaleset.TokenSourceFunc(func(context.Context) (string, error) { return "token", nil })})
	if err != nil {
		t.Fatal(err)
	}
	binding := app.Binding{ScaleSetID: 1, Targets: []string{"budgie-at/budgie"},
		Profile: domain.Profile{ID: "builder", Route: "macos-builder", Platform: domain.PlatformMacOS}}
	health, _ := telemetry.NewHealth(wallClock{}, telemetry.HealthConfig{Profiles: []string{"builder"}, CriticalObservations: []string{"github-rest-legacy"}})
	ingester := &restQueueIngester{coordinator: app.DemandCoordinator{Store: store}, bindings: []app.Binding{binding},
		observer: observer, interval: time.Second, health: health, observation: "github-rest-legacy", now: func() time.Time { return now }}
	changed, err := ingester.IngestChanged(ctx)
	if err != nil || !changed {
		t.Fatalf("first ingest = %v, %v", changed, err)
	}
	jobs, err := store.QueuedGitHubJobs(ctx, 1)
	if err != nil || len(jobs) != 1 || jobs[0].WorkflowJobID != 101 {
		t.Fatalf("persisted jobs = %#v, %v", jobs, err)
	}
	now = now.Add(2 * time.Second)
	if err := ingester.Ingest(ctx); err != nil {
		t.Fatal(err)
	}
	if fresh := health.Snapshot().Observations["github-rest-legacy"]; fresh.Freshness != telemetry.ObservationFresh ||
		fresh.Detail != "" {
		t.Fatalf("fresh REST observation = %+v", fresh)
	}
	timed := &restQueueIngester{coordinator: app.DemandCoordinator{Store: store}, bindings: []app.Binding{binding},
		observer: observer, interval: time.Second, now: time.Now, next: time.Now().Add(time.Millisecond)}
	if _, err := timed.IngestChanged(ctx); err != nil {
		t.Fatalf("timed poll = %v", err)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := ingester.IngestChanged(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait = %v", err)
	}
	broken, _ := githubscaleset.NewObserver(githubscaleset.ObserverConfig{BaseURL: "https://api.test",
		Repositories: []githubscaleset.Repository{{Owner: "budgie-at", Name: "budgie"}}, HTTP: runtimeDoerFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("down")
		}), Tokens: githubscaleset.TokenSourceFunc(func(context.Context) (string, error) { return "token", nil })})
	ingester.observer = broken
	now = now.Add(2 * time.Second)
	if _, err := ingester.IngestChanged(ctx); err == nil ||
		health.Snapshot().Observations["github-rest-legacy"].Freshness != telemetry.ObservationStale ||
		health.Snapshot().Observations["github-rest-legacy"].Detail != githubscaleset.ReasonQueueObservationStale {
		t.Fatalf("stale REST failure = %v observation=%#v", err, health.Snapshot().Observations)
	}
	unavailable := &restQueueIngester{coordinator: app.DemandCoordinator{Store: store}, bindings: []app.Binding{binding}, observer: broken,
		interval: time.Second, health: health, observation: "github-rest-legacy", now: func() time.Time { return now }}
	if _, err := unavailable.IngestChanged(ctx); err == nil ||
		health.Snapshot().Observations["github-rest-legacy"].Freshness != telemetry.ObservationUnavailable ||
		health.Snapshot().Observations["github-rest-legacy"].Detail != githubscaleset.ReasonQueueObservationFailed {
		t.Fatalf("unavailable REST failure = %v observation=%#v", err, health.Snapshot().Observations)
	}
	closed, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	_ = closed.Close()
	ingester = &restQueueIngester{coordinator: app.DemandCoordinator{Store: closed}, bindings: []app.Binding{binding}, observer: observer,
		interval: time.Second, health: health, observation: "github-rest-legacy", now: func() time.Time { return now }}
	if _, err := ingester.IngestChanged(ctx); err == nil ||
		health.Snapshot().Observations["github-rest-legacy"].Detail != githubscaleset.ReasonQueueReconcileFailed {
		t.Fatalf("REST persistence failure = %v observation=%#v", err, health.Snapshot().Observations)
	}
	if _, err := (*restQueueIngester)(nil).IngestChanged(ctx); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("nil ingester = %v", err)
	}
}

func TestRESTBindingGroupingAndRepositorySelection(t *testing.T) {
	bindings := []app.Binding{
		{Scope: "scope", Targets: []string{"o/r", "invalid", "o/r"}},
		{Scope: "scope", Targets: []string{"o/other"}},
		{},
	}
	grouped := bindingsByScope(bindings)
	if len(grouped["scope"]) != 2 || len(grouped[legacyScopeName]) != 1 {
		t.Fatalf("grouped = %#v", grouped)
	}
	repositories := repositoriesForBindings(grouped["scope"], nil)
	if len(repositories) != 2 || repositories[0].Owner != "o" || repositories[1].Name != "other" {
		t.Fatalf("repositories = %#v", repositories)
	}
	repositories = repositoriesForBindings(grouped[legacyScopeName], []config.Target{{Slug: "fallback/repo"}})
	if len(repositories) != 1 || repositories[0].Owner != "fallback" {
		t.Fatalf("fallback repositories = %#v", repositories)
	}
}

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

type fakeSource struct {
	closed         atomic.Int32
	directJITCalls atomic.Int32
}

type closeErrorSource struct{ fakeSource }

func (s *closeErrorSource) Close(context.Context) error {
	s.closed.Add(1)
	return errors.New("close failed")
}

func (*fakeSource) Handle(ctx context.Context, _ func(context.Context, githubscaleset.Demand) error) error {
	<-ctx.Done()
	return ctx.Err()
}
func (f *fakeSource) Close(context.Context) error                    { f.closed.Add(1); return nil }
func (*fakeSource) Registered(context.Context, string) (bool, error) { return true, nil }
func (*fakeSource) AcquireAndGenerateJIT(context.Context, int64, string, string) (*githubscaleset.JITSecret, error) {
	return githubscaleset.NewJITSecret("jit"), nil
}
func (f *fakeSource) GenerateJIT(context.Context, string, string) (*githubscaleset.JITSecret, error) {
	f.directJITCalls.Add(1)
	return githubscaleset.NewJITSecret("direct-jit"), nil
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

type successfulSessionSource struct {
	fakeSource
	handled atomic.Int32
}

func (s *successfulSessionSource) Handle(context.Context, func(context.Context, githubscaleset.Demand) error) error {
	s.handled.Add(1)
	return nil
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
func (*recordingTartRunner) Start(context.Context, ...string) (tart.StartedCommand, error) {
	return nil, nil
}

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
		github = `,"github":{"configUrl":"https://github.com/owner","owner":"owner","clientId":"client","installationId":1,"keychainService":"fleet","keychainAccount":"app","canonicalJobInventory":true,"scaleSets":[{"profile":"small","id":1,"maxCapacity":4},{"profile":"medium","id":2,"maxCapacity":4},{"profile":"large","id":3,"maxCapacity":2},{"profile":"builder","id":4,"maxCapacity":1},{"profile":"maestro","id":5,"maxCapacity":2}]}`
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
		SessionOwner:          "fleet-session",
		CanonicalJobInventory: true,
		App:                   config.GitHubApp{ClientID: "client", KeychainService: "fleet", KeychainAccount: "app"},
		Installations:         []config.GitHubInstallation{{Name: "personal", InstallationID: 101}},
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
	d.inventory = func(runtimeStore, config.Config, app.RecoveryObserver) app.Inventory { return runtimeInventory{} }
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
			var recovery app.RecoveryObserver
			d.inventory = func(_ runtimeStore, _ config.Config, observer app.RecoveryObserver) app.Inventory {
				recovery = observer
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
			if (mode == reconcile.Observe) != (recovery == nil) {
				t.Fatalf("%s recovery observer = %T", mode, recovery)
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

// Regression: canonical inventory is the only safe way for an observe
// candidate to prove complete GitHub queue visibility beside the incumbent.
// Observe must poll the read-only Actions REST API without opening a competing
// official scale-set message session or acquiring lifecycle authority.
func TestRunObservePollsCanonicalRESTWithoutScaleSetSession(t *testing.T) {
	d := testDependencies(t)
	var keyLoads atomic.Int32
	baseLoadKey := d.loadKey
	d.loadKey = func(ctx context.Context, service, account, path string) (*credentials.Secret, error) {
		keyLoads.Add(1)
		return baseLoadKey(ctx, service, account, path)
	}
	var scaleSetCreations atomic.Int32
	d.newScaleSet = func(context.Context, githubscaleset.GitHubAppScaleSetConfig) (scaleSetSource, error) {
		scaleSetCreations.Add(1)
		return nil, errors.New("observe opened a scale-set session")
	}
	observed := make(chan struct{})
	var observedOnce sync.Once
	d.newRESTObserver = func(cfg githubscaleset.ObserverConfig) (queueObserver, error) {
		if len(cfg.Repositories) != 1 || cfg.Repositories[0].Owner != "owner" || cfg.Repositories[0].Name != "repo" {
			return nil, errors.New("unexpected observer repositories")
		}
		return githubscaleset.NewObserver(githubscaleset.ObserverConfig{
			BaseURL:      cfg.BaseURL,
			Repositories: cfg.Repositories,
			HTTP: runtimeDoerFunc(func(*http.Request) (*http.Response, error) {
				observedOnce.Do(func() { close(observed) })
				return runtimeResponse(`{"workflow_runs":[]}`), nil
			}),
			Tokens:  githubscaleset.TokenSourceFunc(func(context.Context) (string, error) { return "token", nil }),
			Timeout: cfg.Timeout,
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runWithDependencies(ctx, options{
			ConfigPath: writeConfig(t, true), DatabasePath: filepath.Join(t.TempDir(), "fleet.db"),
			HealthAddress: "127.0.0.1:0", Mode: reconcile.Observe,
		}, d)
	}()
	select {
	case <-observed:
		cancel()
	case err := <-done:
		cancel()
		t.Fatalf("observe stopped before canonical inventory: %v", err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("observe did not poll canonical REST inventory")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if keyLoads.Load() != 1 || scaleSetCreations.Load() != 0 {
		t.Fatalf("key loads=%d scale-set sessions=%d", keyLoads.Load(), scaleSetCreations.Load())
	}
}

func TestRunReportsRedactedScaleSetCleanupFailure(t *testing.T) {
	d := testDependencies(t)
	ready := make(chan struct{})
	var readyOnce sync.Once
	d.inventory = func(runtimeStore, config.Config, app.RecoveryObserver) app.Inventory {
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
	var smallCursorReads atomic.Int32
	baseCursor := d.cursor
	d.cursor = func(ctx context.Context, store runtimeStore, key int64) (int64, error) {
		if key == 1 {
			smallCursorReads.Add(1)
		}
		return baseCursor(ctx, store, key)
	}
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
	if broken.closed.Load() != 1 || smallCreations.Load() != 2 || smallCursorReads.Load() != 2 {
		t.Fatalf("broken closes=%d small creations=%d cursor reads=%d", broken.closed.Load(), smallCreations.Load(), smallCursorReads.Load())
	}
}

func TestRecoveringScaleSetSourceDelegatesAndFailsClosed(t *testing.T) {
	open := func(context.Context) (scaleSetSource, error) { return &fakeSource{}, nil }
	if _, err := newRecoveringScaleSetSource(recoveringScaleSetConfig{open: open,
		limiter: make(chan struct{}, 1)}); err == nil {
		t.Fatal("nil initial source accepted")
	}
	if _, err := newRecoveringScaleSetSource(recoveringScaleSetConfig{source: &fakeSource{},
		limiter: make(chan struct{}, 1)}); err == nil {
		t.Fatal("nil source factory accepted")
	}
	if _, err := newRecoveringScaleSetSource(recoveringScaleSetConfig{source: &fakeSource{},
		open: open}); err == nil {
		t.Fatal("nil recovery limiter accepted")
	}

	initial := &fakeSource{}
	source, err := newRecoveringScaleSetSource(recoveringScaleSetConfig{source: initial, open: open,
		limiter: make(chan struct{}, 1)})
	if err != nil {
		t.Fatal(err)
	}
	registered, err := source.Registered(context.Background(), "runner")
	if err != nil || !registered {
		t.Fatalf("registered=%v err=%v", registered, err)
	}
	jit, err := source.AcquireAndGenerateJIT(context.Background(), 1, "runner", "_work")
	if err != nil || jit == nil {
		t.Fatalf("jit=%v err=%v", jit, err)
	}
	jit.Destroy()
	direct, ok := any(source).(interface {
		GenerateJIT(context.Context, string, string) (*githubscaleset.JITSecret, error)
	})
	if !ok {
		t.Fatal("recovering source dropped direct JIT generation")
	}
	directJIT, err := direct.GenerateJIT(context.Background(), "runner", "_work")
	if err != nil || directJIT == nil || initial.directJITCalls.Load() != 1 {
		t.Fatalf("direct jit=%v err=%v calls=%d", directJIT, err, initial.directJITCalls.Load())
	}
	directJIT.Destroy()
	if err := source.Deregister(context.Background(), "runner"); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(context.Background()); err != nil || initial.closed.Load() != 1 {
		t.Fatalf("second close err=%v closes=%d", err, initial.closed.Load())
	}
	if _, err := source.Registered(context.Background(), "runner"); err == nil {
		t.Fatal("closed recovering source accepted a control call")
	}
	if _, err := source.AcquireAndGenerateJIT(context.Background(), 1, "runner", "_work"); err == nil {
		t.Fatal("closed recovering source generated JIT configuration")
	}
	if _, err := direct.GenerateJIT(context.Background(), "runner", "_work"); err == nil {
		t.Fatal("closed recovering source generated direct JIT configuration")
	}
	if err := source.Deregister(context.Background(), "runner"); err == nil {
		t.Fatal("closed recovering source deregistered a runner")
	}
}

func TestRecoveringScaleSetSourceBoundsReplacementFailures(t *testing.T) {
	closeFailure := &failingCloseSource{err: errors.New("private close detail")}
	openCalls := 0
	source, err := newRecoveringScaleSetSource(recoveringScaleSetConfig{source: closeFailure,
		open: func(context.Context) (scaleSetSource, error) {
			openCalls++
			return &fakeSource{}, nil
		}, limiter: make(chan struct{}, 1)})
	if err != nil {
		t.Fatal(err)
	}
	if err := source.replace(context.Background(), closeFailure, false); !errors.Is(err, errScaleSetClose) || openCalls != 0 || strings.Contains(err.Error(), "private") {
		t.Fatalf("close replacement err=%v opens=%d", err, openCalls)
	}

	initial := &fakeSource{}
	source, _ = newRecoveringScaleSetSource(recoveringScaleSetConfig{source: initial,
		open: func(context.Context) (scaleSetSource, error) {
			return nil, errors.New("private open detail")
		}, limiter: make(chan struct{}, 1)})
	if err := source.replace(context.Background(), initial, false); err == nil || strings.Contains(err.Error(), "private") {
		t.Fatalf("open replacement err=%v", err)
	}
	if err := source.replace(context.Background(), &fakeSource{}, false); err != nil {
		t.Fatalf("stale replacement err=%v", err)
	}
	source.limiter <- struct{}{}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := source.replace(canceled, initial, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled replacement err=%v", err)
	}
	<-source.limiter
}

func TestRecoveringScaleSetSourceSuccessAndTerminalBranches(t *testing.T) {
	initial := &successfulSessionSource{}
	replacement := &fakeSource{}
	source, err := newRecoveringScaleSetSource(recoveringScaleSetConfig{source: initial,
		open:    func(context.Context) (scaleSetSource, error) { return replacement, nil },
		limiter: make(chan struct{}, 1)})
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Handle(context.Background(), func(context.Context, githubscaleset.Demand) error { return nil }); err != nil || initial.handled.Load() != 1 {
		t.Fatalf("successful handle err=%v calls=%d", err, initial.handled.Load())
	}
	if err := source.replace(context.Background(), initial, false); err != nil {
		t.Fatal(err)
	}
	source.mu.RLock()
	current := source.source
	source.mu.RUnlock()
	if current != replacement || initial.closed.Load() != 1 {
		t.Fatalf("replacement=%T initial closes=%d", current, initial.closed.Load())
	}
	if err := source.Close(context.Background()); err != nil || replacement.closed.Load() != 1 {
		t.Fatalf("replacement close err=%v closes=%d", err, replacement.closed.Load())
	}
	if err := source.Handle(context.Background(), func(context.Context, githubscaleset.Demand) error { return nil }); err == nil {
		t.Fatal("closed source accepted message ingestion")
	}
	if err := source.replace(context.Background(), replacement, false); err != nil {
		t.Fatalf("closed source replacement err=%v", err)
	}

	empty, _ := newRecoveringScaleSetSource(recoveringScaleSetConfig{source: &fakeSource{},
		open:    func(context.Context) (scaleSetSource, error) { return &fakeSource{}, nil },
		limiter: make(chan struct{}, 1)})
	empty.mu.Lock()
	empty.source = nil
	empty.mu.Unlock()
	if err := empty.Close(context.Background()); err != nil {
		t.Fatal(err)
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
			var recovery app.RecoveryObserver
			d.inventory = func(_ runtimeStore, _ config.Config, observer app.RecoveryObserver) app.Inventory {
				recovery = observer
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
			if recovery == nil {
				t.Fatalf("%s recovery observer was not wired", mode)
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
	d.inventory = func(runtimeStore, config.Config, app.RecoveryObserver) app.Inventory {
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
		{name: "nil scale", opts: options{Mode: reconcile.Shadow, ConfigPath: shadow, DatabasePath: filepath.Join(t.TempDir(), "x.db")}, mutate: func(d *dependencies) {
			d.newScaleSet = func(context.Context, githubscaleset.GitHubAppScaleSetConfig) (scaleSetSource, error) {
				return nil, nil
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

func TestProductionInventoryWiresEveryConfiguredHostGuard(t *testing.T) {
	cfg := config.Default()
	cfg.Guards = config.Guards{MinFreeDiskGiB: 70, MinAvailableMemoryMiB: 1536, MaxSwapUsedMiB: 3072, MaxLoadAverage: 8.5, MinCPUIdlePercent: 7.5}
	production, ok := defaultDependencies().inventory(nil, cfg, nil).(app.ProductionInventory)
	if !ok {
		t.Fatal("production inventory adapter type changed")
	}
	want := executor.Guardrails{MinFreeDiskGB: 70, MinAvailableMemoryMB: 1536, MaxSwapUsedMB: 3072, MaxLoadAverage: 8.5, MinCPUidlePercent: 7.5}
	if production.Guards != want {
		t.Fatalf("wired guards = %+v, want %+v", production.Guards, want)
	}
}

func TestProductionInventoryWiresPressureMemoryAccounting(t *testing.T) {
	cfg := config.Default()
	cfg.Guards.PressureMemoryAccounting = true
	production, ok := defaultDependencies().inventory(nil, cfg, nil).(app.ProductionInventory)
	if !ok {
		t.Fatal("production inventory adapter type changed")
	}
	probe, ok := production.Host.(*macos.Probe)
	if !ok {
		t.Fatal("host probe adapter type changed")
	}
	if !probe.PressureAccounting {
		t.Fatal("pressure memory accounting flag was not wired into the host probe")
	}

	cfg.Guards.PressureMemoryAccounting = false
	legacy := defaultDependencies().inventory(nil, cfg, nil).(app.ProductionInventory).Host.(*macos.Probe)
	if legacy.PressureAccounting {
		t.Fatal("legacy configuration enabled pressure accounting")
	}
}

func TestProductionVMControlWiresMacOSPerformanceOptions(t *testing.T) {
	cfg := config.Default()
	cfg.MacOS.RootDiskOptions = "sync=none"
	cfg.MacOS.SharedDirectoryPath = "/private/tmp/ci-shared"
	control, ok := defaultDependencies().newVM(nil, cfg, nil).(*tart.Adapter)
	if !ok {
		t.Fatal("production VM adapter type changed")
	}
	wantPrefixes := []string{"trf-builder-", "trf-maestro-"}
	if !reflect.DeepEqual(control.MacOSVMPrefixes, wantPrefixes) || control.MacOSRootDiskOptions != "sync=none" ||
		control.MacOSSharedDirectoryPath != "/private/tmp/ci-shared" {
		t.Fatalf("wired macOS performance options = %+v", control)
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

	// An actively-renewed incumbent is never stolen: AcquireLease keeps
	// reporting "lease held" for the whole acquisition window, so the successor
	// gives up with the original error rather than taking over. A fake clock
	// advances past the window instantly (no real sleeps, no real waiting).
	base := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	var clock atomic.Int64
	clock.Store(base.UnixNano())
	d.now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	d.after = func(time.Duration) <-chan time.Time {
		clock.Add(int64(authorityLeaseAcquireWindow)) // jump the whole window per retry
		ch := make(chan time.Time, 1)
		ch <- time.Time{}
		return ch
	}
	baseOpen := d.openStore
	d.openStore = func(ctx context.Context, path string) (runtimeStore, error) {
		store, err := baseOpen(ctx, path)
		if err != nil {
			return nil, err
		}
		return heldLeaseStore{runtimeStore: store}, nil
	}
	err := runWithDependencies(context.Background(), options{Mode: reconcile.Authority,
		ConfigPath: writeConfig(t, true), DatabasePath: filepath.Join(t.TempDir(), "held.db")}, d)
	if !errors.Is(err, operations.ErrLeaseHeld) {
		t.Fatalf("held authority err=%v", err)
	}
}

// heldLeaseStore simulates a live incumbent controller: every acquisition
// attempt reports the lease as held, so a successor must never take it over.
type heldLeaseStore struct{ runtimeStore }

func (heldLeaseStore) AcquireLease(context.Context, string, string, time.Time, time.Duration) (operations.Lease, error) {
	return operations.Lease{}, operations.ErrLeaseHeld
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

func TestRunJoinsStartupAndCleanupFailures(t *testing.T) {
	d := testDependencies(t)
	var first *closeErrorSource
	calls := 0
	d.newScaleSet = func(context.Context, githubscaleset.GitHubAppScaleSetConfig) (scaleSetSource, error) {
		calls++
		if calls == 1 {
			first = &closeErrorSource{}
			return first, nil
		}
		return nil, errors.New("open failed")
	}
	err := runWithDependencies(context.Background(), options{Mode: reconcile.Shadow, ConfigPath: writeConfig(t, true), DatabasePath: filepath.Join(t.TempDir(), "x.db")}, d)
	if err == nil || !strings.Contains(err.Error(), "open failed") || !strings.Contains(err.Error(), "scale-set session cleanup failed") || first.closed.Load() != 1 {
		t.Fatalf("joined startup cleanup error = %v, closed=%d", err, first.closed.Load())
	}
}

func TestCloseScaleSetSourcesHonorsCancellationWhileDispatching(t *testing.T) {
	started := make(chan struct{}, scaleSetCloseConcurrency)
	release := make(chan struct{})
	sources := make([]scaleSetSource, scaleSetCloseConcurrency+1)
	for i := range sources {
		sources[i] = &contextIgnoringCloseSource{started: started, release: release}
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- closeScaleSetSources(ctx, sources)
	}()
	for range scaleSetCloseConcurrency {
		<-started
	}
	cancel()
	err := <-result
	close(release)
	if !errors.Is(err, errScaleSetClose) {
		t.Fatalf("cancelled close = %v", err)
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
	if d.inventory(store, config.Default(), nil) == nil {
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
	// The blocked plan's bounded reason is surfaced verbatim on the scheduler
	// observation so an operator can see why ticks stopped advancing.
	if detail := health.Snapshot().Observations["scheduler"].Detail; detail != "host stale: stale" {
		t.Fatalf("scheduler detail=%q", detail)
	}
}

func TestFailureReporterLogsOncePerComponentPerWindow(t *testing.T) {
	var buffer bytes.Buffer
	now := time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC)
	reporter := newFailureReporter(&buffer, func() time.Time { return now })
	reporter.report("scheduler", "")
	reporter.report("scheduler", "") // suppressed inside the window
	reporter.report("ingest", githubscaleset.ReasonSessionExpired)
	if got := strings.Count(buffer.String(), "component loop failure"); got != 2 {
		t.Fatalf("in-window log lines = %d: %q", got, buffer.String())
	}
	if !strings.Contains(buffer.String(), "component=scheduler") || !strings.Contains(buffer.String(), "component=ingest") {
		t.Fatalf("component names missing: %q", buffer.String())
	}
	// Four hours of "component=ingest" told an operator nothing. A distinct
	// closed-vocabulary reason is logged and tracked separately.
	if !strings.Contains(buffer.String(), "reason="+githubscaleset.ReasonSessionExpired) {
		t.Fatalf("ingest reason missing: %q", buffer.String())
	}
	reporter.report("ingest", githubscaleset.ReasonRecreatedAfterFailures)
	if !strings.Contains(buffer.String(), "reason="+githubscaleset.ReasonRecreatedAfterFailures) {
		t.Fatalf("changed reason suppressed: %q", buffer.String())
	}
	reporter.report("ingest", githubscaleset.ReasonSessionExpired) // suppressed inside the window
	if got := strings.Count(buffer.String(), "reason="+githubscaleset.ReasonSessionExpired); got != 1 {
		t.Fatalf("repeated reason lines = %d: %q", got, buffer.String())
	}
	now = now.Add(2 * time.Minute)
	reporter.report("scheduler", "") // window elapsed, logs again
	if got := strings.Count(buffer.String(), "component=scheduler"); got != 2 {
		t.Fatalf("post-window scheduler lines = %d: %q", got, buffer.String())
	}
	// An unclassified failure keeps the historical single-attribute line.
	if strings.Contains(buffer.String(), "component=scheduler reason") {
		t.Fatalf("empty reason emitted an attribute: %q", buffer.String())
	}
	// A nil clock falls back to the wall clock and still emits.
	fallback := newFailureReporter(io.Discard, nil)
	fallback.report("operations", "")
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
	ticker.recordMetrics(app.TickResult{HostMode: domain.HostLinux, Host: domain.Host{Pressure: domain.HostPressure{
		AvailableMemoryMB: 8192, FreeDiskGB: 200, CPUIdlePercent: 50, LoadAverage: 3,
		AdmissionAllowed: true, AdmissionReason: "capacity available",
	}}, Demands: []domain.Demand{
		{Profile: "small", CreatedAt: now.Add(-time.Minute)}, {Profile: "small", CreatedAt: now.Add(-2 * time.Minute)},
	}, Queues: map[domain.ProfileID]app.QueueSummary{
		"maestro": {Count: 3, Oldest: now.Add(-3 * time.Minute)},
	}, Instances: []domain.Instance{
		{Profile: "small", State: domain.InstanceRunning, Resources: domain.Resources{CPU: 2, MemoryMB: 4096}},
		{Profile: "maestro", State: domain.InstanceDeleted, Resources: domain.Resources{CPU: 4, MemoryMB: 7168}},
	}})
	snapshot := health.Snapshot()
	if snapshot.Mode != telemetry.ModeLinux || snapshot.Queues["small"].Count != 2 || snapshot.Queues["small"].OldestEnqueuedAt != now.Add(-2*time.Minute) ||
		snapshot.Queues["maestro"].Count != 3 || snapshot.Queues["maestro"].OldestEnqueuedAt != now.Add(-3*time.Minute) || snapshot.Instances["small"].CPU != 2 || snapshot.Instances["maestro"].Count != 0 ||
		snapshot.HostPressure.FreeDiskGiB != 200 || !snapshot.HostPressure.AdmissionAllowed {
		t.Fatalf("snapshot=%#v", snapshot)
	}
	ticker.recordMetrics(app.TickResult{HostMode: domain.HostMacOS})
	if health.Snapshot().Mode != telemetry.ModeMacOS {
		t.Fatalf("mode=%s", health.Snapshot().Mode)
	}
	ticker.recordMetrics(app.TickResult{HostMode: domain.HostMixed})
	if health.Snapshot().Mode != telemetry.ModeMixed {
		t.Fatalf("mixed mode=%s", health.Snapshot().Mode)
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
	alreadyLabelled := []app.Binding{{StoreKey: 8, ScaleSetID: 8, Scope: "scope", RequiredLabels: []string{canaryDemandLabel}, Profile: domain.Profile{ID: "small"}}}
	selected, err = selectRuntimeBindings(alreadyLabelled, options{Mode: reconcile.Canary, CanaryScope: "scope", CanaryProfile: "small"})
	if err != nil || len(selected) != 1 || len(selected[0].RequiredLabels) != 1 {
		t.Fatalf("existing canary label duplicated: %#v, %v", selected, err)
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
	d.inventory = func(runtimeStore, config.Config, app.RecoveryObserver) app.Inventory {
		return notifyingInventory{ready: ready, once: &once}
	}
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
	probe := execReadiness{Runner: recorder, Timeout: time.Second, AttemptTimeout: 100 * time.Millisecond, RetryInterval: time.Millisecond}
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
	if err := (execReadiness{Runner: &recordingTartRunner{}, Timeout: time.Second}).Wait(context.Background(), operations.Instance{ID: "bad name"}); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("invalid name err=%v", err)
	}
}

func TestTartReadinessDefaultsRemainBounded(t *testing.T) {
	recorder := &recordingTartRunner{fails: 100}
	probe := execReadiness{Runner: recorder, Timeout: 2 * time.Millisecond}
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

func TestProvisionLifecycleNeverDeadLettersOwnedVMs(t *testing.T) {
	if lifecycleRetryMaxAttempts != 0 {
		t.Fatalf("provision retry max attempts = %d, want durable retry", lifecycleRetryMaxAttempts)
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

// stubLeaseStore drives acquireAuthorityLease directly with a fixed AcquireLease
// outcome; the embedded nil Store is never touched because only AcquireLease is
// exercised by the acquisition loop.
type stubLeaseStore struct {
	operations.Store
	err error
}

func (s stubLeaseStore) AcquireLease(context.Context, string, string, time.Time, time.Duration) (operations.Lease, error) {
	return operations.Lease{}, s.err
}

// TestAcquireAuthorityLeaseWaitsOutStalePredecessor is the headline regression:
// a predecessor left a durable lease row that expires mid-window (it died
// without releasing). The old single-shot acquisition returned "lease held" and
// the daemon exited silently; the retry now waits out the stale lease and
// acquires it the moment it expires — never before (that would be theft).
func TestAcquireAuthorityLeaseWaitsOutStalePredecessor(t *testing.T) {
	d := testDependencies(t)
	store, err := d.openStore(context.Background(), filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	base := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	if _, err := store.AcquireLease(context.Background(), authorityLeaseName, "predecessor", base, authorityLeaseTTL); err != nil {
		t.Fatal(err)
	}
	var clock atomic.Int64
	clock.Store(base.UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	attempts := 0
	after := func(time.Duration) <-chan time.Time {
		attempts++
		clock.Store(base.Add(time.Duration(attempts) * authorityLeaseAcquireRetry).UnixNano())
		ch := make(chan time.Time, 1)
		ch <- time.Time{}
		return ch
	}
	lease, err := acquireAuthorityLease(context.Background(), store, "successor", now, after)
	if err != nil {
		t.Fatalf("successor failed to wait out stale predecessor lease: %v", err)
	}
	if attempts == 0 {
		t.Fatal("successor acquired without retrying a held lease")
	}
	acquiredAt := lease.ExpiresAt.Add(-authorityLeaseTTL)
	if acquiredAt.Before(base.Add(authorityLeaseTTL)) {
		t.Fatalf("acquired at %s before predecessor expiry %s: stole a live lease", acquiredAt, base.Add(authorityLeaseTTL))
	}
}

// TestAcquireAuthorityLeasePropagatesNonHeldError proves the retry only swallows
// ErrLeaseHeld; any other store error surfaces immediately without waiting.
func TestAcquireAuthorityLeasePropagatesNonHeldError(t *testing.T) {
	boom := errors.New("disk gone")
	now := func() time.Time { return time.Unix(0, 0).UTC() }
	_, err := acquireAuthorityLease(context.Background(), stubLeaseStore{err: boom}, "owner", now,
		func(time.Duration) <-chan time.Time { return make(chan time.Time) })
	if !errors.Is(err, boom) {
		t.Fatalf("non-held error not propagated: %v", err)
	}
}

// TestAcquireAuthorityLeaseHonorsContextCancellation proves a shutdown during
// the acquisition wait exits promptly with the context error, never blocking.
func TestAcquireAuthorityLeaseHonorsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	now := func() time.Time { return time.Unix(0, 0).UTC() } // never advances: always inside the window
	_, err := acquireAuthorityLease(ctx, stubLeaseStore{err: operations.ErrLeaseHeld}, "owner", now,
		func(time.Duration) <-chan time.Time {
			cancel()
			return make(chan time.Time)
		})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation not honored: %v", err)
	}
}

// TestRunAuthorityReleasesLeaseOnShutdown proves the graceful-shutdown release:
// a SIGTERM-equivalent context cancel makes the daemon drop its lease, so an
// immediate successor acquires without waiting out the TTL.
func TestRunAuthorityReleasesLeaseOnShutdown(t *testing.T) {
	d := testDependencies(t)
	ready := make(chan struct{})
	var once sync.Once
	d.inventory = func(_ runtimeStore, _ config.Config, _ app.RecoveryObserver) app.Inventory {
		return notifyingInventory{ready: ready, once: &once}
	}
	d.newScaleSet = func(context.Context, githubscaleset.GitHubAppScaleSetConfig) (scaleSetSource, error) {
		return &fakeSource{}, nil
	}
	database := filepath.Join(t.TempDir(), "fleet.db")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runWithDependencies(ctx, options{Mode: reconcile.Authority, ConfigPath: writeConfig(t, true),
			DatabasePath: database, HealthAddress: "127.0.0.1:0"}, d)
	}()
	select {
	case <-ready:
		cancel()
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("authority controller did not become ready")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("graceful shutdown err=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("authority controller did not stop after cancellation")
	}
	store, err := d.openStore(context.Background(), database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.AcquireLease(context.Background(), authorityLeaseName, "successor", time.Now().UTC(), authorityLeaseTTL); err != nil {
		t.Fatalf("successor could not acquire immediately after graceful shutdown: %v", err)
	}
}

// renewFailRuntimeStore fences the daemon out of its own lease. ErrLeaseLost is
// the durable proof that authority moved — the row is gone or another owner holds
// it — so the guard must surrender immediately rather than spend the remaining
// TTL on grace retries that can never succeed.
type renewFailRuntimeStore struct{ runtimeStore }

func (renewFailRuntimeStore) RenewLease(context.Context, operations.Lease, time.Time, time.Duration) (operations.Lease, error) {
	return operations.Lease{}, operations.ErrLeaseLost
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

// renewalStore drives the authority guard's renewal loop directly. AcquireLease
// hands out a lease with a caller-chosen expiry and RenewLease replays a fixed
// script of outcomes, so a transient failure can be told apart from a fencing
// loss without a real database and without real elapsed time.
type renewalStore struct {
	operations.Store
	expires  time.Time
	outcomes []error

	mu       sync.Mutex
	attempts int
}

func (s *renewalStore) AcquireLease(_ context.Context, name, owner string, _ time.Time, _ time.Duration) (operations.Lease, error) {
	return operations.Lease{Name: name, Owner: owner, Token: 1, ExpiresAt: s.expires}, nil
}

func (*renewalStore) RecoverExpired(context.Context, time.Time) (int64, error) { return 0, nil }
func (*renewalStore) ReleaseLease(context.Context, operations.Lease) error     { return nil }

func (s *renewalStore) RenewLease(ctx context.Context, lease operations.Lease, now time.Time, ttl time.Duration) (operations.Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if deadline, ok := ctx.Deadline(); !ok || !deadline.After(time.Now()) {
		return operations.Lease{}, errors.New("renewal attempt must carry a live deadline")
	}
	outcome := s.outcomes[min(s.attempts, len(s.outcomes)-1)]
	s.attempts++
	if outcome != nil {
		return operations.Lease{}, outcome
	}
	lease.ExpiresAt = now.Add(ttl)
	return lease, nil
}

func (s *renewalStore) renewals() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts
}

// leaseGuardHarness starts an authority guard on a scripted store with a clock
// and a renewal ticker the test drives, and reports the delay the guard asked to
// wait before each attempt.
type leaseGuardHarness struct {
	guard  *authorityLease
	clock  *atomic.Int64
	ticks  chan time.Time
	delays chan time.Duration
}

func startLeaseGuard(t *testing.T, store *renewalStore, base time.Time) leaseGuardHarness {
	t.Helper()
	var clock atomic.Int64
	clock.Store(base.UnixNano())
	harness := leaseGuardHarness{clock: &clock, ticks: make(chan time.Time),
		delays: make(chan time.Duration, 16)}
	guard, err := startAuthorityLease(context.Background(), store, "authority",
		func() time.Time { return time.Unix(0, clock.Load()).UTC() },
		func(delay time.Duration) <-chan time.Time {
			harness.delays <- delay
			return harness.ticks
		})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(guard.Close)
	harness.guard = guard
	return harness
}

func (h leaseGuardHarness) nextDelay(t *testing.T) time.Duration {
	t.Helper()
	select {
	case delay := <-h.delays:
		return delay
	case <-time.After(2 * time.Second):
		t.Fatal("renewal loop did not schedule its next attempt")
		return 0
	}
}

func (h leaseGuardHarness) failure(t *testing.T) error {
	t.Helper()
	select {
	case err := <-h.guard.errors:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("authority loss was not reported")
		return nil
	}
}

// TestAuthorityLeaseRetriesTransientRenewalInsideItsValidity is the regression
// case for the recurring 2026-08-01 crash. The authority daemon exited three
// times in seventy minutes with
//
//	fleet daemon failed: controller authority lost: renew lease: context deadline exceeded
//
// while the host was saturated (load 26, CPU idle 1.6%). The lease TTL is thirty
// seconds and renewal runs every ten, so at the moment of every one of those
// exits the durable lease was still held for roughly twenty more seconds — the
// daemon inferred a durable loss from a five-second I/O timeout on a SQLite
// connection it shares with the plan commit and the operations worker. Each exit
// cost a launchd restart plus the successor's wait for the abandoned lease to
// drain, which is exactly the scheduling gap the queue was already suffering.
//
// A renewal that fails to complete must be retried inside the lease's own
// validity, and a later success must return the loop to its ordinary cadence.
func TestAuthorityLeaseRetriesTransientRenewalInsideItsValidity(t *testing.T) {
	base := time.Date(2026, 8, 1, 18, 15, 0, 0, time.UTC)
	store := &renewalStore{expires: base.Add(authorityLeaseTTL),
		outcomes: []error{context.DeadlineExceeded, context.DeadlineExceeded, nil}}
	harness := startLeaseGuard(t, store, base)

	if got := harness.nextDelay(t); got != authorityLeaseRenewInterval {
		t.Fatalf("first renewal scheduled after %v, want %v", got, authorityLeaseRenewInterval)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		harness.ticks <- time.Time{}
		if got := harness.nextDelay(t); got != authorityLeaseRenewRetry {
			t.Fatalf("grace retry %d scheduled after %v, want %v", attempt, got, authorityLeaseRenewRetry)
		}
		select {
		case err := <-harness.guard.errors:
			t.Fatalf("transient renewal failure surrendered authority: %v", err)
		default:
		}
	}

	harness.clock.Store(base.Add(5 * time.Second).UnixNano())
	harness.ticks <- time.Time{}
	if got := harness.nextDelay(t); got != authorityLeaseRenewInterval {
		t.Fatalf("recovered renewal scheduled after %v, want %v", got, authorityLeaseRenewInterval)
	}
	if got := store.renewals(); got != 3 {
		t.Fatalf("renewal attempts = %d, want 3", got)
	}
	harness.guard.mu.Lock()
	expiry := harness.guard.lease.ExpiresAt
	harness.guard.mu.Unlock()
	if want := base.Add(5 * time.Second).Add(authorityLeaseTTL); !expiry.Equal(want) {
		t.Fatalf("renewed expiry = %v, want %v", expiry, want)
	}
}

// TestAuthorityLeaseSurrendersWhenItCanNoLongerBeProvenHeld pins the fail-closed
// half of the grace retry. Patience is bounded by the durable lease itself, never
// by a retry budget: once no further attempt can complete before ExpiresAt, and
// once the lease has already expired, the guard reports the loss so a successor
// may legitimately take over.
func TestAuthorityLeaseSurrendersWhenItCanNoLongerBeProvenHeld(t *testing.T) {
	base := time.Date(2026, 8, 1, 18, 15, 0, 0, time.UTC)

	near := &renewalStore{expires: base.Add(authorityLeaseTTL), outcomes: []error{context.DeadlineExceeded}}
	harness := startLeaseGuard(t, near, base)
	harness.nextDelay(t)
	// One second of validity remains: the attempt is still made, but no retry
	// could land before expiry, so the failure is final.
	harness.clock.Store(base.Add(authorityLeaseTTL - time.Second).UnixNano())
	harness.ticks <- time.Time{}
	if err := harness.failure(t); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("surrender err=%v, want the transient failure that ended the retries", err)
	}
	if got := near.renewals(); got != 1 {
		t.Fatalf("renewal attempts = %d, want 1", got)
	}

	expired := &renewalStore{expires: base.Add(authorityLeaseTTL), outcomes: []error{nil}}
	harness = startLeaseGuard(t, expired, base)
	harness.nextDelay(t)
	harness.clock.Store(base.Add(authorityLeaseTTL).UnixNano())
	harness.ticks <- time.Time{}
	if err := harness.failure(t); !errors.Is(err, operations.ErrLeaseLost) {
		t.Fatalf("expired lease err=%v, want %v", err, operations.ErrLeaseLost)
	}
	if got := expired.renewals(); got != 0 {
		t.Fatalf("an expired lease attempted %d renewals, want none", got)
	}
}

// TestAuthorityLeaseFencingLossIsNeverRetried proves the grace retry cannot delay
// a real handover. ErrLeaseLost and ErrInvalid are durable proof rather than an
// I/O outcome, so they surrender on the first attempt even with the whole TTL
// still ahead.
func TestAuthorityLeaseFencingLossIsNeverRetried(t *testing.T) {
	base := time.Date(2026, 8, 1, 18, 15, 0, 0, time.UTC)
	for _, fencing := range []error{operations.ErrLeaseLost, operations.ErrInvalid} {
		store := &renewalStore{expires: base.Add(authorityLeaseTTL), outcomes: []error{fencing}}
		harness := startLeaseGuard(t, store, base)
		harness.nextDelay(t)
		harness.ticks <- time.Time{}
		if err := harness.failure(t); !errors.Is(err, fencing) {
			t.Fatalf("fencing err=%v, want %v", err, fencing)
		}
		if got := store.renewals(); got != 1 {
			t.Fatalf("fencing loss attempted %d renewals, want 1", got)
		}
	}
}

// TestRenewBudgetNeverOutlivesTheLease proves an in-flight attempt cannot keep
// this process acting on authority past the expiry a successor is entitled to
// acquire at.
func TestRenewBudgetNeverOutlivesTheLease(t *testing.T) {
	base := time.Date(2026, 8, 1, 18, 15, 0, 0, time.UTC)
	for _, testCase := range []struct {
		remaining time.Duration
		want      time.Duration
	}{
		{remaining: authorityLeaseTTL, want: authorityLeaseRenewTimeout},
		{remaining: authorityLeaseRenewTimeout, want: authorityLeaseRenewTimeout},
		{remaining: time.Second, want: time.Second},
		{remaining: 0, want: 0},
		{remaining: -time.Second, want: -time.Second},
	} {
		if got := renewBudget(base, base.Add(testCase.remaining)); got != testCase.want {
			t.Fatalf("renewBudget with %v remaining = %v, want %v", testCase.remaining, got, testCase.want)
		}
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

func (v *lifecycleVM) Create(context.Context, executor.InstanceSpec) error {
	v.clone.Add(1)
	return nil
}
func (v *lifecycleVM) Start(context.Context, string, operations.Ownership) error {
	v.start.Add(1)
	return nil
}
func (*lifecycleVM) Stop(context.Context, string, operations.Ownership) error   { return nil }
func (*lifecycleVM) Delete(context.Context, string, operations.Ownership) error { return nil }
func (*lifecycleVM) Running(context.Context, string) (bool, error)              { return false, nil }

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
	d.inventory = func(runtimeStore, config.Config, app.RecoveryObserver) app.Inventory {
		return notifyingInventory{ready: ready, once: &once}
	}
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

// TestFailureReporterCountsEveryOccurrenceItDoesNotLog is the pairing that made
// the 2026-08-02 wedge look smaller than it was: the log emits at most one line
// per component and reason per minute, so a loop failing every tick left eight
// lines standing for roughly a hundred failures. The counter must see all of
// them, including the ones the rate limit suppresses.
func TestFailureReporterCountsEveryOccurrenceItDoesNotLog(t *testing.T) {
	var logged bytes.Buffer
	clock := time.Unix(80_000, 0).UTC()
	counter := &recordingFailureCounter{}
	reporter := newFailureReporter(&logged, func() time.Time { return clock })
	reporter.counter = counter

	for range 6 {
		reporter.report("scheduler", "plan_commit_failed")
	}
	reporter.report("ingest", "")

	if got := counter.calls; len(got) != 7 {
		t.Fatalf("counted %d failures, want every one: %#v", len(got), got)
	}
	if got := strings.Count(logged.String(), "component=scheduler"); got != 1 {
		t.Fatalf("logged scheduler lines = %d, want the rate limit to hold at 1", got)
	}
	if counter.calls[6] != (failureCall{component: "ingest"}) {
		t.Fatalf("unclassified failure = %#v; the counter, not the reporter, names the gap", counter.calls[6])
	}
}

// TestFailureReporterWithoutACounterStillLogs keeps the metric optional so the
// reporter can never become a reason a failure goes unreported.
func TestFailureReporterWithoutACounterStillLogs(t *testing.T) {
	var logged bytes.Buffer
	reporter := newFailureReporter(&logged, func() time.Time { return time.Unix(90_000, 0).UTC() })

	reporter.report("scheduler", "plan_commit_rejected")

	if !strings.Contains(logged.String(), "plan_commit_rejected") {
		t.Fatalf("log = %q", logged.String())
	}
}

type failureCall struct{ component, reason string }

type recordingFailureCounter struct{ calls []failureCall }

func (r *recordingFailureCounter) RecordComponentFailure(component, reason string) {
	r.calls = append(r.calls, failureCall{component: component, reason: reason})
}
