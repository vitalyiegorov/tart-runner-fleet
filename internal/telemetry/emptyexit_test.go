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

func TestARefusedVersionTheGrammarCannotCarryIsRefused(t *testing.T) {
	health, err := NewHealth(emptyExitClock{}, HealthConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := health.SetRunnerImages([]RunnerImageMetric{{Platform: "linux", VM: "base", Floor: "2.336.0", RefusedVersion: "not a version"}}); err == nil {
		t.Fatal("a mangled version must not be published")
	}
}
