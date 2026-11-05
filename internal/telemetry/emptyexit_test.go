package telemetry

import (
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
)

func TestAnEmptyExitBackoffIsNamedInTheStatusAndLeavesHealthAlone(t *testing.T) {
	now := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	metric := EmptyExitBackoffMetric{Demand: "o/r/1/1/7", Scope: "s", Profile: "maestro", Exits: 3, LastExitAt: now, RetryAt: now.Add(5 * time.Minute)}
	result := emptyExitResult([]EmptyExitBackoffMetric{metric})
	if result.OK || !strings.HasPrefix(result.Reasons[0], "demand_empty_exit_backoff: o/r/1/1/7") {
		t.Fatalf("got %+v", result)
	}
	if !emptyExitResult(nil).OK {
		t.Fatal("no backoff is a pass")
	}
	rows := emptyExitRows(Snapshot{EmptyExitBackoffs: []EmptyExitBackoffMetric{metric}})
	if want := (adminapi.EmptyExitBackoff{Demand: "o/r/1/1/7", Scope: "s", Profile: "maestro", Exits: 3, LastExitAt: now, RetryAt: now.Add(5 * time.Minute)}); len(rows) != 1 || rows[0] != want {
		t.Fatalf("got %+v", rows)
	}
	if emptyExitRows(Snapshot{}) != nil {
		t.Fatal("nothing to report stays nil")
	}
}

func TestASignalIsPublishedWholeAndReachesTheStatus(t *testing.T) {
	health, err := NewHealth(emptyExitClock{}, HealthConfig{})
	if err != nil {
		t.Fatal(err)
	}
	health.SetImageSignals([]ImageSignalMetric{{Name: "image_generation_not_receiving_jobs", Image: "base@1", Detail: "d"}})
	rows := imageSignalRows(health.Snapshot())
	if len(rows) != 1 || rows[0].Name != "image_generation_not_receiving_jobs" || rows[0].Image != "base@1" || rows[0].Detail != "d" {
		t.Fatalf("got %+v", rows)
	}
	health.SetImageSignals(nil)
	if rows := imageSignalRows(health.Snapshot()); rows != nil {
		t.Fatalf("a finding that stopped being true goes: %+v", rows)
	}
}

func TestAPublishedBackoffReachesTheSnapshotAndIsReplacedWhole(t *testing.T) {
	health, err := NewHealth(emptyExitClock{}, HealthConfig{})
	if err != nil {
		t.Fatal(err)
	}
	health.SetEmptyExitBackoffs([]EmptyExitBackoffMetric{{Demand: "o/r/1/1/7", Exits: 3}})
	if rows := health.Snapshot().EmptyExitBackoffs; len(rows) != 1 {
		t.Fatalf("got %+v", rows)
	}
	health.SetEmptyExitBackoffs(nil)
	if rows := health.Snapshot().EmptyExitBackoffs; len(rows) != 0 {
		t.Fatalf("got %+v", rows)
	}
}

type emptyExitClock struct{}

func (emptyExitClock) Now() time.Time { return time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC) }
