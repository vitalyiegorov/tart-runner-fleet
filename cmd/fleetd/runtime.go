package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/macos"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/sqlite"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/tart"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/credentials"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/reconcile"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/telemetry"
)

type runtimeStore interface {
	app.EngineStore
	app.LiveInstanceStore
	DemandCursor(context.Context, int64) (int64, error)
	OperationCounts(context.Context) (int, int, error)
	Close() error
}

type scaleSetSource interface {
	app.MessageSource
	Close(context.Context) error
}

type dependencies struct {
	openConfig  func(string) (io.ReadCloser, error)
	openStore   func(context.Context, string) (runtimeStore, error)
	loadKey     func(context.Context, string, string) (*credentials.Secret, error)
	newScaleSet func(context.Context, githubscaleset.GitHubAppScaleSetConfig) (scaleSetSource, error)
	inventory   func(runtimeStore, config.Config) app.Inventory
	listen      func(string, string) (net.Listener, error)
	adminListen func(string) (net.Listener, error)
	cursor      func(context.Context, runtimeStore, int64) (int64, error)
}

var deps = defaultDependencies()

func defaultDependencies() dependencies {
	return dependencies{
		openConfig: func(path string) (io.ReadCloser, error) { return os.Open(path) },
		openStore:  func(ctx context.Context, path string) (runtimeStore, error) { return sqlite.Open(ctx, path) },
		loadKey: func(ctx context.Context, service, account string) (*credentials.Secret, error) {
			return (credentials.Keychain{}).Load(ctx, service, account)
		},
		newScaleSet: func(ctx context.Context, cfg githubscaleset.GitHubAppScaleSetConfig) (scaleSetSource, error) {
			return githubscaleset.NewGitHubAppScaleSet(ctx, cfg)
		},
		inventory: func(store runtimeStore, cfg config.Config) app.Inventory {
			return app.ProductionInventory{Store: store, Tart: &tart.Adapter{}, Host: &macos.Probe{},
				Capacity: domain.Resources{CPU: cfg.Linux.Capacity.CPU, MemoryMB: cfg.Linux.Capacity.MemoryMiB, Slots: cfg.Linux.MaxInstances},
				Guards:   macos.Guardrails{MinFreeDiskGB: int64(cfg.Guards.MinFreeDiskGiB)}}
		},
		listen:      net.Listen,
		adminListen: adminapi.Listen,
		cursor: func(ctx context.Context, store runtimeStore, id int64) (int64, error) {
			return store.DemandCursor(ctx, id)
		},
	}
}

func runDaemon(ctx context.Context, opts options) error { return runWithDependencies(ctx, opts, deps) }

