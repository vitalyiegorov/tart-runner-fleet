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
	"strings"
	"sync"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/macos"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/sqlite"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/tart"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/credentials"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/discharge"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/lifecycle"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/reconcile"
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
	openConfig      func(string) (io.ReadCloser, error)
	openStore       func(context.Context, string) (runtimeStore, error)
	loadKey         func(context.Context, string, string, string) (*credentials.Secret, error)
	newScaleSet     func(context.Context, githubscaleset.GitHubAppScaleSetConfig) (scaleSetSource, error)
	newRESTObserver func(githubscaleset.ObserverConfig) (queueObserver, error)
	inventory       func(runtimeStore, config.Config, app.RecoveryObserver) app.Inventory
	listen          func(string, string) (net.Listener, error)
	adminListen     func(string) (net.Listener, error)
	cursor          func(context.Context, runtimeStore, int64) (int64, error)
	newVM           func(runtimeStore, config.Config, lifecycle.DrainControl) lifecycle.VMControl
	newReaper       func(runtimeStore, config.Config) discharge.VM
	readiness       func(config.Config) lifecycle.Readiness
	bootstrap       func(config.Config) lifecycle.Bootstrapper
	now             func() time.Time
	after           func(time.Duration) <-chan time.Time
	leaseOwner      func(config.Config) string
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
	deletionConfirmationMaxAge = 30 * time.Second
	scaleSetCloseTimeout       = 20 * time.Second
	scaleSetCloseConcurrency   = 4
	lifecycleRetryMaxAttempts  = 0
	provisionRetryMaximum      = 30 * time.Second
	drainRetryMaximum          = 30 * time.Second
)

var (
	errScaleSetClose = errors.New("scale-set session cleanup failed")
	errScaleSetOpen  = errors.New("recreate scale-set session failed")
)

