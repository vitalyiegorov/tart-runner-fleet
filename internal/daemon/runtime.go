package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/sqlite"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/autoupdate"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/credentials"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/discharge"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/executor"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/lifecycle"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/reconcile"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/telemetry"
)

type runtimeStore interface {
	operations.Store
	app.EngineStore
	app.LiveInstanceStore
	lifecycle.StateStore
	lifecycle.DemandReader
	DemandCursor(context.Context, int64) (int64, error)
	OperationCounts(context.Context) (int, int, error)
	OperationFailures(context.Context) ([]operations.OperationFailure, error)
	operations.DeadLetterStore
	operations.StalledOperationStore
	Close() error
}

const canaryDemandLabel = "tart-fleet-canary"

type scaleSetSource interface {
	app.MessageSource
	lifecycle.ScaleSetControl
	GenerateJIT(context.Context, string, string) (*githubscaleset.JITSecret, error)
	Close(context.Context) error
}

type scaleSetSourceFactory func(context.Context) (scaleSetSource, error)

// recoveringScaleSetSource keeps the message-session and lifecycle-control
// views on one atomically replaceable official client. A failed long poll is a
// session failure, not a reason to reuse the same broken session forever.
type recoveringScaleSetSource struct {
	mu      sync.RWMutex
	source  scaleSetSource
	open    scaleSetSourceFactory
	limiter chan struct{}
	closed  bool
	// suspended is a session this node gave up on purpose, not one that broke.
	// A suspended source holds no session at all, which is what makes GitHub
	// stop binding new jobs to the scale set behind it (issue #297).
	suspended bool
	// released records that the broker no longer owns the current session, so a
	// later replacement must not try to delete it again. Without this, a
	// successful release followed by a failed open pins the binding to a session
	// whose second deletion can only fail.
	released bool
	policy   githubscaleset.SessionRecoveryPolicy
	failures githubscaleset.SessionFailureState
	now      func() time.Time
}

type recoveringScaleSetConfig struct {
	source  scaleSetSource
	open    scaleSetSourceFactory
	limiter chan struct{}
	policy  githubscaleset.SessionRecoveryPolicy
	now     func() time.Time
}

func newRecoveringScaleSetSource(c recoveringScaleSetConfig) (*recoveringScaleSetSource, error) {
	if c.source == nil || c.open == nil || c.limiter == nil {
		return nil, operations.ErrInvalid
	}
	if c.now == nil {
		c.now = time.Now
	}
	return &recoveringScaleSetSource{source: c.source, open: c.open, limiter: c.limiter,
		policy: c.policy, now: c.now}, nil
}

func (s *recoveringScaleSetSource) acquire() (scaleSetSource, func(), error) {
	s.mu.RLock()
	if s.closed || s.source == nil {
		s.mu.RUnlock()
		return nil, func() {}, operations.ErrInvalid
	}
	return s.source, s.mu.RUnlock, nil
}

func (s *recoveringScaleSetSource) Handle(ctx context.Context, commit func(context.Context, githubscaleset.Demand) error) error {
	source, release, err := s.acquire()
	if err != nil {
		return err
	}
	err = source.Handle(ctx, commit)
	release()
	if err == nil {
		s.clearFailures()
		return nil
	}
	if ctx.Err() != nil {
		return err
	}
	// The concrete broker failure never reaches a log line. It travels wrapped
	// inside a closed-vocabulary reason so an operator can see why ingestion
	// failed through the admin API without any upstream text being rendered.
	decision := s.recordFailure(err)
	reason := replacementReason(s.replace(ctx, source, decision.Discard), decision.Reason)
	return &githubscaleset.SessionFailure{Reason: reason, Cause: err}
}

// replacementReason keeps the recovery outcome inside the closed vocabulary.
func replacementReason(err error, classified string) string {
	switch {
	case errors.Is(err, errScaleSetClose):
		return githubscaleset.ReasonSessionReleaseFailed
	case errors.Is(err, errScaleSetOpen):
		return githubscaleset.ReasonSessionCreateFailed
	default:
		return classified
	}
}

func (s *recoveringScaleSetSource) recordFailure(err error) githubscaleset.SessionRecoveryDecision {
	s.mu.Lock()
	defer s.mu.Unlock()
	decision := s.policy.OnFailure(s.failures, err, s.now())
	s.failures = decision.State
	return decision
}

func (s *recoveringScaleSetSource) clearFailures() {
	s.mu.Lock()
	s.failures = githubscaleset.SessionFailureState{}
	s.mu.Unlock()
}

// replace discards the failed session and installs a replacement. discard means
// the failure is terminal or has exceeded its bounded escalation, so a broker
// that refuses to release the session must not keep the binding pinned to it.
func (s *recoveringScaleSetSource) replace(ctx context.Context, failed scaleSetSource, discard bool) error {
	select {
	case s.limiter <- struct{}{}:
		defer func() { <-s.limiter }()
	case <-ctx.Done():
		return ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.source != failed {
		return nil
	}
	if !s.released {
		closeCtx, cancel := context.WithTimeout(ctx, scaleSetCloseTimeout)
		closeErr := failed.Close(closeCtx)
		cancel()
		if closeErr != nil && !discard {
			return errScaleSetClose
		}
		s.released = true
	}
	replacement, err := s.open(ctx)
	if err != nil {
		return errScaleSetOpen
	}
	s.source = replacement
	s.released = false
	s.failures = githubscaleset.SessionFailureState{}
	return nil
}

func (s *recoveringScaleSetSource) Registered(ctx context.Context, name string) (bool, error) {
	source, release, err := s.acquire()
	if err != nil {
		return false, err
	}
	defer release()
	return source.Registered(ctx, name)
}

func (s *recoveringScaleSetSource) AcquireAndGenerateJIT(ctx context.Context, requestID int64, name, workFolder string) (*githubscaleset.JITSecret, error) {
	source, release, err := s.acquire()
	if err != nil {
		return nil, err
	}
	defer release()
	return source.AcquireAndGenerateJIT(ctx, requestID, name, workFolder)
}

func (s *recoveringScaleSetSource) GenerateJIT(ctx context.Context, name, workFolder string) (*githubscaleset.JITSecret, error) {
	source, release, err := s.acquire()
	if err != nil {
		return nil, err
	}
	defer release()
	return source.GenerateJIT(ctx, name, workFolder)
}

func (s *recoveringScaleSetSource) Deregister(ctx context.Context, name string) error {
	source, release, err := s.acquire()
	if err != nil {
		return err
	}
	defer release()
	return source.Deregister(ctx, name)
}

// Suspend releases this binding's session without ending the source. It is the
// difference between a node that is idle and a node that has stopped competing
// for work it cannot run: the scale set survives, and GitHub binds its next job
// to a sibling advertising the same labels instead.
//
// A close GitHub refuses is reported rather than assumed: the caller retries,
// because a session this node believes it dropped while the broker still holds
// it is exactly the state that would strand the jobs this exists to free.
func (s *recoveringScaleSetSource) Suspend(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.suspended {
		return nil
	}
	if s.source != nil && !s.released {
		closeCtx, cancel := context.WithTimeout(ctx, scaleSetCloseTimeout)
		err := s.source.Close(closeCtx)
		cancel()
		if err != nil {
			return err
		}
		s.released = true
	}
	s.suspended = true
	s.source = nil
	return nil
}

// Resume opens a fresh session through the same factory the failure path uses,
// so rejoining the fleet and recovering from a broken session are one mechanism
// with one set of conflict semantics.
func (s *recoveringScaleSetSource) Resume(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || !s.suspended {
		return nil
	}
	replacement, err := s.open(ctx)
	if err != nil {
		return errScaleSetOpen
	}
	s.source = replacement
	s.released = false
	s.suspended = false
	s.failures = githubscaleset.SessionFailureState{}
	return nil
}

// Suspended reports whether this binding is currently withdrawn.
func (s *recoveringScaleSetSource) Suspended() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.suspended
}

func (s *recoveringScaleSetSource) Close(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.source == nil || s.released {
		return nil
	}
	return s.source.Close(ctx)
}

type dependencies struct {
	// executes mirrors platform.executes: whether this node's backend can bring
	// an instance into existence at all. It is a question asked of the decoded
	// configuration, because on Linux the answer is a configured backend rather
	// than a kernel (ADR 0034, issue #139).
	executes func(config.Config) bool
	// preflight mirrors platform.preflight: the fail-closed check that a
	// configured execution technology is actually usable on this machine.
	preflight       func(context.Context, config.Config) error
	linuxImage      func(config.Config) string
	openConfig      func(string) (io.ReadCloser, error)
	openStore       func(context.Context, string) (runtimeStore, error)
	loadKey         func(context.Context, string, string, string) (*credentials.Secret, error)
	newScaleSet     func(context.Context, githubscaleset.GitHubAppScaleSetConfig) (scaleSetSource, error)
	newRESTObserver func(githubscaleset.ObserverConfig) (queueObserver, error)
	inventory       func(runtimeStore, config.Config, app.RecoveryObserver, *updateDrainController) app.Inventory
	listen          func(string, string) (net.Listener, error)
	adminListen     func(string) (net.Listener, error)
	cursor          func(context.Context, runtimeStore, int64) (int64, error)
	newVM           func(runtimeStore, config.Config, lifecycle.DrainControl) lifecycle.VMControl
	newReaper       func(runtimeStore, config.Config) discharge.VM
	readiness       func(config.Config) lifecycle.Readiness
	bootstrap       func(config.Config) lifecycle.Bootstrapper
	// guestProbe is the fresh re-verification a guest-liveness drain performs at
	// the moment it acts. It is the same probe the inventory runs each tick; the
	// drain holds its own so a standing kill order re-derives its premise from
	// ground truth rather than trusting the observation that planned it.
	guestProbe func(config.Config) app.GuestProbe
	now        func() time.Time
	after      func(time.Duration) <-chan time.Time
	leaseOwner func(config.Config) string
}

var deps = defaultDependencies()

