package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/reconcile"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/telemetry"
)

// engineOver builds an authority engine over the given inventory plus a fresh
// health recorder, so each scenario differs only by what the inventory reports.
func engineOver(t *testing.T, inventory app.Inventory) (*telemetry.Health, app.Engine) {
	t.Helper()
	d := testDependencies(t)
	store, err := d.openStore(context.Background(), filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	engine := app.Engine{Store: store, Inventory: inventory,
		Config: app.BuildSchedulerConfig(config.Default()), ControllerID: "c",
		Mode: reconcile.Authority, Now: func() time.Time { return now }}
	health, err := telemetryHealthForTest()
	if err != nil {
		t.Fatal(err)
	}
	return health, engine
}

// blockedPlanEngine reports a stale host, so the scheduler fails closed with a
// blocked plan and no error.
func blockedPlanEngine(t *testing.T) (*telemetry.Health, app.Engine) {
	t.Helper()
	return engineOver(t, staleInventory{now: time.Now().UTC()})
}

// invalidPlanEngine reports an instance with an unrecognized platform, so the
// scheduler forms an invalid plan and Commit fails while Plan.Reason carries the
// domain explanation.
func invalidPlanEngine(t *testing.T) (*telemetry.Health, app.Engine) {
	t.Helper()
	return engineOver(t, unknownPlatformInventory{now: time.Now().UTC()})
}

type unknownPlatformInventory struct{ now time.Time }

func (u unknownPlatformInventory) Observe(context.Context) (domain.Observation[[]domain.Instance], domain.Observation[domain.Host]) {
	odd := domain.Instance{ID: "trf-weird", Repo: "a/repo", Platform: domain.Platform("plan9"),
		Profile: "small", Route: "tiered", Resources: domain.Resources{CPU: 1, MemoryMB: 1024, Slots: 1},
		State: domain.InstanceRunning, Power: domain.InstancePowerRunning}
	return domain.Fresh([]domain.Instance{odd}, u.now), domain.Fresh(domain.Host{}, u.now)
}

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

// TestEngineTickerPrefersThePlannerReasonOverTheCoarseToken covers a regression
// introduced when the error path started taking its detail from the failure
// classification. On the commit path the tick returns a POPULATED result, so the
// planner may already have explained itself in Plan.Reason -- a specific,
// adapter-authored string like the exact reason an observation was rejected.
// Overwriting that with the coarse classification loses the more useful of the
// two. The classification remains the fallback when the planner said nothing.
func TestEngineTickerPrefersThePlannerReasonOverTheCoarseToken(t *testing.T) {
	health, engine := invalidPlanEngine(t)
	err := (engineTicker{engine: engine, health: health}).Tick(context.Background())
	if err == nil {
		t.Fatal("an unrecognized instance platform must surface as an error")
	}
	observation := health.Snapshot().Observations["scheduler"]
	if observation.Detail == "" {
		t.Fatal("no detail published for a failing tick")
	}
	if observation.Detail == app.ReasonPlanInvalid || observation.Detail == app.ReasonPlanCommitFailed {
		t.Fatalf("detail = %q: the planner's own reason was displaced by the coarse token",
			observation.Detail)
	}
}