func defaultDependencies() dependencies {
	return dependencies{
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
		inventory: func(store runtimeStore, cfg config.Config, recovery app.RecoveryObserver) app.Inventory {
			return app.ProductionInventory{Store: store,
				Tart:                       &tart.Adapter{CommandTimeout: cfg.Timeouts.Tart, StartTimeout: cfg.Timeouts.Boot},
				Host:                       &macos.Probe{Timeout: cfg.Timeouts.Tart, PressureAccounting: cfg.Guards.PressureMemoryAccounting},
				Recovery:                   recovery,
				RecoveryConfirmationMaxAge: deletionConfirmationMaxAge,
				Capacity:                   domain.Resources{CPU: cfg.Linux.Capacity.CPU, MemoryMB: cfg.Linux.Capacity.MemoryMiB, Slots: cfg.Linux.MaxInstances},
				Guards: macos.Guardrails{MinFreeDiskGB: int64(cfg.Guards.MinFreeDiskGiB),
					MinAvailableMemoryMB: int64(cfg.Guards.MinAvailableMemoryMiB), MaxSwapUsedMB: int64(cfg.Guards.MaxSwapUsedMiB),
					MaxLoadAverage: cfg.Guards.MaxLoadAverage, MinCPUidlePercent: cfg.Guards.MinCPUIdlePercent},
				ElasticHostEnvelope: cfg.Guards.ElasticHostEnvelope}
		},
		listen:      net.Listen,
		adminListen: adminapi.Listen,
		cursor: func(ctx context.Context, store runtimeStore, id int64) (int64, error) {
			return store.DemandCursor(ctx, id)
		},
		newVM: func(store runtimeStore, cfg config.Config, control lifecycle.DrainControl) lifecycle.VMControl {
			return &tart.Adapter{Ownership: store, Confirmation: control, CommandTimeout: cfg.Timeouts.Tart,
				StartTimeout: cfg.Timeouts.Boot, ConfirmationMaxAge: deletionConfirmationMaxAge,
				MacOSVMPrefixes:      []string{"trf-" + strings.ToLower(cfg.MacOS.Builder.ID) + "-", "trf-" + strings.ToLower(cfg.MacOS.Maestro.ID) + "-"},
				MacOSRootDiskOptions: cfg.MacOS.RootDiskOptions, MacOSSharedDirectoryPath: cfg.MacOS.SharedDirectoryPath,
				LinuxNestedVirtualization: cfg.Linux.NestedVirtualization}
		},
		// newReaper builds the discharge path's own Tart port. It deliberately omits
		// Confirmation: Reap does not consult GitHub runner evidence, because the
		// case it exists for is a registration that can never be confirmed released.
		newReaper: func(store runtimeStore, cfg config.Config) discharge.VM {
			return &tart.Adapter{Ownership: store, CommandTimeout: cfg.Timeouts.Tart, StartTimeout: cfg.Timeouts.Boot}
		},
		readiness: func(cfg config.Config) lifecycle.Readiness {
			return tartReadiness{Runner: tart.ExecRunner{}, Timeout: cfg.Timeouts.Boot,
				AttemptTimeout: cfg.Timeouts.Tart, RetryInterval: 250 * time.Millisecond, After: time.After}
		},
		bootstrap: func(cfg config.Config) lifecycle.Bootstrapper {
			return lifecycle.StdinBootstrapper{Runner: lifecycle.ExecStdinRunner{Binary: "tart"}, Timeout: cfg.Timeouts.Tart}
		},
		now:   time.Now,
		after: time.After,
		leaseOwner: func(cfg config.Config) string {
			owner := cfg.GitHub.SessionOwner
			if owner == "" {
				owner = cfg.GitHub.Owner
			}
			return fmt.Sprintf("%s/%d", owner, os.Getpid())
		},
	}
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
		if err := cfg.ValidateAuthority(); err != nil {
			return err
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
		CriticalObservations: criticalObservations, CriticalObservationTTL: 2 * time.Minute})
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

	coordinator := app.DemandCoordinator{Store: store, Now: d.now, StatisticsMaxAge: 2 * time.Minute,
		StrictJobRouting: opts.Mode != reconcile.Canary}
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
					health: health, observation: fmt.Sprintf("github-%d", binding.StoreKey)})
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
	engine := app.Engine{Store: store, Demand: coordinator, Inventory: d.inventory(store, cfg, recovery), Config: schedulerConfig,
		Bindings: bindings, ControllerID: "tart-runner-fleet", Mode: opts.Mode}
	ticker := engineTicker{engine: engine, health: health, profiles: profiles, operationCounts: store.OperationCounts,
		operationFailures: store.OperationFailures, deadLetters: store.DeadLetters}
	var worker app.WorkRunner
	if opts.Mode == reconcile.Canary || opts.Mode == reconcile.Authority {
		vm := d.newVM(store, cfg, control)
		diskGiB := profileDiskFloors(cfg)
		worker = operationRunner{worker: operations.Worker{
			Store: store, Owner: controllerLeaseOwner, MaxConcurrent: workerConcurrency(opts.Mode, cfg),
			Executors: map[string]operations.Executor{
				lifecycle.OperationProvision: lifecycle.ProvisionExecutor{State: store, VM: vm, Ready: d.readiness(cfg),
					Registration: control, Bootstrap: d.bootstrap(cfg), Bases: map[domain.Platform]string{
						domain.PlatformLinux: cfg.Linux.BaseVM, domain.PlatformMacOS: cfg.MacOS.BaseVM}, DiskGiB: diskGiB},
				lifecycle.OperationDrain: lifecycle.DrainExecutor{State: store, VM: vm, Control: control,
					ConfirmationMaxAge: deletionConfirmationMaxAge, Now: d.now},
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
	reporter := newFailureReporter(os.Stderr, d.now)
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

type tartReadiness struct {
	Runner         tart.Runner
	Timeout        time.Duration
	AttemptTimeout time.Duration
	RetryInterval  time.Duration
	After          func(time.Duration) <-chan time.Time
}

func (p tartReadiness) Wait(ctx context.Context, instance operations.Instance) error {
	if p.Runner == nil || tart.ValidateName(instance.ID) != nil || p.Timeout <= 0 || p.AttemptTimeout < 0 || p.RetryInterval < 0 {
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

func (g *authorityLease) renew(ctx context.Context, after func(time.Duration) <-chan time.Time) {
	defer close(g.done)
	for {
		select {
		case <-ctx.Done():
			return
		case <-after(authorityLeaseRenewInterval):
			g.mu.Lock()
			lease := g.lease
			g.mu.Unlock()
			renewCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			renewed, err := g.store.RenewLease(renewCtx, lease, g.now().UTC(), authorityLeaseTTL)
			cancel()
			if err != nil {
				g.errors <- err
				return
			}
			g.mu.Lock()
			g.lease = renewed
			g.mu.Unlock()
		}
	}
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
	changed, err := b.coordinator.IngestOnceResult(ctx, b.binding, b.source)
	if b.health != nil && b.observation != "" {
		freshness := telemetry.ObservationFresh
		if err != nil {
			freshness = telemetry.ObservationUnavailable
		}
		// The detail is closed vocabulary. It explains why one binding's
		// ingestion failed without exposing a token, a JIT configuration, or an
		// upstream response body.
		_ = b.health.RecordObservationDetail(b.observation, freshness, githubscaleset.IngestFailureDetail(err))
	}
	return changed, err
}

// failureReporter turns app.Service's secret-safe failure callback into a
// rate-limited log line. The callback receives only a static component name by
// design, so nothing an upstream error carries can reach the log. Emitting at
// most one line per component per window keeps a hot failure loop from
// drowning the daemon's stderr while still breaking the historical silence
// that once hid an 18h scheduler outage.
type failureReporter struct {
	mu     sync.Mutex
	last   map[string]time.Time
	now    func() time.Time
	window time.Duration
	logger *slog.Logger
}

func newFailureReporter(w io.Writer, now func() time.Time) *failureReporter {
	if now == nil {
		now = time.Now
	}
	return &failureReporter{last: make(map[string]time.Time), now: now, window: time.Minute,
		logger: slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelWarn}))}
}

func (r *failureReporter) report(component, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	// Rate limiting keys on component and reason together. The reason vocabulary
	// is closed, so cardinality stays bounded while a changed reason — the
	// signal an operator needs — is never suppressed by an older one.
	key := component + "\x00" + reason
	if last, ok := r.last[key]; ok && now.Sub(last) < r.window {
		return
	}
	r.last[key] = now
	if reason == "" {
		r.logger.Warn("component loop failure", "component", component)
		return
	}
	r.logger.Warn("component loop failure", "component", component, "reason", reason)
}

type engineTicker struct {
	engine            app.Engine
	health            *telemetry.Health
	profiles          []string
	operationCounts   func(context.Context) (int, int, error)
	operationFailures func(context.Context) ([]operations.OperationFailure, error)
	deadLetters       func(context.Context) ([]operations.DeadLetter, error)
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
		if e.operationCounts != nil {
			retrying, dead, countErr := e.operationCounts(ctx)
			failures, failureErr := e.operationFailureMetrics(ctx)
			letters, letterErr := e.deadLetterMetrics(ctx, result.Instances)
			if countErr != nil || failureErr != nil || letterErr != nil {
				// Rule 4: an unreadable aggregate degrades the observation. It never
				// publishes an empty failure set, which would read as a healed fleet, and
				// never an empty dead-letter set, which would read as "nothing is parked"
				// and wrongly release the update quiescence gate.
				_ = e.health.RecordObservation("operations", telemetry.ObservationUnavailable)
			} else {
				_ = e.health.SetOperations(retrying, dead)
				_ = e.health.SetOperationFailures(failures)
				_ = e.health.SetDeadLetters(letters)
				_ = e.health.RecordObservation("operations", telemetry.ObservationFresh)
			}
		}
	}
	return err
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
			OldestEnqueuedAt: row.Oldest})
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
	pressure := result.Host.Pressure
	if pressure.AdmissionReason != "" {
		_ = e.health.SetHostPressure(telemetry.HostPressureMetric{AvailableMemoryMiB: pressure.AvailableMemoryMB,
			FreeDiskGiB: pressure.FreeDiskGB, SwapUsedMiB: pressure.SwapUsedMB, SwapOuts: pressure.SwapOuts,
			CPUIdlePercent: pressure.CPUIdlePercent, LoadAverage: pressure.LoadAverage,
			AdmissionAllowed: pressure.AdmissionAllowed, AdmissionReason: pressure.AdmissionReason})
	}
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now().UTC() }

var _ operations.Store = (*sqlite.Store)(nil)