const (
	legacyScopeName             = "legacy"
	authorityLeaseName          = "tart-runner-fleet-authority"
	authorityLeaseTTL           = 30 * time.Second
	authorityLeaseRenewInterval = 10 * time.Second
	// authorityLeaseAcquireWindow bounds how long a starting controller waits
	// for a stale predecessor lease to drain before giving up. A restart within
	// the previous daemon's TTL leaves its durable lease row behind; rather than
	// exit silently on "lease held" (launchd has no operator watching the one
	// stderr line), the successor waits it out. 1.5×TTL guarantees a full TTL of
	// patience even when we start the instant after the predecessor's last renew.
	authorityLeaseAcquireWindow = authorityLeaseTTL * 3 / 2
	// authorityLeaseAcquireRetry paces reacquisition attempts inside the window.
	authorityLeaseAcquireRetry = 1 * time.Second
	// authorityLeaseRenewTimeout bounds one renewal attempt. It is deliberately
	// far shorter than the TTL so a wedged attempt leaves room for grace retries
	// inside the same lease.
	authorityLeaseRenewTimeout = 5 * time.Second
	// authorityLeaseRenewRetry paces grace retries after a transient renewal
	// failure. Two seconds inside a thirty-second TTL leaves roughly nine
	// attempts after the first scheduled renewal, which is far more resilience
	// than the single attempt that has been ending the process.
	authorityLeaseRenewRetry   = 2 * time.Second
	deletionConfirmationMaxAge = 30 * time.Second
	// containerStopGrace is how long podman lets a runner container's process
	// finish before it is killed. It is deliberately a constant rather than
	// Timeouts.Tart: the grace is spent *inside* the `podman stop` this adapter
	// runs under that timeout, so deriving one from the other would let a stop
	// that used its whole grace be cancelled at the exact moment it succeeded.
	containerStopGrace        = 10 * time.Second
	scaleSetCloseTimeout      = 20 * time.Second
	scaleSetCloseConcurrency  = 4
	lifecycleRetryMaxAttempts = 0
	provisionRetryMaximum     = 30 * time.Second
	drainRetryMaximum         = 30 * time.Second
)

var (
	errScaleSetClose = errors.New("scale-set session cleanup failed")
	errScaleSetOpen  = errors.New("recreate scale-set session failed")
)

func defaultDependencies() dependencies { return newDependencies(runtime.GOOS) }

// newDependencies is the production wiring for a node running goos. Everything
// a node's execution technology decides comes from platformFor; everything else
// is the same on every machine.
func newDependencies(goos string) dependencies {
	node := platformFor(goos)
	return dependencies{
		executes:   node.executes,
		preflight:  node.preflight,
		linuxImage: node.linuxImage,
		// #nosec G304 -- the config path is an explicit operator-controlled CLI input.
		openConfig: func(path string) (io.ReadCloser, error) { return os.Open(path) },
		openStore:  func(ctx context.Context, path string) (runtimeStore, error) { return sqlite.Open(ctx, path) },
		loadKey: func(ctx context.Context, service, account, path string) (*credentials.Secret, error) {
			return (credentials.GitHubAppKey{}).Load(ctx, service, account, path)
		},
		newScaleSet: func(ctx context.Context, cfg githubscaleset.GitHubAppScaleSetConfig) (scaleSetSource, error) {
			return githubscaleset.NewGitHubAppScaleSet(ctx, cfg)
		},
		newRESTObserver: func(cfg githubscaleset.ObserverConfig) (queueObserver, error) {
			return githubscaleset.NewObserver(cfg)
		},
		inventory: func(store runtimeStore, cfg config.Config, recovery app.RecoveryObserver, drain *updateDrainController) app.Inventory {
			return app.ProductionInventory{Store: store, UpdateDrain: drain.Draining,
				Executor:                   node.executor(cfg),
				Host:                       node.host(cfg),
				Recovery:                   recovery,
				RecoveryConfirmationMaxAge: deletionConfirmationMaxAge,
				Capacity:                   domain.Resources{CPU: cfg.Linux.Capacity.CPU, MemoryMB: cfg.Linux.Capacity.MemoryMiB, Slots: cfg.Linux.MaxInstances},
				Guards: executor.Guardrails{MinFreeDiskGB: int64(cfg.Guards.MinFreeDiskGiB),
					MinAvailableMemoryMB: int64(cfg.Guards.MinAvailableMemoryMiB), MaxSwapUsedMB: int64(cfg.Guards.MaxSwapUsedMiB),
					MaxLoadAverage: cfg.Guards.MaxLoadAverage, MinCPUidlePercent: cfg.Guards.MinCPUIdlePercent},
				ElasticHostEnvelope: cfg.Guards.ElasticHostEnvelope,
				HostBudget:          domain.Resources{CPU: cfg.HostBudget.CPU, MemoryMB: cfg.HostBudget.MemoryMiB},
				Guest:               guestLivenessTracker(node, cfg, time.Now),
				// Unconditional, unlike the guest tracker: the bound it enforces is not a
				// configured mechanism but the corroboration every other destructive premise
				// in this fleet already required, and this one did not (issue #246).
				// The clock is stated here for the same reason the guest tracker's is:
				// the accumulator judges its own instants, and a bound stamped on one
				// clock and judged against another silently never fires (issue #247).
				Power: &app.PowerCorroborator{Now: time.Now}}
		},
		listen:      net.Listen,
		adminListen: adminapi.Listen,
		cursor: func(ctx context.Context, store runtimeStore, id int64) (int64, error) {
			return store.DemandCursor(ctx, id)
		},
		newVM:      node.newVM,
		newReaper:  node.newReaper,
		readiness:  node.readiness,
		bootstrap:  node.bootstrap,
		guestProbe: node.guestProbe,
		now:        time.Now,
		after:      time.After,
		leaseOwner: func(cfg config.Config) string {
			owner := cfg.GitHub.SessionOwner
			if owner == "" {
				owner = cfg.GitHub.Owner
			}
			return fmt.Sprintf("%s/%d", owner, os.Getpid())
		},
	}
}

// guestLivenessTracker builds this node's probe accumulator, or nil when either
// half of the mechanism is absent: a configuration that states no bound, or a
// backend with no guest to ask. Both are fail-open by construction — a nil
// tracker probes nothing, so no instance can ever be declared dead by a node
// that is not measuring.
func guestLivenessTracker(node platform, cfg config.Config, now func() time.Time) *app.GuestLivenessTracker {
	if !cfg.GuestLiveness.Enabled() || node.guestProbe == nil {
		return nil
	}
	probe := node.guestProbe(cfg)
	if probe == nil {
		return nil
	}
	return &app.GuestLivenessTracker{Probe: probe, Now: now,
		Policy: domain.GuestLivenessPolicy{ConsecutiveRefusals: cfg.GuestLiveness.ConsecutiveRefusals,
			Window: cfg.GuestLiveness.Window}}
}

func runDaemon(ctx context.Context, opts options) error { return runWithDependencies(ctx, opts, deps) }

