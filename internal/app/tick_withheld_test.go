package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

type emptyExitStoreFake struct {
	*tickStore
	exits map[domain.EmptyExitKey]domain.EmptyExits
	err   error
}

func (s emptyExitStoreFake) EmptyExits(context.Context, time.Time) (map[domain.EmptyExitKey]domain.EmptyExits, error) {
	return s.exits, s.err
}

func withheldEngine(now time.Time, exits map[domain.EmptyExitKey]domain.EmptyExits, err error) Engine {
	engine := liveIncarnationEngine(now, nil)
	store := emptyExitStoreFake{tickStore: engine.Store.(*tickStore), exits: exits, err: err}
	engine.Store, engine.Demand.Store = store, store
	return engine
}

func TestAnEmptyExitHoldWithholdsTheDemandAndKeepsItNamed(t *testing.T) {
	now := time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)
	key := domain.EmptyExitKey{Demand: liveIncarnationKey}
	exits := map[domain.EmptyExitKey]domain.EmptyExits{key: {Count: 3, Last: now.Add(-time.Minute)}}
	result, err := withheldEngine(now, exits, nil).Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Plan.Operations) != 0 || len(result.Demands) != 0 {
		t.Fatalf("a held demand was planned: %+v", result.Plan.Operations)
	}
	if len(result.EmptyExitBackoffs) != 1 || result.EmptyExitBackoffs[0].Exits != 3 {
		t.Fatalf("the hold must be named: %+v", result.EmptyExitBackoffs)
	}

	exits[key] = domain.EmptyExits{Count: 3, Last: now.Add(-5 * time.Minute)}
	result, err = withheldEngine(now, exits, nil).Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Plan.Operations) != 1 || len(result.EmptyExitBackoffs) != 1 {
		t.Fatalf("after the hold it is retried and still named: %+v %+v", result.Plan.Operations, result.EmptyExitBackoffs)
	}
}

// The image generation scopes the count: exits on the image that was replaced never
// hold its replacement back, and it starts from zero rather than from theirs.
func TestAReplacementImageOwesNothingForItsPredecessorsExits(t *testing.T) {
	now := time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)
	old := domain.EmptyExitKey{Demand: liveIncarnationKey, Image: "base@2.336.0"}
	exits := map[domain.EmptyExitKey]domain.EmptyExits{old: {Count: 6, Last: now.Add(-time.Minute)}}
	engine := withheldEngine(now, exits, nil)
	engine.Images = map[domain.Platform]string{domain.PlatformLinux: "base@2.336.0"}
	result, err := engine.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(spawnKeys(result.Plan)) != 0 || len(result.EmptyExitBackoffs) != 1 {
		t.Fatalf("the image that earned the hold keeps it: %+v", result.Plan.Operations)
	}
	engine.Images = map[domain.Platform]string{domain.PlatformLinux: "base@2.338.2"}
	result, err = engine.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(spawnKeys(result.Plan)) != 1 || len(result.EmptyExitBackoffs) != 0 {
		t.Fatalf("the replacement was charged: %+v %+v", result.Plan.Operations, result.EmptyExitBackoffs)
	}
}

func TestAnUnreadableEmptyExitCountFailsTheTick(t *testing.T) {
	now := time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)
	if _, err := withheldEngine(now, nil, errors.New("boom")).Tick(context.Background()); err == nil {
		t.Fatal("a count that could not be read must not read as no exits")
	}
}

func TestARefusalOnTheWallClockIsDated(t *testing.T) {
	refusals := &RunnerRefusals{}
	refusals.Record(domain.PlatformLinux, "2.336.0", "trf-a")
	if refusals.Observe(nil)[domain.PlatformLinux].At.IsZero() {
		t.Fatal("a nil clock is the wall clock")
	}
}

func spawnKeys(plan scheduler.Plan) []domain.DemandKey {
	var keys []domain.DemandKey
	for _, operation := range plan.Operations {
		if operation.Kind == scheduler.OperationSpawn {
			keys = append(keys, operation.Demand)
		}
	}
	return keys
}
