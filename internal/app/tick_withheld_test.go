package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

type emptyExitStoreFake struct {
	*tickStore
	exits map[domain.DemandKey]domain.EmptyExits
	err   error
}

func (s emptyExitStoreFake) EmptyExits(context.Context, time.Time) (map[domain.DemandKey]domain.EmptyExits, error) {
	return s.exits, s.err
}

func withheldEngine(now time.Time, exits map[domain.DemandKey]domain.EmptyExits, err error) Engine {
	engine := liveIncarnationEngine(now, nil)
	store := emptyExitStoreFake{tickStore: engine.Store.(*tickStore), exits: exits, err: err}
	engine.Store, engine.Demand.Store = store, store
	return engine
}

func TestAnEmptyExitHoldWithholdsTheDemandAndKeepsItNamed(t *testing.T) {
	now := time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)
	exits := map[domain.DemandKey]domain.EmptyExits{liveIncarnationKey: {Count: 3, Last: now.Add(-time.Minute)}}
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

	exits[liveIncarnationKey] = domain.EmptyExits{Count: 3, Last: now.Add(-5 * time.Minute)}
	result, err = withheldEngine(now, exits, nil).Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Plan.Operations) != 1 || len(result.EmptyExitBackoffs) != 1 {
		t.Fatalf("after the hold it is retried and still named: %+v %+v", result.Plan.Operations, result.EmptyExitBackoffs)
	}
}

func TestAnUnreadableEmptyExitCountFailsTheTick(t *testing.T) {
	now := time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)
	if _, err := withheldEngine(now, nil, errors.New("boom")).Tick(context.Background()); err == nil {
		t.Fatal("a count that could not be read must not read as no exits")
	}
}

func TestARefusedImageWithholdsItsWorkFromTheTick(t *testing.T) {
	now := time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)
	engine := liveIncarnationEngine(now, nil)
	engine.Refusals = &RunnerRefusals{Now: func() time.Time { return now }}
	engine.Refusals.Record(domain.PlatformLinux, "2.336.0", "trf-a")
	result, err := engine.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Plan.Operations) != 0 || result.RunnerRefusals[domain.PlatformLinux].Version != "2.336.0" {
		t.Fatalf("got %+v %+v", result.Plan.Operations, result.RunnerRefusals)
	}
	engine.Now = func() time.Time { return now.Add(domain.RunnerRefusalProbeInterval) }
	result, err = engine.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Plan.Operations) != 1 {
		t.Fatalf("the probe must launch: %+v", result.Plan.Operations)
	}
}

func TestARefusalOnTheWallClockIsDated(t *testing.T) {
	refusals := &RunnerRefusals{}
	refusals.Record(domain.PlatformLinux, "2.336.0", "trf-a")
	if refusals.Observe(nil)[domain.PlatformLinux].At.IsZero() {
		t.Fatal("a nil clock is the wall clock")
	}
}