func runWithDependencies(ctx context.Context, opts options, d dependencies) error {
	if opts.Mode != reconcile.Observe && opts.Mode != reconcile.Shadow {
		return errors.New("only observe and shadow modes are armed")
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
	if opts.Mode == reconcile.Shadow {
		if err := cfg.ValidateAuthority(); err != nil {
			return err
		}
	}
	store, err := d.openStore(ctx, opts.DatabasePath)
	if err != nil {
		return err
	}
	defer store.Close()
	schedulerConfig := app.BuildSchedulerConfig(cfg)
	bindings, err := app.BuildBindings(cfg, schedulerConfig)
	if err != nil {
		return err
	}

	profiles := make([]string, 0, len(schedulerConfig.Profiles))
	for id := range schedulerConfig.Profiles {
		profiles = append(profiles, string(id))
	}
	health, _ := telemetry.NewHealth(wallClock{}, telemetry.HealthConfig{Profiles: profiles, CriticalObservations: []string{"operations", "scheduler"}})
	serverConfig := telemetry.ServerConfig{ControllerVersion: version, ControllerMode: string(opts.Mode)}
	healthServer, _ := telemetry.NewServer(health, serverConfig)
	listener, err := d.listen("tcp", opts.HealthAddress)
	if err != nil {
		return fmt.Errorf("listen health: %w", err)
	}
	defer listener.Close()
	adminServer, _ := telemetry.NewServer(health, serverConfig)
	adminListener, err := d.adminListen(opts.AdminSocket)
	if err != nil {
		return fmt.Errorf("listen admin: %w", err)
	}
	defer adminListener.Close()
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

	coordinator := app.DemandCoordinator{Store: store}
	ingesters := make([]app.Ingester, 0, len(bindings))
	closers := make([]scaleSetSource, 0, len(bindings))
	defer func() {
		for _, source := range closers {
			_ = source.Close(context.Background())
		}
	}()
	if opts.Mode == reconcile.Shadow {
		secret, err := d.loadKey(ctx, cfg.GitHub.KeychainService, cfg.GitHub.KeychainAccount)
		if err != nil {
			return err
		}
		privateKey := githubscaleset.NewPrivateKeySecret(secret.Reveal())
		secret.Destroy()
		defer privateKey.Destroy()
		for index, binding := range bindings {
			cursor, err := d.cursor(ctx, store, binding.ScaleSetID)
			if err != nil {
				return err
			}
			scale := cfg.GitHub.ScaleSets[index]
			source, err := d.newScaleSet(ctx, githubscaleset.GitHubAppScaleSetConfig{GitHubConfigURL: cfg.GitHub.ConfigURL,
				ClientID: cfg.GitHub.ClientID, InstallationID: cfg.GitHub.InstallationID, PrivateKey: privateKey,
				ScaleSetID: scale.ID, Owner: cfg.GitHub.Owner, MaxCapacity: scale.MaxCapacity, InitialCursor: int(cursor),
				System: "tart-runner-fleet", Version: version, Subsystem: "controller"})
			if err != nil {
				return err
			}
			closers = append(closers, source)
			ingesters = append(ingesters, boundIngester{coordinator: coordinator, binding: binding, source: source})
		}
	}
	engine := app.Engine{Store: store, Demand: coordinator, Inventory: d.inventory(store, cfg), Config: schedulerConfig,
		Bindings: bindings, ControllerID: "tart-runner-fleet", Mode: opts.Mode}
	ticker := engineTicker{engine: engine, health: health, profiles: profiles, operationCounts: store.OperationCounts}
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	serviceDone := make(chan error, 1)
	go func() {
		serviceDone <- (app.Service{Ticker: ticker, Ingesters: ingesters, TickInterval: cfg.PollInterval}).Run(runCtx)
	}()
	select {
	case err := <-serviceDone:
		return err
	case result := <-serverDone:
		cancelRun()
		<-serviceDone
		return fmt.Errorf("%s %w", result.name, serverFailure(result.err))
	}
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
}

func (b boundIngester) Ingest(ctx context.Context) error {
	return b.coordinator.IngestOnce(ctx, b.binding, b.source)
}

type engineTicker struct {
	engine          app.Engine
	health          *telemetry.Health
	profiles        []string
	operationCounts func(context.Context) (int, int, error)
}

func (e engineTicker) Tick(ctx context.Context) error {
	result, err := e.engine.Tick(ctx)
	success := err == nil && result.Plan.Status == "ready"
	e.health.RecordTick(success)
	freshness := telemetry.ObservationFresh
	if err != nil {
		freshness = telemetry.ObservationUnavailable
	} else if !success {
		freshness = telemetry.ObservationStale
	}
	_ = e.health.RecordObservation("scheduler", freshness)
	if err == nil {
		e.recordMetrics(result)
		if e.operationCounts != nil {
			retrying, dead, countErr := e.operationCounts(ctx)
			if countErr != nil {
				_ = e.health.RecordObservation("operations", telemetry.ObservationUnavailable)
			} else {
				_ = e.health.SetOperations(retrying, dead)
				_ = e.health.RecordObservation("operations", telemetry.ObservationFresh)
			}
		}
	}
	return err
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
	if result.HostMode == domain.HostLinux {
		mode = telemetry.ModeLinux
	} else if result.HostMode == domain.HostMacOS {
		mode = telemetry.ModeMacOS
	}
	_ = e.health.SetMode(mode)
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now().UTC() }

var _ operations.Store = (*sqlite.Store)(nil)