func runWithDependencies(ctx context.Context, opts options, d dependencies) (retErr error) {
	if !opts.Mode.Valid() {
		return errors.New("invalid controller mode")
	}
	if opts.Mode == reconcile.Canary && (strings.TrimSpace(opts.CanaryScope) == "" || strings.TrimSpace(opts.CanaryProfile) == "") {
		return errors.New("canary requires an exact scope and profile")
	}
	file, err := d.openConfig(opts.ConfigPath)
	if err != nil {
		return fmt.Errorf("open config: %w", err)
	}
	cfg, decodeErr := config.Decode(file)
	closeErr := file.Close()
	if decodeErr != nil {
		return decodeErr
	}
	if closeErr != nil {
		return fmt.Errorf("close config: %w", closeErr)
	}
	if opts.Mode != reconcile.Observe {
		// A node with no execution technology may observe and nothing else. Shadow,
		// canary and authority all exist to act on a machine this build cannot act
		// on, and every plan they produced would end in a refused Create. Refusing
		// here makes that a startup error an operator reads once, instead of a queue
		// of parked dead letters they read for an hour (ADR 0034, and Phase 1 Part A
		// of docs/MULTI_NODE_PLAN.md, which brings the node up in observe mode).
		//
		// The question is asked of the configuration rather than of the platform,
		// because on Linux the two stages of the bring-up differ only by an
		// `executor` block: issue #138's node has none and observes, and issue
		// #139's names podman and provisions.
		if !d.executes(cfg) {
			return fmt.Errorf("controller mode %s requires an execution backend, which this node has none configured for", opts.Mode)
		}
		if err := cfg.ValidateAuthority(); err != nil {
			return err
		}
		// The backend is configured; now prove it is there. An absent or root-ful
		// container runtime is a refusal to start, never a node that promises
		// GitHub a runner it cannot build.
		if d.preflight != nil {
			if err := d.preflight(ctx, cfg); err != nil {
				return fmt.Errorf("execution backend preflight: %w", err)
			}
		}
	}
	store, err := d.openStore(ctx, opts.DatabasePath)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	schedulerConfig := app.BuildSchedulerConfig(cfg)
	bindings, err := app.BuildBindings(cfg, schedulerConfig)
	if err != nil {
		return err
	}
	bindings, err = selectRuntimeBindings(bindings, opts)
	if err != nil {
		return err
	}
	var authority *authorityLease
	controllerLeaseOwner := ""
	if opts.Mode == reconcile.Canary || opts.Mode == reconcile.Authority {
		controllerLeaseOwner = d.leaseOwner(cfg)
		authority, err = startAuthorityLease(ctx, store, controllerLeaseOwner, d.now, d.after)
		if err != nil {
			return fmt.Errorf("acquire controller authority: %w", err)
		}
		defer authority.Close()
	}

	profiles := make([]string, 0, len(schedulerConfig.Profiles))
	for id := range schedulerConfig.Profiles {
		profiles = append(profiles, string(id))
	}
	officialScaleSetSessions := opts.Mode != reconcile.Observe
	canonicalRESTInventory := cfg.GitHub.CanonicalJobInventory
	criticalObservations := []string{"operations", "scheduler"}
	if officialScaleSetSessions {
		for _, binding := range bindings {
			criticalObservations = append(criticalObservations, fmt.Sprintf("github-%d", binding.StoreKey))
		}
	}
	if canonicalRESTInventory {
		for scope := range bindingsByScope(bindings) {
			criticalObservations = append(criticalObservations, "github-rest-"+scope)
		}
	}
	health, _ := telemetry.NewHealth(wallClock{}, telemetry.HealthConfig{Profiles: profiles,
		CriticalObservations: criticalObservations, CriticalObservationTTL: 2 * time.Minute,
		FailureComponents: failureComponents})
	serverConfig := telemetry.ServerConfig{ControllerVersion: opts.Version, ControllerMode: string(opts.Mode)}
	healthServer, _ := telemetry.NewServer(health, serverConfig)
	listener, err := d.listen("tcp", opts.HealthAddress)
	if err != nil {
		return fmt.Errorf("listen health: %w", err)
	}
	defer func() { _ = listener.Close() }()
	// Only the private socket server carries the mutator, so the loopback health
	// listener has no mutating route at all. Authority gating is the daemon's, not
	// the CLI's: a direct socket caller must meet the same bar.
	adminConfig := serverConfig
	adminConfig.Mutator = discharge.Service{Store: store, VM: d.newReaper(store, cfg),
		Authority: opts.Mode == reconcile.Authority, Now: d.now,
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))}
	adminServer, _ := telemetry.NewServer(health, adminConfig)
	adminListener, err := d.adminListen(opts.AdminSocket)
	if err != nil {
		return fmt.Errorf("listen admin: %w", err)
	}
	defer func() { _ = adminListener.Close() }()
	type serverResult struct {
		name string
		err  error
	}
	serverDone := make(chan serverResult, 2)
	go func() { serverDone <- serverResult{name: "health", err: healthServer.Serve(listener)} }()
	go func() { serverDone <- serverResult{name: "admin", err: adminServer.Serve(adminListener)} }()
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = healthServer.Shutdown(shutdown)
		_ = adminServer.Shutdown(shutdown)
	}()

	reporter := newFailureReporter(os.Stderr, d.now)
	// Consulted by the scheduler's host observation on every tick: a node
	// draining toward its own update refuses admission while the instances it is
	// waiting on finish, which is how quiescence becomes reachable at all.
	updateDrain := newUpdateDrainController(autoupdate.DrainPolicy{Enabled: cfg.UpdateDrain.Enabled,
		PendingFor: cfg.UpdateDrain.PendingFor, MaxWait: cfg.UpdateDrain.MaxWait,
		Cooldown: cfg.UpdateDrain.Cooldown}, installationRoot(opts.ConfigPath), opts.Version,
		autoupdate.ReleaseDirNames, reporter)
	reporter.counter = health
	// What runner version each base image carries is a fact about the
	// configuration this process started with, and configuration does not change
	// without a restart, so it is published once rather than recomputed on every
	// tick. A refusal is said out loud: an unpublished set renders as "no image is
	// behind", which is precisely the silence issue #206 exists to remove.
	if err := health.SetRunnerImages(runnerImages(cfg)); err != nil {
		reporter.logger.Warn("runner image versions were refused, so this node cannot judge its own "+
			"brownout compliance", "error", err, "linuxBaseVm", cfg.Linux.BaseVM, "macosBaseVm", cfg.MacOS.BaseVM)
	}
	// Whether a Linux guest's kernel can leave evidence behind when it dies is
	// also a fact about the configuration this process started with. Three
	// incidents (#236, #258, #259) ended at *trigger unidentified* because the
	// serial sink was off on every node; publishing the posture once here is what
	// makes the lapse a doctor finding instead of an operator's memory.
	health.SetGuestConsole(guestConsole(runtime.GOOS, cfg))
	coordinator := app.DemandCoordinator{Store: store, Now: d.now, StatisticsMaxAge: 2 * time.Minute,
		StrictJobRouting: opts.Mode != reconcile.Canary, OnSequenceReset: reporter.reportSequenceReset,
		Priority: cfg.Priority.Policy()}
	// One state for the whole node: withdrawal is a property of the machine's
	// ability to admit, not of any single binding, and the ingesters must read
	// the same conclusion the controller acts on.
	yieldState := newSessionYieldState(sessionYieldPolicy{Enabled: cfg.SessionYield.Enabled,
		BlockedFor: cfg.SessionYield.BlockedFor, HealthyFor: cfg.SessionYield.HealthyFor})
	ingesters := make([]app.Ingester, 0, len(bindings))
	closers := make([]scaleSetSource, 0, len(bindings))
	recoveryLimiter := make(chan struct{}, scaleSetCloseConcurrency)
	controls := make(map[lifecycle.SourceKey]lifecycle.SourceBinding)
	if officialScaleSetSessions || canonicalRESTInventory {
		service, account, path := githubCredential(cfg)
		secret, err := d.loadKey(ctx, service, account, path)
		if err != nil {
			return err
		}
		privateKey := githubscaleset.NewPrivateKeySecret(secret.Reveal())
		secret.Destroy()
		defer privateKey.Destroy()
		if officialScaleSetSessions {
			defer func() {
				closeCtx, cancel := context.WithTimeout(context.Background(), scaleSetCloseTimeout)
				defer cancel()
				if closeErr := closeScaleSetSources(closeCtx, closers); closeErr != nil {
					if retErr == nil {
						retErr = closeErr
					} else {
						retErr = errors.Join(retErr, closeErr)
					}
				}
			}()
			for _, binding := range bindings {
				settings, err := sourceSettingsFor(cfg, binding)
				if err != nil {
					return err
				}
				openSource := func(openCtx context.Context) (scaleSetSource, error) {
					cursor, cursorErr := d.cursor(openCtx, store, settings.cursorKey)
					if cursorErr != nil {
						return nil, cursorErr
					}
					return d.newScaleSet(openCtx, githubscaleset.GitHubAppScaleSetConfig{GitHubConfigURL: settings.configURL,
						ClientID: settings.clientID, InstallationID: settings.installationID, PrivateKey: privateKey,
						ScaleSetID: settings.scaleSet.ID, Owner: settings.owner, MaxCapacity: settings.scaleSet.MaxCapacity, InitialCursor: int(cursor),
						RequestTimeout: cfg.Timeouts.GitHub,
						System:         "tart-runner-fleet", Version: opts.Version, Subsystem: "controller"})
				}
				initial, err := openSource(ctx)
				if err != nil {
					return err
				}
				source, err := newRecoveringScaleSetSource(recoveringScaleSetConfig{source: initial,
					open: openSource, limiter: recoveryLimiter, now: d.now,
					policy: githubscaleset.SessionRecoveryPolicy{
						MaxConsecutiveFailures: cfg.SessionRecovery.MaxConsecutiveFailures,
						FailureWindow:          cfg.SessionRecovery.FailureWindow}})
				if err != nil {
					if initial != nil {
						_ = initial.Close(ctx)
					}
					return err
				}
				closers = append(closers, source)
				ingesters = append(ingesters, boundIngester{coordinator: coordinator, binding: binding, source: source,
					health: health, observation: fmt.Sprintf("github-%d", binding.StoreKey), reporter: reporter,
					yield: yieldState})
				for _, target := range bindingTargets(binding, cfg.Targets) {
					key := lifecycle.SourceKey{Repo: target, Profile: binding.Profile.ID}
					if _, duplicate := controls[key]; duplicate {
						return fmt.Errorf("duplicate GitHub control binding for %s/%s", target, binding.Profile.ID)
					}
					controls[key] = lifecycle.SourceBinding{StoreKey: binding.StoreKey, Source: source}
				}
			}
		}
		if canonicalRESTInventory {
			for scope, scopeBindings := range bindingsByScope(bindings) {
				settings, err := sourceSettingsFor(cfg, scopeBindings[0])
				if err != nil {
					return err
				}
				apiBase, err := githubscaleset.APIBaseURL(settings.configURL)
				if err != nil {
					return err
				}
				tokens, err := githubscaleset.NewInstallationTokenSource(githubscaleset.InstallationTokenConfig{
					APIBaseURL: apiBase, ClientID: settings.clientID, InstallationID: settings.installationID,
					PrivateKey: privateKey, Timeout: cfg.Timeouts.GitHub,
				})
				if err != nil {
					return err
				}
				observer, err := d.newRESTObserver(githubscaleset.ObserverConfig{BaseURL: apiBase,
					Repositories: repositoriesForBindings(scopeBindings, cfg.Targets), HTTP: http.DefaultClient,
					Tokens: tokens, Timeout: cfg.Timeouts.GitHub})
				if err != nil {
					return err
				}
				ingesters = append(ingesters, &restQueueIngester{coordinator: coordinator, bindings: scopeBindings,
					observer: observer, interval: max(30*time.Second, cfg.PollInterval), health: health,
					observation: "github-rest-" + scope, now: d.now})
			}
		}
	}
	control := &lifecycle.ControlRouter{State: store, Demand: store, Sources: controls, Now: d.now}
	var recovery app.RecoveryObserver
	if opts.Mode != reconcile.Observe {
		recovery = control
	}
	engine := app.Engine{Store: store, Demand: coordinator, Inventory: d.inventory(store, cfg, recovery, updateDrain), Config: schedulerConfig,
		Bindings: bindings, ControllerID: "tart-runner-fleet", Mode: opts.Mode}
	yield := newSessionYieldController(yieldState, closers, reporter)
	ticker := engineTicker{engine: engine, health: health, profiles: profiles, operationCounts: store.OperationCounts,
		operationFailures: store.OperationFailures, deadLetters: store.DeadLetters,
		stalledOperations: store.StalledOperations, now: d.now, reporter: reporter, yield: yield, drain: updateDrain}
	var worker app.WorkRunner
	if opts.Mode == reconcile.Canary || opts.Mode == reconcile.Authority {
		vm := d.newVM(store, cfg, control)
		diskGiB := profileDiskFloors(cfg)
		worker = operationRunner{worker: operations.Worker{
			Store: store, Owner: controllerLeaseOwner, MaxConcurrent: workerConcurrency(opts.Mode, cfg),
			Executors: map[string]operations.Executor{
				lifecycle.OperationProvision: lifecycle.ProvisionExecutor{State: store, VM: vm, Ready: d.readiness(cfg),
					Registration: control, Bootstrap: d.bootstrap(cfg), Bases: map[domain.Platform]string{
						domain.PlatformLinux: d.linuxImage(cfg), domain.PlatformMacOS: cfg.MacOS.BaseVM}, DiskGiB: diskGiB,
					Capabilities: profileCapabilities(cfg)},
				lifecycle.OperationDrain: lifecycle.DrainExecutor{State: store, VM: vm, Control: control,
					ConfirmationMaxAge: deletionConfirmationMaxAge, Guest: d.guestProbe(cfg), Now: d.now},
			},
			Retry: operations.RetryPolicy{Maximum: provisionRetryMaximum, MaxAttempts: lifecycleRetryMaxAttempts},
			RetryByKind: map[string]operations.RetryPolicy{
				lifecycle.OperationDrain: operations.DurableCleanupRetryPolicy(drainRetryMaximum),
			},
			OperationDeadline: lifecycleOperationDeadline(cfg),
		}}
	}
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	serviceDone := make(chan error, 1)
	go func() {
		serviceDone <- (app.Service{Ticker: ticker, Ingesters: ingesters, Worker: worker,
			TickInterval: cfg.PollInterval, WorkInterval: 250 * time.Millisecond,
			OnFailure: reporter.report}).Run(runCtx)
	}()
	select {
	case err := <-serviceDone:
		// Release the authority lease immediately, before the deferred
		// scale-set drain (up to scaleSetCloseTimeout). An immediate successor
		// then acquires during that drain instead of waiting out the full TTL.
		// Close is idempotent, so the deferred Close above stays a safe backstop.
		authority.Close()
		return err
	case err := <-authorityErrors(authority):
		cancelRun()
		<-serviceDone
		return fmt.Errorf("controller authority lost: %w", err)
	case result := <-serverDone:
		cancelRun()
		select {
		case <-serviceDone:
		case <-time.After(5 * time.Second):
			return fmt.Errorf("%s %w: service shutdown timed out", result.name, serverFailure(result.err))
		}
		return fmt.Errorf("%s %w", result.name, serverFailure(result.err))
	}
}

func profileDiskFloors(cfg config.Config) map[domain.ProfileID]int {
	floors := make(map[domain.ProfileID]int, len(cfg.Linux.Profiles)+2)
	for _, profile := range cfg.Linux.Profiles {
		floors[domain.ProfileID(profile.ID)] = profile.DiskGiB
	}
	if cfg.MacOS.Enabled {
		floors[domain.ProfileID(cfg.MacOS.Builder.ID)] = cfg.MacOS.Builder.DiskGiB
		floors[domain.ProfileID(cfg.MacOS.Maestro.ID)] = cfg.MacOS.Maestro.DiskGiB
	}
	return floors
}

