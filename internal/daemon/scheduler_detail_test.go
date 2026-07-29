package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/reconcile"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/telemetry"
)

// blockedPlanEngine builds an engine whose host observation is stale, so the
// scheduler fails closed with a blocked plan and no error.
func blockedPlanEngine(t *testing.T) (*telemetry.Health, app.Engine) {
	t.Helper()
	d := testDependencies(t)
	store, err := d.openStore(context.Background(), filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	engine := app.Engine{Store: store, Inventory: staleInventory{now: now},
		Config: app.BuildSchedulerConfig(config.Default()), ControllerID: "c",
		Mode: reconcile.Observe, Now: func() time.Time { return now }}
	health, err := telemetryHealthForTest()
	if err != nil {
		t.Fatal(err)
	}
	return health, engine
}

// The Observation DTO carries a Detail field precisely so a non-fresh
// observation can explain itself, and docs/OPERATIONS.md states that stderr and
// the admin API agree on the reason. On the scheduler error path they did not.
//
// engineTicker recorded `result.Plan.Reason` as the detail, but every error
// return from Engine.Tick yields a zero TickResult, so the plan reason is empty
// exactly when the tick failed. `detail` was therefore omitted from the API for
// every genuine scheduler failure, while a blocked plan -- which is not a
// failure -- was the only case that ever populated it.
//
// Sampled on the production host, the failures arrive in bursts of consecutive
// ticks reported as `freshness: "unavailable"` with no detail, so the admin API
// could not name a cause even while the failure was still live.

// TestEngineTickerRecordsClassifiedFailureAsDetail is the RED-first case: a
// classified tick error must reach the observation detail, so `status`,
// `doctor`, and `observations` explain the failure instead of only reporting
// that one happened.
func TestEngineTickerRecordsClassifiedFailureAsDetail(t *testing.T) {
	// A zero Engine fails wiring validation, which app classifies as
	// engine_invalid. Any classified error exercises the same path.
	health, _ := telemetryHealthForTest()
	err := (engineTicker{engine: app.Engine{}, health: health}).Tick(context.Background())
	if err == nil {
		t.Fatal("Tick() unexpectedly succeeded; the failure path is not exercised")
	}

	observation := health.Snapshot().Observations["scheduler"]
	if observation.Freshness == "fresh" {
		t.Fatalf("a failed tick recorded a fresh observation: %#v", observation)
	}
	if observation.Detail != app.ReasonEngineInvalid {
		t.Fatalf("scheduler observation detail = %q, want %q so the API names the cause",
			observation.Detail, app.ReasonEngineInvalid)
	}
}

// TestEngineTickerKeepsPlanReasonForBlockedPlans proves the change is additive:
// a blocked plan is not an error, and its existing plan-reason detail must keep
// flowing rather than being displaced by failure classification.
func TestEngineTickerKeepsPlanReasonForBlockedPlans(t *testing.T) {
	health, engine := blockedPlanEngine(t)
	if err := (engineTicker{engine: engine, health: health}).Tick(context.Background()); err != nil {
		t.Fatalf("a blocked plan must not be an error: %v", err)
	}
	observation := health.Snapshot().Observations["scheduler"]
	if observation.Freshness == "fresh" {
		t.Fatalf("a blocked plan must not read as fresh: %#v", observation)
	}
	if observation.Detail == "" {
		t.Fatal("a blocked plan lost its plan-reason detail")
	}
	// A blocked plan carries the scheduler's own reason, never a failure token.
	if observation.Detail == app.ReasonEngineInvalid {
		t.Fatalf("blocked-plan detail was displaced by a failure reason: %q", observation.Detail)
	}
}
