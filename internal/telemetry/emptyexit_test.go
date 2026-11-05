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