// profileCapabilities is what each profile's guest image must provide, keyed the
// way an instance records itself. A durable instance carries its profile and not
// the scale set it was spawned for, and a profile may be exposed by more than
// one scope, so the union across every scale set routed to it is both the
// conservative answer and the only one derivable at this point.
// runnerImages projects each base image this node boots into the telemetry DTO,
// carrying the verdict internal/config computed rather than a second opinion.
// The floor rule is stated once, as config.RunnerImage.Reason, so the metric,
// the `fleet doctor` finding and the configuration file cannot disagree about
// which image GitHub is about to stop accepting registrations from.
func runnerImages(cfg config.Config) []telemetry.RunnerImageMetric {
	declared := cfg.RunnerImages()
	images := make([]telemetry.RunnerImageMetric, 0, len(declared))
	for _, image := range declared {
		images = append(images, telemetry.RunnerImageMetric{Platform: image.Platform, VM: image.VM,
			Version: image.Version, Floor: image.Floor, Reason: image.Reason()})
	}
	return images
}

// guestConsole is the node's answer to the doctor check's one question: does
// this machine boot Linux guests through Tart, and are their consoles captured?
//
// BootsLinuxGuests asks about the OPERATING SYSTEM as well as the base image,
// and that is not incidental. `linux.baseVm` is a Tart base VM name, but the
// schema keeps it in every node's file — an observe-only Linux node or a podman
// container node carries one it will never clone (ADR 0034) — so a predicate on
// the field alone would fail every Linux node in CI and in production for a
// console no such node can lose.
func guestConsole(goos string, cfg config.Config) telemetry.GuestConsoleMetric {
	return telemetry.GuestConsoleMetric{BootsLinuxGuests: goos == "darwin" && cfg.Linux.BaseVM != "",
		SerialLogConfigured: cfg.Linux.SerialLogDirectory != ""}
}

func profileCapabilities(cfg config.Config) map[domain.ProfileID][]string {
	required := cfg.ProfileRequiredCapabilities()
	capabilities := make(map[domain.ProfileID][]string, len(required))
	for profile, names := range required {
		capabilities[domain.ProfileID(profile)] = names
	}
	return capabilities
}

func closeScaleSetSources(ctx context.Context, sources []scaleSetSource) error {
	active := make([]scaleSetSource, 0, len(sources))
	for _, source := range sources {
		if source != nil {
			active = append(active, source)
		}
	}
	if len(active) == 0 {
		return nil
	}

	jobs := make(chan scaleSetSource)
	failures := make(chan struct{}, len(active))
	workers := min(scaleSetCloseConcurrency, len(active))
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for source := range jobs {
				if err := source.Close(ctx); err != nil {
					failures <- struct{}{}
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, source := range active {
			select {
			case jobs <- source:
			case <-ctx.Done():
				return
			}
		}
	}()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		if failed := len(failures); failed > 0 {
			return fmt.Errorf("%w: %d source(s)", errScaleSetClose, failed)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%w: deadline exceeded", errScaleSetClose)
	}
}

func selectRuntimeBindings(bindings []app.Binding, opts options) ([]app.Binding, error) {
	if opts.Mode != reconcile.Canary {
		return append([]app.Binding(nil), bindings...), nil
	}
	scope := strings.TrimSpace(opts.CanaryScope)
	profile := domain.ProfileID(strings.TrimSpace(opts.CanaryProfile))
	if scope == "" || profile == "" {
		return nil, errors.New("canary requires an exact scope and profile")
	}
	selected := make([]app.Binding, 0, 1)
	for _, binding := range bindings {
		bindingScope := binding.Scope
		if bindingScope == "" {
			bindingScope = legacyScopeName
		}
		if bindingScope == scope && binding.Profile.ID == profile {
			binding.Targets = append([]string(nil), binding.Targets...)
			binding.ScaleSetLabels = append([]string(nil), binding.ScaleSetLabels...)
			binding.RequiredLabels = append([]string(nil), binding.RequiredLabels...)
			if !containsString(binding.RequiredLabels, canaryDemandLabel) {
				binding.RequiredLabels = append(binding.RequiredLabels, canaryDemandLabel)
			}
			selected = append(selected, binding)
		}
	}
	if len(selected) != 1 {
		return nil, fmt.Errorf("canary selector %s/%s resolved %d bindings", scope, profile, len(selected))
	}
	return selected, nil
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

type sourceSettings struct {
	configURL      string
	clientID       string
	installationID int64
	owner          string
	scaleSet       config.ScaleSet
	cursorKey      int64
}

func sourceSettingsFor(cfg config.Config, binding app.Binding) (sourceSettings, error) {
	if binding.StoreKey <= 0 || binding.ScaleSetID <= 0 || binding.Profile.ID == "" {
		return sourceSettings{}, errors.New("invalid runtime binding")
	}
	if len(cfg.GitHub.Scopes) == 0 {
		for _, scaleSet := range cfg.GitHub.ScaleSets {
			if int64(scaleSet.ID) == binding.ScaleSetID && scaleSet.Profile == string(binding.Profile.ID) {
				return sourceSettings{configURL: cfg.GitHub.ConfigURL, clientID: cfg.GitHub.ClientID,
					installationID: cfg.GitHub.InstallationID, owner: cfg.GitHub.Owner, scaleSet: scaleSet, cursorKey: binding.StoreKey}, nil
			}
		}
		return sourceSettings{}, fmt.Errorf("legacy scale set %d/%s is not configured", binding.ScaleSetID, binding.Profile.ID)
	}
	for _, scope := range cfg.GitHub.Scopes {
		if scope.Name != binding.Scope {
			continue
		}
		installationID := int64(0)
		for _, installation := range cfg.GitHub.Installations {
			if strings.EqualFold(installation.Name, scope.Installation) {
				installationID = installation.InstallationID
				break
			}
		}
		if installationID <= 0 {
			return sourceSettings{}, fmt.Errorf("scope %q installation is unavailable", scope.Name)
		}
		for _, scaleSet := range scope.ScaleSets {
			if int64(scaleSet.ID) == binding.ScaleSetID && scaleSet.Profile == string(binding.Profile.ID) {
				return sourceSettings{configURL: scope.ConfigURL, clientID: cfg.GitHub.App.ClientID,
					installationID: installationID, owner: cfg.GitHub.SessionOwner, scaleSet: scaleSet, cursorKey: binding.StoreKey}, nil
			}
		}
		return sourceSettings{}, fmt.Errorf("scope %q scale set %d/%s is not configured", scope.Name, binding.ScaleSetID, binding.Profile.ID)
	}
	return sourceSettings{}, fmt.Errorf("GitHub scope %q is not configured", binding.Scope)
}

func githubCredential(cfg config.Config) (string, string, string) {
	if len(cfg.GitHub.Scopes) > 0 {
		return cfg.GitHub.App.KeychainService, cfg.GitHub.App.KeychainAccount, cfg.GitHub.App.PrivateKeyFile
	}
	return cfg.GitHub.KeychainService, cfg.GitHub.KeychainAccount, ""
}

func bindingTargets(binding app.Binding, configured []config.Target) []string {
	if len(binding.Targets) > 0 {
		return append([]string(nil), binding.Targets...)
	}
	targets := make([]string, 0, len(configured))
	for _, target := range configured {
		targets = append(targets, target.Slug)
	}
	return targets
}

func lifecycleOperationDeadline(cfg config.Config) time.Duration {
	return cfg.Timeouts.Boot + 4*cfg.Timeouts.Tart + 3*cfg.Timeouts.GitHub
}

func workerConcurrency(mode reconcile.Mode, cfg config.Config) int {
	if mode == reconcile.Canary {
		return 1
	}
	return cfg.Linux.MaxInstances
}

// execReadiness waits for a guest to answer, by asking the node's backend to
// execute a trivial command inside it. The verb is `exec <instance> true` on
// both supported backends, so the probe is typed on the neutral command runner
// and a second backend changes the wiring, not this code.
type execReadiness struct {
	Runner         executor.CommandRunner
	Timeout        time.Duration
	AttemptTimeout time.Duration
	RetryInterval  time.Duration
	After          func(time.Duration) <-chan time.Time
}

func (p execReadiness) Wait(ctx context.Context, instance operations.Instance) error {
	if p.Runner == nil || domain.ValidateInstanceName(instance.ID) != nil || p.Timeout <= 0 || p.AttemptTimeout < 0 || p.RetryInterval < 0 {
		return operations.ErrInvalid
	}
	probeCtx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	attemptTimeout := p.AttemptTimeout
	if attemptTimeout <= 0 || attemptTimeout > p.Timeout {
		attemptTimeout = p.Timeout
	}
	retryInterval := p.RetryInterval
	if retryInterval <= 0 {
		retryInterval = 250 * time.Millisecond
	}
	after := p.After
	if after == nil {
		after = time.After
	}
	for {
		attemptCtx, attemptCancel := context.WithTimeout(probeCtx, attemptTimeout)
		_, err := p.Runner.Run(attemptCtx, "exec", instance.ID, "true")
		attemptCancel()
		if err == nil {
			return nil
		}
		select {
		case <-probeCtx.Done():
			return probeCtx.Err()
		case <-after(retryInterval):
		}
	}
}

// execGuestProbe asks a running guest to execute a trivial command, and
// classifies the three outcomes that matter. It is the same `exec <instance>
// true` verb `execReadiness` polls at boot, on the same neutral command runner,
// so a second backend changes the wiring rather than this code.
//
// The classification is the whole safety argument of ADR 0040, and it is
// deliberately made from the probe's OWN deadline rather than from anything the
// backend said. A command that returned before the deadline and failed could not
// reach the guest: on Tart that is `Failed to connect to the VM using its control
// socket`, which is what a panicked kernel produces immediately and repeatedly. A
// command that ran out of the deadline established nothing — a guest running a
// monorepo build at full tilt is slow, and slow is not dead. Reading a backend's
// error text to tell those apart would put a Tart string in a layer that must not
// know which machine it is on.
type execGuestProbe struct {
	Runner  executor.CommandRunner
	Timeout time.Duration
}

func (p execGuestProbe) Probe(ctx context.Context, instanceID string) domain.GuestLiveness {
	if p.Runner == nil || p.Timeout <= 0 || domain.ValidateInstanceName(instanceID) != nil {
		return domain.GuestLivenessUnknown
	}
	attempt, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	_, err := p.Runner.Run(attempt, "exec", instanceID, "true")
	switch {
	case err == nil:
		return domain.GuestLivenessAlive
	case attempt.Err() != nil:
		// The probe ran out of its own deadline, or the tick was cancelled under
		// it. Either way nothing was established, and an unknown observation never
		// accumulates toward a verdict.
		return domain.GuestLivenessUnknown
	default:
		return domain.GuestLivenessRefused
	}
}

type authorityLease struct {
	store  operations.Store
	lease  operations.Lease
	now    func() time.Time
	cancel context.CancelFunc
	done   chan struct{}
	errors chan error
	mu     sync.Mutex
	once   sync.Once
}

// acquireAuthorityLease obtains the controller authority lease, patiently
// waiting out a stale lease left behind by a predecessor that exited without
// releasing it (crash, SIGKILL, or a deploy/restart inside the lease TTL).
//
// It is strictly fail-closed and NEVER steals an actively renewed lease:
// AcquireLease only succeeds when the row is absent or already expired, so a
// live incumbent that keeps renewing keeps us waiting until the window elapses,
// at which point we surface the original "lease held" error unchanged. We are
// adding patience, not takeover.
func acquireAuthorityLease(ctx context.Context, store operations.Store, owner string, now func() time.Time, after func(time.Duration) <-chan time.Time) (operations.Lease, error) {
	deadline := now().Add(authorityLeaseAcquireWindow)
	for {
		lease, err := store.AcquireLease(ctx, authorityLeaseName, owner, now().UTC(), authorityLeaseTTL)
		if !errors.Is(err, operations.ErrLeaseHeld) {
			return lease, err
		}
		if !now().Before(deadline) {
			return operations.Lease{}, err
		}
		select {
		case <-ctx.Done():
			return operations.Lease{}, ctx.Err()
		case <-after(authorityLeaseAcquireRetry):
		}
	}
}

func startAuthorityLease(ctx context.Context, store operations.Store, owner string, now func() time.Time, after func(time.Duration) <-chan time.Time) (*authorityLease, error) {
	if store == nil || strings.TrimSpace(owner) == "" {
		return nil, operations.ErrInvalid
	}
	if now == nil {
		now = time.Now
	}
	if after == nil {
		after = time.After
	}
	acquired, err := acquireAuthorityLease(ctx, store, owner, now, after)
	if err != nil {
		return nil, err
	}
	if _, err := store.RecoverExpired(ctx, now().UTC()); err != nil {
		_ = store.ReleaseLease(context.WithoutCancel(ctx), acquired)
		return nil, err
	}
	leaseCtx, cancel := context.WithCancel(ctx)
	guard := &authorityLease{store: store, lease: acquired, now: now, cancel: cancel, done: make(chan struct{}), errors: make(chan error, 1)}
	go guard.renew(leaseCtx, after)
	return guard, nil
}

// renew keeps the durable authority lease alive.
//
// A renewal that merely fails to COMPLETE is not evidence that authority moved.
// The durable row still names this owner until ExpiresAt, and the store is
// reached over one serialized SQLite connection shared with the scheduler's plan
// commit and the operations worker, so a saturated host turns a five-second
// attempt into a timeout while the lease still holds twenty seconds of validity.
// Incident 2026-08-01: three daemon exits inside seventy minutes, each
// "controller authority lost: renew lease: context deadline exceeded", each
// costing a launchd restart plus a successor's wait for the abandoned lease to
// drain — scheduling gaps measured in minutes, caused by a transient I/O blip.
//
// So a transient failure is retried INSIDE the lease's own validity, and the
// guard surrenders the moment it can no longer prove it holds the lease. This
// only ever narrows the window in which the process claims authority: no attempt
// may outlive ExpiresAt, and an expired lease is reported as lost rather than
// renewed. A fencing loss (ErrLeaseLost — another owner or a deleted row) or a
// malformed request (ErrInvalid) is reported immediately and unchanged: those are
// proof, not a blip.
func (g *authorityLease) renew(ctx context.Context, after func(time.Duration) <-chan time.Time) {
	defer close(g.done)
	delay := authorityLeaseRenewInterval
	for {
		select {
		case <-ctx.Done():
			return
		case <-after(delay):
		}
		g.mu.Lock()
		lease := g.lease
		g.mu.Unlock()
		budget := renewBudget(g.now().UTC(), lease.ExpiresAt)
		if budget <= 0 {
			g.errors <- fmt.Errorf("renew lease: %w", operations.ErrLeaseLost)
			return
		}
		renewCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), budget)
		renewed, err := g.store.RenewLease(renewCtx, lease, g.now().UTC(), authorityLeaseTTL)
		cancel()
		if err == nil {
			g.mu.Lock()
			g.lease = renewed
			g.mu.Unlock()
			delay = authorityLeaseRenewInterval
			continue
		}
		if !transientRenewFailure(err) || !renewRetryFits(g.now().UTC(), lease.ExpiresAt) {
			g.errors <- err
			return
		}
		delay = authorityLeaseRenewRetry
	}
}

