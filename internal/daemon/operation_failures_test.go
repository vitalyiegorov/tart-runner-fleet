package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/reconcile"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/telemetry"
)

func tickerFixture(t *testing.T) (app.Engine, *telemetry.Health) {
	t.Helper()
	d := testDependencies(t)
	store, err := d.openStore(context.Background(), filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	engine := app.Engine{Store: store, Inventory: runtimeInventory{}, Config: app.BuildSchedulerConfig(config.Default()),
		ControllerID: "c", Mode: reconcile.Observe, Now: func() time.Time { return now }}
	health, err := telemetry.NewHealth(wallClock{}, telemetry.HealthConfig{CriticalObservations: []string{"operations", "scheduler"}})
	if err != nil {
		t.Fatal(err)
	}
	return engine, health
}

// Publishing the durable failure aggregate is what turns "retrying 1" into
// "deregister:runner_busy, 397 attempts" for the operator.
func TestEngineTickerPublishesOperationFailures(t *testing.T) {
	engine, health := tickerFixture(t)
	ticker := engineTicker{engine: engine, health: health,
		operationCounts: func(context.Context) (int, int, error) { return 1, 0, nil },
		operationFailures: func(context.Context) ([]operations.OperationFailure, error) {
			return []operations.OperationFailure{{Kind: "deregister", Code: "deregister:runner_busy", Count: 1, Attempts: 397}}, nil
		}}

	if err := ticker.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	failures := health.Snapshot().OperationFailures
	if len(failures) != 1 || failures[0].Code != "deregister:runner_busy" || failures[0].Attempts != 397 {
		t.Fatalf("snapshot failures=%#v", failures)
	}
}

// Rule 4: an unavailable aggregate may never be published as "no failures". The
// operations observation degrades instead, keeping the last known aggregate.
func TestEngineTickerNeverPublishesAnUnavailableAggregateAsEmpty(t *testing.T) {
	engine, health := tickerFixture(t)
	known := []operations.OperationFailure{{Kind: "deregister", Code: "deregister:runner_forbidden", Count: 1, Attempts: 7}}
	failing := false
	ticker := engineTicker{engine: engine, health: health,
		operationCounts: func(context.Context) (int, int, error) { return 1, 0, nil },
		operationFailures: func(context.Context) ([]operations.OperationFailure, error) {
			if failing {
				return nil, errors.New("db unavailable")
			}
			return known, nil
		}}
	if err := ticker.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}

	failing = true
	if err := ticker.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}

	if got := health.Snapshot().Observations["operations"].Freshness; got != telemetry.ObservationUnavailable {
		t.Fatalf("operations freshness=%s", got)
	}
	if failures := health.Snapshot().OperationFailures; len(failures) != 1 || failures[0].Attempts != 7 {
		t.Fatalf("unavailable aggregate overwrote known failures: %#v", failures)
	}
}