// renewBudget bounds one renewal attempt so it can never outlive the lease it is
// renewing. Blocking past ExpiresAt would leave this process acting on authority
// a successor is already entitled to acquire.
func renewBudget(now, expiresAt time.Time) time.Duration {
	return min(authorityLeaseRenewTimeout, expiresAt.Sub(now))
}

// renewRetryFits reports whether another grace retry can still complete while
// this lease is provably held. When it cannot, the guard fails closed instead of
// scheduling an attempt that would land after expiry.
func renewRetryFits(now, expiresAt time.Time) bool {
	return now.Add(authorityLeaseRenewRetry).Before(expiresAt)
}

// transientRenewFailure separates "the renewal did not complete" from "the lease
// is no longer ours". Only the latter is durable evidence of lost authority; the
// former is an I/O outcome that the remaining TTL exists to absorb.
func transientRenewFailure(err error) bool {
	return !errors.Is(err, operations.ErrLeaseLost) && !errors.Is(err, operations.ErrInvalid)
}

func (g *authorityLease) Close() {
	if g == nil {
		return
	}
	g.once.Do(func() {
		g.cancel()
		<-g.done
		g.mu.Lock()
		lease := g.lease
		g.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = g.store.ReleaseLease(ctx, lease)
	})
}

func authorityErrors(lease *authorityLease) <-chan error {
	if lease == nil {
		return nil
	}
	return lease.errors
}

type operationRunner struct{ worker operations.Worker }

func (r operationRunner) Work(ctx context.Context) error {
	_, err := r.worker.RunOnce(ctx)
	return err
}

func serverFailure(err error) error {
	if err == nil {
		return errors.New("health server stopped unexpectedly")
	}
	return fmt.Errorf("health server stopped: %w", err)
}

type boundIngester struct {
	coordinator app.DemandCoordinator
	binding     app.Binding
	source      app.MessageSource
	health      *telemetry.Health
	observation string
	reporter    *failureReporter
	// yield is the node's own withdrawal. A suspended binding has no session to
	// poll, and polling it anyway would report a session failure this node
	// caused on purpose — which is how a deliberate withdrawal would come to
	// look like the broker outage it is not.
	yield *sessionYieldState
}

type restQueueIngester struct {
	coordinator app.DemandCoordinator
	bindings    []app.Binding
	observer    queueObserver
	interval    time.Duration
	health      *telemetry.Health
	observation string
	now         func() time.Time
	previous    *githubscaleset.Snapshot
	next        time.Time
}

type queueObserver interface {
	Refresh(context.Context, *githubscaleset.Snapshot) githubscaleset.Observation
}

func (r *restQueueIngester) Ingest(ctx context.Context) error {
	_, err := r.IngestChanged(ctx)
	return err
}

func (r *restQueueIngester) IngestChanged(ctx context.Context) (bool, error) {
	if r == nil || r.observer == nil || r.now == nil || r.interval <= 0 {
		return false, operations.ErrInvalid
	}
	now := r.now().UTC()
	if wait := r.next.Sub(now); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-timer.C:
		}
		now = r.now().UTC()
	}
	r.next = now.Add(r.interval)
	observation := r.observer.Refresh(ctx, r.previous)
	if observation.Err != nil {
		if r.health != nil && r.observation != "" {
			freshness, detail := telemetry.ObservationUnavailable, githubscaleset.ReasonQueueObservationFailed
			if observation.Freshness == githubscaleset.Stale {
				freshness, detail = telemetry.ObservationStale, githubscaleset.ReasonQueueObservationStale
			}
			_ = r.health.RecordObservationDetail(r.observation, freshness, detail)
		}
		return false, observation.Err
	}
	r.previous = observation.Snapshot
	changed, err := r.coordinator.ReconcileQueuedJobs(ctx, r.bindings, observation.Snapshot)
	if r.health != nil && r.observation != "" {
		freshness, detail := telemetry.ObservationFresh, ""
		if err != nil {
			freshness, detail = telemetry.ObservationUnavailable, githubscaleset.ReasonQueueReconcileFailed
		}
		_ = r.health.RecordObservationDetail(r.observation, freshness, detail)
	}
	return changed, err
}

func bindingsByScope(bindings []app.Binding) map[string][]app.Binding {
	grouped := make(map[string][]app.Binding)
	for _, binding := range bindings {
		scope := binding.Scope
		if scope == "" {
			scope = legacyScopeName
		}
		grouped[scope] = append(grouped[scope], binding)
	}
	return grouped
}

func repositoriesForBindings(bindings []app.Binding, targets []config.Target) []githubscaleset.Repository {
	seen := map[string]struct{}{}
	var repositories []githubscaleset.Repository
	add := func(slug string) {
		parts := strings.Split(slug, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return
		}
		if _, exists := seen[slug]; exists {
			return
		}
		seen[slug] = struct{}{}
		repositories = append(repositories, githubscaleset.Repository{Owner: parts[0], Name: parts[1]})
	}
	for _, binding := range bindings {
		for _, target := range binding.Targets {
			add(target)
		}
	}
	if len(repositories) == 0 {
		for _, target := range targets {
			add(target.Slug)
		}
	}
	return repositories
}

func (b boundIngester) Ingest(ctx context.Context) error {
	_, err := b.IngestChanged(ctx)
	return err
}

func (b boundIngester) IngestChanged(ctx context.Context) (bool, error) {
	if b.yield.Yielded() {
		// Stale, not fresh: this node observed nothing. Stale, not unavailable:
		// nothing failed. The detail names the cause so the freshness column is
		// never the only thing an operator has to interpret.
		if b.health != nil && b.observation != "" {
			_ = b.health.RecordObservationDetail(b.observation, telemetry.ObservationStale, "session_yielded")
		}
		return false, nil
	}
	changed, err := b.coordinator.IngestOnceResult(ctx, b.binding, b.source)
	// The detail is closed vocabulary. It explains why one binding's ingestion
	// failed without exposing a token, a JIT configuration, or an upstream
	// response body.
	detail := githubscaleset.IngestFailureDetail(err)
	if b.health != nil && b.observation != "" {
		freshness := telemetry.ObservationFresh
		if err != nil {
			freshness = telemetry.ObservationUnavailable
		}
		_ = b.health.RecordObservationDetail(b.observation, freshness, detail)
	}
	// The aggregate loop failure names the component and nothing else, so for
	// three days `component=ingest reason=message_poll_failed` was the whole
	// signal that one named scale set had stopped hearing GitHub (issue #165).
	// A binding failure now says which binding.
	if err != nil && ctx.Err() == nil && b.reporter != nil {
		b.reporter.reportBinding(b.binding, detail)
	}
	return changed, err
}

// failureReporter turns app.Service's secret-safe failure callback into a
// rate-limited log line. The callback receives only a static component name by
// design, so nothing an upstream error carries can reach the log. Emitting at
// most one line per component per window keeps a hot failure loop from
// drowning the daemon's stderr while still breaking the historical silence
// that once hid an 18h scheduler outage.
// failureComponents is the closed inventory of control-plane loops app.Service
// starts. It lives beside the wiring that starts them so the metric's label
// space cannot drift from the loops that exist.
var failureComponents = []string{"ingest", "operations", "scheduler"}

// failureCounter is the metric half of a reported failure. It is separated from
// the logger because the two have opposite duties: the log is rate limited so a
// hot loop cannot drown stderr, and the counter must not be, or it reproduces
// the undercount that made the 2026-08-02 wedge look like eight incidents
// instead of one continuous one.
type failureCounter interface {
	RecordComponentFailure(component, reason string)
}

type failureReporter struct {
	mu      sync.Mutex
	last    map[string]time.Time
	now     func() time.Time
	window  time.Duration
	logger  *slog.Logger
	counter failureCounter
}

func newFailureReporter(w io.Writer, now func() time.Time) *failureReporter {
	if now == nil {
		now = time.Now
	}
	return &failureReporter{last: make(map[string]time.Time), now: now, window: time.Minute,
		logger: slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelWarn}))}
}

func (r *failureReporter) report(component, reason string) {
	// Counted before the rate limit, and outside it: every occurrence is a data
	// point even when only the first of the minute is logged.
	if r.counter != nil {
		r.counter.RecordComponentFailure(component, reason)
	}
	// Rate limiting keys on component and reason together. The reason vocabulary
	// is closed, so cardinality stays bounded while a changed reason — the
	// signal an operator needs — is never suppressed by an older one.
	if !r.admit(component + "\x00" + reason) {
		return
	}
	if reason == "" {
		r.logger.Warn("component loop failure", "component", component)
		return
	}
	r.logger.Warn("component loop failure", "component", component, "reason", reason)
}

// reportBinding names the scope, profile, and scale set behind one ingest
// failure. It is rate limited per binding and reason, so a hot loop cannot drown
// stderr while a CHANGED reason on any binding is always emitted, and it does
// not count: the aggregate component counter already counts the same failure,
// and counting it twice would make one incident read as two.
func (r *failureReporter) reportBinding(binding app.Binding, reason string) {
	if !githubscaleset.ValidFailureReason(reason) {
		return
	}
	if !r.admit("binding\x00" + strconv.FormatInt(binding.StoreKey, 10) + "\x00" + reason) {
		return
	}
	r.logger.Warn("binding ingest failure", "scope", binding.Scope, "profile", string(binding.Profile.ID),
		"scaleSet", binding.ScaleSetID, "observation", fmt.Sprintf("github-%d", binding.StoreKey), "reason", reason)
}

// reportUpdateDrain records that this node started or stopped refusing admission
// to reach the quiescence a pending generation needs. It is deliberately not
// rate-limited: a drain is rare, deliberate, and the only explanation for a node
// that is healthy, idle, and admitting nothing.
func (r *failureReporter) reportUpdateDrain(action, version string, instances int) {
	if r == nil || r.logger == nil {
		return
	}
	if version == "" {
		version = "unknown"
	}
	r.logger.Warn("update drain "+action, "candidate", version, "instances", instances)
}

// reportSessionYield records that this node stopped, or resumed, competing for
// the jobs GitHub binds to its scale sets. It is not a failure and it is not
// rate-limited away: a withdrawal is rare, deliberate, and the single fact that
// explains an otherwise healthy node with an empty queue (#292, #297).
func (r *failureReporter) reportSessionYield(action, reason string, failures int) {
	if r == nil || r.logger == nil {
		return
	}
	if reason == "" {
		reason = "admission refused"
	}
	r.logger.Warn("scale-set sessions "+action, "reason", reason, "unreleased", failures)
}

// reportSequenceReset records that a broker restarted its message-id sequence
// and the fleet adopted a new inbox generation. It is not a failure -- the fleet
// recovered by itself, which is the whole point of the contract -- but it is
// durable, rare, and the exact event that once needed three days and hand-written
// SQL, so it is never silent. It is not rate limited: a reset per delivery would
// mean the detection is oscillating, and an operator must see that.
func (r *failureReporter) reportSequenceReset(binding app.Binding, reset operations.DemandSequenceReset) {
	if !reset.Detected {
		return
	}
	r.logger.Warn("broker message sequence restarted", "scope", binding.Scope, "profile", string(binding.Profile.ID),
		"scaleSet", binding.ScaleSetID, "observation", fmt.Sprintf("github-%d", binding.StoreKey),
		"generation", reset.Generation, "retiredMessageId", reset.RetiredMessageID,
		"adoptedMessageId", reset.AdoptedMessageID)
}

// reportOccupancy says out loud that an instance is approaching or has passed
// the ceiling on how long it may hold its vector. It exists because the
// 2026-08-09 leak was found by an owner asking why a release was slow, three
// quarters of an hour after the fleet could first have said so (ADR 0036).
//
// It is rate limited per instance and per state, so a hold that crosses the
// warning fraction and later the budget produces two lines rather than one per
// tick for an hour, while a genuine escalation is never suppressed by the
// warning that preceded it. The lines carry the vector, the duration, the
// ceiling and the job — everything needed to tell a reaped job apart from a
// flake without opening the daemon log a second time.
func (r *failureReporter) reportOccupancy(occupancy []scheduler.Occupancy) {
	for _, hold := range occupancy {
		if !hold.Warned {
			continue
		}
		state := "approaching"
		if hold.OverBudget {
			state = "exceeded"
		}
		if !r.admit("occupancy\x00" + hold.Instance + "\x00" + state) {
			continue
		}
		r.logger.Warn("instance occupancy budget "+state, "instance", hold.Instance,
			"profile", string(hold.Profile), "repo", hold.Repo, "cpu", hold.Resources.CPU,
			"memoryMb", hold.Resources.MemoryMB, "held", hold.Age.Round(time.Second).String(),
			"budget", hold.Budget.Round(time.Second).String(),
			"runId", hold.Demand.RunID, "jobId", hold.Demand.JobID,
			"queuedDemandFits", hold.StarvesQueuedDemand)
	}
}

// reportOccupancyReclaim names a job the fleet is about to end. A reaped job
// fails on GitHub with a lost-communication error, which is indistinguishable
// from a flake unless something says which job was cut and how long it had held
// the host. It is never rate limited: each reclaim is a distinct destructive
// decision, and suppressing the second would hide it.
func (r *failureReporter) reportOccupancyReclaim(operation scheduler.Operation, occupancy []scheduler.Occupancy) {
	if !operation.OccupancyExceeded {
		return
	}
	held, budget := time.Duration(0), time.Duration(0)
	for _, hold := range occupancy {
		if hold.Instance == operation.Instance {
			held, budget = hold.Age, hold.Budget
		}
	}
	r.logger.Warn("instance reclaimed for exceeding its occupancy budget", "instance", operation.Instance,
		"profile", string(operation.Profile), "repo", operation.Demand.Repo,
		"runId", operation.Demand.RunID, "jobId", operation.Demand.JobID, "attempt", operation.Demand.Attempt,
		"held", held.Round(time.Second).String(), "budget", budget.Round(time.Second).String(),
		"outcome", "the job ends as a lost-communication failure on GitHub")
}

// reportGuestSilence says out loud that an instance's guest has stopped
// answering, while it is happening.
//
// It exists because issue #236 produced NO daemon log line at all, eight times.
// The whole class was self-concealing: nothing in the guest could report its own
// kernel panic, nothing in the workflow could run after it, and nothing on the
// host was asking. The line below is the first artifact this fleet has ever
// produced for that condition.
//
// It is rate limited per instance and per state, so a guest that goes quiet and
// is then declared dead produces two lines rather than one per tick, while the
// escalation to a verdict is never suppressed by the warning that preceded it.
func (r *failureReporter) reportGuestSilence(silences []scheduler.GuestSilence) {
	for _, silence := range silences {
		state := "silent"
		if silence.Unresponsive {
			state = "unresponsive"
		}
		if !r.admit("guest\x00" + silence.Instance + "\x00" + state) {
			continue
		}
		r.logger.Warn("instance guest "+state, "instance", silence.Instance,
			"profile", string(silence.Profile), "repo", silence.Repo,
			"cpu", silence.Resources.CPU, "memoryMb", silence.Resources.MemoryMB,
			"refusals", silence.Refusals, "requiredRefusals", silence.RequiredRefusals,
			"silent", silence.Silence.Round(time.Second).String(),
			"window", silence.Window.Round(time.Second).String(),
			"lastAlive", livenessInstant(silence.LastAlive),
			"runId", silence.Demand.RunID, "jobId", silence.Demand.JobID)
	}
}

// reportGuestReclaim names a job the fleet is about to end because the machine
// running it stopped executing. It is never rate limited: each reclaim is a
// distinct destructive decision, and the whole point of this record is that the
// eight it is modelled on produced none.
func (r *failureReporter) reportGuestReclaim(operation scheduler.Operation, silences []scheduler.GuestSilence) {
	if !operation.GuestUnresponsive {
		return
	}
	refusals, silent, lastAlive := 0, time.Duration(0), time.Time{}
	for _, silence := range silences {
		if silence.Instance == operation.Instance {
			refusals, silent, lastAlive = silence.Refusals, silence.Silence, silence.LastAlive
		}
	}
	r.logger.Warn("instance reclaimed because its guest stopped answering", "instance", operation.Instance,
		"profile", string(operation.Profile), "repo", operation.Demand.Repo,
		"runId", operation.Demand.RunID, "jobId", operation.Demand.JobID, "attempt", operation.Demand.Attempt,
		"refusals", refusals, "silent", silent.Round(time.Second).String(),
		"lastAlive", livenessInstant(lastAlive),
		"outcome", "the job ends as a lost-communication failure on GitHub")
}

// reportRecovery names every destructive recovery drain the fleet has just
// planned, and the cause it rests on.
//
// It exists because until issue #246 only two of the six recovery causes said
// anything at all: the occupancy budget and the guest-liveness verdict. A stopped
// recovery, an inactive recovery, a stalled assignment and a lingering runner
// have each been able to destroy a runner in silence since they were written, and
// on 2026-08-17 and 2026-08-18 a stopped recovery was planned two hundred and one
// times across two nights without producing a single line. The whole incident had
// to be reconstructed by re-deriving content-addressed operation identities out of
// the durable ledger, because nothing else recorded which cause had fired.
//
// It is NEVER rate limited. Each of these is a distinct decision to destroy a
// live instance, and a storm of them is precisely the artifact an operator needs
// to see — a suppressed eighty-sixth line is the one that would have named the
// problem.
func (r *failureReporter) reportRecovery(operation scheduler.Operation) {
	if !operation.Recovery {
		return
	}
	cause := "vm powered off"
	switch {
	case operation.ConfirmedInactive:
		cause = "runner confirmed inactive"
	case operation.StalledAssignment:
		cause = "assignment never started"
	case operation.LingeringRunner:
		cause = "runner idle past its deadline"
	case operation.GuestUnresponsive:
		cause = "guest stopped answering"
	case operation.OccupancyExceeded:
		cause = "occupancy budget exceeded"
	}
	r.logger.Warn("instance recovery drain planned", "instance", operation.Instance,
		"profile", string(operation.Profile), "cause", cause,
		"repo", operation.Demand.Repo, "runId", operation.Demand.RunID, "jobId", operation.Demand.JobID)
}

// reportRetractedPremise says out loud that the fleet has contradicted itself
// about an instance: it planned a stopped recovery, and the drain's own re-read
// of the power at the moment of acting sent the instance back to Running.
//
// An abort is the most interesting event in the whole recovery ladder — it is the
// fleet catching itself about to destroy a live runner — and nothing has ever
// recorded one. Two hundred and one of them happened over two nights in silence
// (issue #246). The durable phase left behind on a Running row is that abort, and
// it is what raises the bound the premise must meet before it may act again.
//
// Rate limited per instance, because the condition persists on the row for the
// rest of the instance's life and one line per tick would bury it.
func (r *failureReporter) reportRetractedPremise(instances []domain.Instance) {
	for _, instance := range instances {
		if !instance.PowerRetracted || !instance.Live() {
			continue
		}
		if !r.admit("retracted\x00" + instance.ID) {
			continue
		}
		r.logger.Warn("instance power premise retracted by its own drain", "instance", instance.ID,
			"profile", string(instance.Profile), "repo", instance.Repo,
			"power", string(instance.Power), "stoppedReadings", instance.PowerRun.Refusals,
			"requiredWindow", (domain.PowerCorroboration.Window * domain.PowerRetractedFactor).String(),
			"runId", instance.Demand.RunID, "jobId", instance.Demand.JobID,
			"outcome", "this instance is not reclaimed for a power reading again until it holds for the longer window")
	}
}

// reportUnreadablePower names every live instance whose power the backend could
// not read this tick, and WHICH failure it was.
//
// It exists because the trigger behind two lost nightlies has never been
// identified. Issue #246 tried five reproductions and failed; ADR 0042 could only
// demonstrate the mechanism (`chmod 000` a `config.json`) and had to record the
// cause as unknown. The reason is that `tart`'s `running()` swallows the error
// and answers "not running", so the fleet was never told there had been an error
// at all — the one artifact that would answer the question was discarded at the
// bottom of the stack, every time, for nine minutes a night.
//
// The error class and the latency are both here because the two live hypotheses
// have different shapes: a refused open fails immediately, a starved one takes as
// long as the host is busy. One occurrence with both numbers distinguishes them.
//
// Rate limited per instance and per reason, so a condition that persists for
// minutes produces one line, and a reason that CHANGES — which would itself be
// the finding — produces another.
func (r *failureReporter) reportUnreadablePower(instances []domain.Instance) {
	for _, instance := range instances {
		if !instance.PowerUnreadable.Unreadable() || !instance.Live() {
			continue
		}
		if !r.admit("unreadable\x00" + instance.ID + "\x00" + string(instance.PowerUnreadable.Reason)) {
			continue
		}
		r.logger.Warn("instance power state unreadable", "instance", instance.ID,
			"profile", string(instance.Profile), "repo", instance.Repo, "state", string(instance.State),
			"reason", string(instance.PowerUnreadable.Reason),
			"readLatency", instance.PowerUnreadable.Latency.Round(time.Millisecond).String(),
			"runId", instance.Demand.RunID, "jobId", instance.Demand.JobID,
			"outcome", "nothing is planned on this reading; the instance goes on charging the host")
	}
}

// recordRecoveries says out loud what the fleet has decided to destroy and what
// it has already been wrong about. Both readings come from the tick the plan was
// made on, so the log and the decision can never disagree.
func (e engineTicker) recordRecoveries(result app.TickResult) {
	if e.reporter == nil {
		return
	}
	for _, operation := range result.Plan.Operations {
		e.reporter.reportRecovery(operation)
	}
	e.reporter.reportRetractedPremise(result.Instances)
	e.reporter.reportUnreadablePower(result.Instances)
}

// livenessInstant renders a probe instant, or names its absence. A guest this
// daemon has never seen answer is a different fact from one that answered a
// minute ago, and a zero time rendered as a date is the kind of artifact that
// sends an operator looking at 0001-01-01.
func livenessInstant(at time.Time) string {
	if at.IsZero() {
		return "never observed"
	}
	return at.UTC().Format(time.RFC3339)
}

// admit is the shared rate-limit gate: one line per key per window.
func (r *failureReporter) admit(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if last, ok := r.last[key]; ok && now.Sub(last) < r.window {
		return false
	}
	r.last[key] = now
	return true
}

type engineTicker struct {
	engine            app.Engine
	health            *telemetry.Health
	profiles          []string
	operationCounts   func(context.Context) (int, int, error)
	operationFailures func(context.Context) ([]operations.OperationFailure, error)
	deadLetters       func(context.Context) ([]operations.DeadLetter, error)
	// stalledOperations names what is not finishing: the operations still
	// retrying and the instances still held in a cleanup state (ADR 0039).
	stalledOperations func(context.Context, time.Time) ([]operations.StalledOperation, error)
	now               func() time.Time
	// reporter carries the occupancy warning. A nil reporter publishes the metric
	// and says nothing, which is what an observe-mode harness wants.
	reporter *failureReporter
	// yield decides whether this node keeps competing for the jobs GitHub binds
	// to its scale sets. A nil controller never withdraws, which is every mode
	// that owns no sessions.
	yield *sessionYieldController
	// drain decides whether this node refuses admission to reach the quiescence
	// its own pending update needs (ADR 0011 amendment, issue #230).
	drain *updateDrainController
}

// schedulerFailureDetail extracts the bounded reason a classified tick error
// carries. An unclassified error yields no detail rather than free-form text, so
// no store message, token, or JIT payload can reach the API through this path.
func schedulerFailureDetail(err error) string {
	var reason app.FailureReason
	if errors.As(err, &reason) {
		return reason.FailureReason()
	}
	return ""
}

func (e engineTicker) Tick(ctx context.Context) error {
	result, err := e.engine.Tick(ctx)
	success := err == nil && result.Plan.Status == "ready"
	e.health.RecordTick(success)
	freshness := telemetry.ObservationFresh
	// A blocked plan explains itself through the plan reason. An error has no
	// plan at all -- every failing return yields a zero TickResult -- so the
	// detail comes from the error's bounded classification instead, which is what
	// makes the admin API name the same cause the stderr warning does.
	detail := result.Plan.Reason
	if err != nil {
		freshness = telemetry.ObservationUnavailable
		// The planner's own reason is more specific than the failure token, and on
		// the commit path the tick returns a populated result, so it may already be
		// set. Prefer it and fall back to the classification when the planner said
		// nothing -- every early error return yields a zero TickResult.
		if detail == "" {
			detail = schedulerFailureDetail(err)
		}
	} else if !success {
		freshness = telemetry.ObservationStale
	}
	_ = e.health.RecordObservationDetail("scheduler", freshness, detail)
	if err == nil {
		e.recordMetrics(result)
		busy, counted := 0, false
		if e.operationCounts != nil {
			retrying, dead, countErr := e.operationCounts(ctx)
			failures, failureErr := e.operationFailureMetrics(ctx)
			letters, letterErr := e.deadLetterMetrics(ctx, result.Instances)
			stalled, stalledErr := e.stalledMetrics(ctx)
			if countErr != nil || failureErr != nil || letterErr != nil || stalledErr != nil {
				// Rule 4: an unreadable aggregate degrades the observation. It never
				// publishes an empty failure set, which would read as a healed fleet, and
				// never an empty dead-letter set, which would read as "nothing is parked"
				// and wrongly release the update quiescence gate.
				_ = e.health.RecordObservation("operations", telemetry.ObservationUnavailable)
			} else {
				_ = e.health.SetOperations(retrying, dead)
				_ = e.health.SetOperationFailures(failures)
				_ = e.health.SetDeadLetters(letters)
				_ = e.health.SetStalled(stalled)
				_ = e.health.RecordObservation("operations", telemetry.ObservationFresh)
				busy = retrying
				counted = true
			}
		}
		// Withdrawal is decided on the same tick's facts the scheduler admitted
		// on, and only when the operation aggregate was readable: an unknown
		// number of retrying operations is not proof of an idle node, and Rule 4
		// forbids treating an unreadable count as zero.
		if counted {
			e.applySessionYield(ctx, result, busy)
		}
		e.applyUpdateDrain(result)
	}
	return err
}

// applyUpdateDrain folds this tick into the update-drain policy and publishes
// the result every tick, so a node that has been draining for an hour still says
// so rather than only having said it once.
func (e engineTicker) applyUpdateDrain(result app.TickResult) {
	if e.drain == nil {
		return
	}
	live := 0
	for _, instance := range result.Instances {
		if instance.Live() {
			live++
		}
	}
	e.drain.Observe(result.At, live)
	e.health.SetUpdateDrain(e.drain.Metric())
}

// applySessionYield folds this tick into the yield policy and publishes the
// result. The metric is published every tick, not only on a transition, so a
// node that withdrew an hour ago still says so.
func (e engineTicker) applySessionYield(ctx context.Context, result app.TickResult, busy int) {
	if e.yield == nil {
		return
	}
	live := 0
	for _, instance := range result.Instances {
		if instance.Live() {
			live++
		}
	}
	pressure := result.Host.Pressure
	facts := sessionYieldFacts{At: result.At, AdmissionAllowed: pressure.AdmissionAllowed,
		LiveInstances: live, BusyOperations: busy}
	yielded := e.yield.Apply(ctx, facts, pressure.AdmissionReason)
	bindings, withdrawn := e.yield.Bindings()
	e.health.SetSessionYield(telemetry.SessionYieldMetric{Yielded: yielded, Reason: pressure.AdmissionReason,
		Since: e.yield.Since(), Bindings: bindings, Withdrawn: withdrawn})
}

// operationFailureMetrics converts the durable failure aggregate into telemetry
// values. A daemon without the aggregate port simply publishes none.
func (e engineTicker) operationFailureMetrics(ctx context.Context) ([]telemetry.OperationFailure, error) {
	if e.operationFailures == nil {
		return nil, nil
	}
	durable, err := e.operationFailures(ctx)
	if err != nil {
		return nil, err
	}
	failures := make([]telemetry.OperationFailure, 0, len(durable))
	for _, failure := range durable {
		failures = append(failures, telemetry.OperationFailure{Kind: failure.Kind, Code: failure.Code,
			Count: failure.Count, Attempts: failure.Attempts})
	}
	return failures, nil
}

// deadLetterMetrics identifies the parked operations from this tick's own
// inventory. Parked means two things at once, and only this seam can see both:
// the durable side proves no operation for the resource is pending or claimed,
// and the inventory side proves the owning VM is PROVEN IDLE — enumerated by a
// successful Tart read as powered off, or not enumerated at all.
//
// Proof is required rather than merely "not running". An unknown power state is
// never treated as parked, so an instance the fleet cannot see clearly keeps
// deferring release updates, and a resource with no live instance row at all is
// not parked either — there is nothing left for an operator to reclaim, and
// counting it would let the quiescence gate discount capacity that is still real.
// An absent VM is admitted for the same reason a stopped one is, and it is the
// stronger evidence of the two: a generation swap cannot interrupt a guest that
// does not exist. Withholding it would let the wedge ADR 0021 removed return in a
// new shape — a dead letter whose VM is already gone disabling updates forever.
func (e engineTicker) deadLetterMetrics(ctx context.Context, instances []domain.Instance) ([]telemetry.DeadLetter, error) {
	if e.deadLetters == nil {
		return nil, nil
	}
	durable, err := e.deadLetters(ctx)
	if err != nil {
		return nil, err
	}
	provenIdle := make(map[string]struct{}, len(instances))
	for _, instance := range instances {
		if instance.Live() && instance.Power.ProvenIdle() {
			provenIdle[instance.ID] = struct{}{}
		}
	}
	letters := make([]telemetry.DeadLetter, 0, len(durable))
	for _, letter := range durable {
		_, idle := provenIdle[letter.ResourceID]
		letters = append(letters, telemetry.DeadLetter{OperationID: letter.OperationID, Kind: letter.Kind,
			Code: letter.Code, ResourceID: letter.ResourceID, Attempts: letter.Attempts,
			Parked: idle && !letter.ResourceProgressing})
	}
	return letters, nil
}

// stalledMetrics converts the durable stall projection into telemetry values. A
// daemon without the port simply publishes none, and an unreadable read degrades
// the operations observation rather than publishing "everything is progressing".
func (e engineTicker) stalledMetrics(ctx context.Context) ([]telemetry.Stalled, error) {
	if e.stalledOperations == nil {
		return nil, nil
	}
	now := time.Now().UTC()
	if e.now != nil {
		now = e.now().UTC()
	}
	durable, err := e.stalledOperations(ctx, now)
	if err != nil {
		return nil, err
	}
	stalled := make([]telemetry.Stalled, 0, len(durable))
	for _, row := range durable {
		stalled = append(stalled, telemetry.Stalled{Operation: row.OperationID, Kind: row.Kind, Code: row.Code,
			Instance: row.Instance, Attempts: row.Attempts, Retrying: row.Retrying,
			DrainState: row.DrainState, Held: row.Held})
	}
	return stalled, nil
}

// queueTierMetrics carries the per-tier breakdown to telemetry. An empty
// breakdown is a fleet that declares no priority tier, and it publishes nothing.
func queueTierMetrics(tiers []app.QueueTier) []telemetry.QueueTierMetrics {
	if len(tiers) == 0 {
		return nil
	}
	rows := make([]telemetry.QueueTierMetrics, 0, len(tiers))
	for _, tier := range tiers {
		rows = append(rows, telemetry.QueueTierMetrics{Tier: tier.Tier, Rank: tier.Rank,
			Count: tier.Count, OldestEnqueuedAt: tier.Oldest})
	}
	return rows
}

func (e engineTicker) recordMetrics(result app.TickResult) {
	queues := make(map[string]struct {
		count  int
		oldest time.Time
	}, len(e.profiles))
	instances := make(map[string]struct{ count, cpu, memory int }, len(e.profiles))
	for _, profile := range e.profiles {
		queues[profile] = struct {
			count  int
			oldest time.Time
		}{}
		instances[profile] = struct{ count, cpu, memory int }{}
	}
	for _, demand := range result.Demands {
		metric := queues[string(demand.Profile)]
		metric.count++
		if metric.oldest.IsZero() || demand.CreatedAt.Before(metric.oldest) {
			metric.oldest = demand.CreatedAt
		}
		queues[string(demand.Profile)] = metric
	}
	for profile, summary := range result.Queues {
		queues[string(profile)] = struct {
			count  int
			oldest time.Time
		}{count: summary.Count, oldest: summary.Oldest}
	}
	// The per-scope breakdown is published as a whole set: a binding removed from
	// configuration must stop being reported, which a partial update cannot say.
	scopeRows := make([]telemetry.ScopeQueueMetrics, 0, len(result.ScopeQueues))
	for _, row := range result.ScopeQueues {
		scopeRows = append(scopeRows, telemetry.ScopeQueueMetrics{Scope: row.Scope,
			Profile: string(row.Profile), ScaleSetID: row.ScaleSetID, Count: row.Count,
			OldestEnqueuedAt: row.Oldest, Tiers: queueTierMetrics(row.Tiers)})
	}
	_ = e.health.SetScopeQueues(scopeRows)
	for _, instance := range result.Instances {
		if !instance.Live() {
			continue
		}
		metric := instances[string(instance.Profile)]
		metric.count++
		metric.cpu += instance.Resources.CPU
		metric.memory += instance.Resources.MemoryMB
		instances[string(instance.Profile)] = metric
	}
	for _, profile := range e.profiles {
		queue := queues[profile]
		_ = e.health.SetQueue(profile, queue.count, queue.oldest)
		instance := instances[profile]
		_ = e.health.SetInstances(profile, instance.count, instance.cpu, instance.memory)
	}
	mode := telemetry.ModeIdle
	switch result.HostMode {
	case domain.HostLinux:
		mode = telemetry.ModeLinux
	case domain.HostMacOS:
		mode = telemetry.ModeMacOS
	case domain.HostMixed:
		mode = telemetry.ModeMixed
	}
	_ = e.health.SetMode(mode)
	e.recordOccupancy(result)
	e.recordGuestLiveness(result)
	e.recordRecoveries(result)
	e.recordReservation(result)
	pressure := result.Host.Pressure
	if pressure.AdmissionReason != "" {
		_ = e.health.SetHostPressure(telemetry.HostPressureMetric{AvailableMemoryMiB: pressure.AvailableMemoryMB,
			FreeDiskGiB: pressure.FreeDiskGB, SwapUsedMiB: pressure.SwapUsedMB, SwapOuts: pressure.SwapOuts,
			SwapOutRatePerSecond: pressure.SwapOutRatePerSecond, SwapOutRateObserved: pressure.SwapOutRateObserved,
			CPUIdlePercent: pressure.CPUIdlePercent, LoadAverage: pressure.LoadAverage,
			AdmissionAllowed: pressure.AdmissionAllowed, AdmissionReason: pressure.AdmissionReason})
	}
}

// recordOccupancy publishes how long each instance has held its vector and says
// out loud when one is approaching or past its ceiling. Both readings come from
// scheduler.Occupancies — the same pure projection the reap itself is planned
// from — so the metric, the warning, the doctor finding and the drain can never
// disagree about a hold.
func (e engineTicker) recordOccupancy(result app.TickResult) {
	occupancy := scheduler.Occupancies(result.At, e.engine.Config, result.Instances, result.Demands)
	metrics := make([]telemetry.OccupancyMetric, 0, len(occupancy))
	for _, hold := range occupancy {
		metrics = append(metrics, telemetry.OccupancyMetric{Instance: hold.Instance, Profile: string(hold.Profile),
			Repo: hold.Repo, CPU: hold.Resources.CPU, MemoryMiB: hold.Resources.MemoryMB, Age: hold.Age,
			Budget: hold.Budget, Warned: hold.Warned, OverBudget: hold.OverBudget,
			StarvesQueuedDemand: hold.StarvesQueuedDemand})
	}
	_ = e.health.SetOccupancy(metrics)
	if e.reporter == nil {
		return
	}
	e.reporter.reportOccupancy(occupancy)
	for _, operation := range result.Plan.Operations {
		e.reporter.reportOccupancyReclaim(operation, occupancy)
	}
}

// recordGuestLiveness publishes every guest that has stopped answering and says
// out loud when one is declared dead. Both readings come from
// scheduler.GuestSilences — the same pure projection the reclaim itself is
// planned from — so the metric, the warning, the doctor finding and the drain
// can never disagree about a silence.
func (e engineTicker) recordGuestLiveness(result app.TickResult) {
	silences := scheduler.GuestSilences(result.At, e.engine.Config, result.Instances)
	metrics := make([]telemetry.GuestSilenceMetric, 0, len(silences))
	for _, silence := range silences {
		metrics = append(metrics, telemetry.GuestSilenceMetric{Instance: silence.Instance,
			Profile: string(silence.Profile), Repo: silence.Repo, CPU: silence.Resources.CPU,
			MemoryMB: silence.Resources.MemoryMB, Refusals: silence.Refusals, Silence: silence.Silence,
			RequiredRefusals: silence.RequiredRefusals, Window: silence.Window,
			Unresponsive: silence.Unresponsive, RunID: silence.Demand.RunID, JobID: silence.Demand.JobID})
	}
	_ = e.health.SetGuestSilences(metrics)
	if e.reporter == nil {
		return
	}
	e.reporter.reportGuestSilence(silences)
	for _, operation := range result.Plan.Operations {
		e.reporter.reportGuestReclaim(operation, silences)
	}
}

// recordReservation publishes the vector the scheduler is holding for its aged
// global-FIFO head, and WHICH of the two axes is holding that head out.
//
// Nothing published this before, and that is why issue #226 ran in production
// unobserved: a reserved head its own repository cap held sterilized the
// residual for the whole runtime of the blocking job, and no artifact named the
// reservation, its repository, or the axis. `grep reservation` over the
// authority log returned nothing, because there was nothing to find. A
// deterministic simulator caught it; the fleet itself could not have.
//
// The axis comes from the plan that decided it rather than being recomputed
// here, so the diagnosis an operator reads is the one the scheduler acted on.
func (e engineTicker) recordReservation(result app.TickResult) {
	reservation := result.Plan.Next.Reservation
	if reservation == nil {
		_ = e.health.SetReservation(nil)
		return
	}
	held := time.Duration(0)
	if !reservation.Since.IsZero() && result.At.After(reservation.Since) {
		held = result.At.Sub(reservation.Since)
	}
	axis := result.Plan.ReservationAxis
	_ = e.health.SetReservation(&telemetry.ReservationMetric{
		Demand: reservation.Demand.String(), Repo: reservation.Demand.Repo,
		Profile: string(reservation.Profile), CPU: reservation.Resources.CPU,
		MemoryMiB: reservation.Resources.MemoryMB, Slots: reservation.Resources.Slots,
		Held: held, Axis: string(axis),
	})
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now().UTC() }

var _ operations.Store = (*sqlite.Store)(nil)
