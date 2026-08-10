package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// TestStalledMetricsCarryTheWedgeToTelemetry pins the seam that publishes what
// the 2026-08-10 incident could only be read from SQLite: the instance, the
// step, the attempt count, and the elapsed time (ADR 0039).
func TestStalledMetricsCarryTheWedgeToTelemetry(t *testing.T) {
	now := time.Date(2026, 8, 10, 14, 16, 0, 0, time.UTC)
	wedge := operations.StalledOperation{OperationID: "event-drain-trf-macos-6x12-f458a747883b9a0d",
		Kind: "deregister", Code: "stop", Instance: "trf-macos-6x12-f458a747883b9a0d", Attempts: 67,
		Retrying: 82 * time.Minute, DrainState: "deregistering", Held: 82 * time.Minute}
	var asked time.Time
	ticker := engineTicker{now: func() time.Time { return now },
		stalledOperations: func(_ context.Context, at time.Time) ([]operations.StalledOperation, error) {
			asked = at
			return []operations.StalledOperation{wedge}, nil
		}}
	stalled, err := ticker.stalledMetrics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !asked.Equal(now) {
		t.Fatalf("the projection was asked about %s, not the ticker's own clock %s", asked, now)
	}
	if len(stalled) != 1 {
		t.Fatalf("stalled = %#v", stalled)
	}
	row := stalled[0]
	if row.Operation != wedge.OperationID || row.Kind != wedge.Kind || row.Code != wedge.Code ||
		row.Instance != wedge.Instance || row.Attempts != wedge.Attempts || row.Retrying != wedge.Retrying ||
		row.DrainState != wedge.DrainState || row.Held != wedge.Held {
		t.Fatalf("row = %#v, want every field carried through", row)
	}
}

// TestStalledMetricsFailClosedAndTolerateAnAbsentPort keeps rule 4 at this seam
// too: an unreadable projection is an error the caller degrades the observation
// on, and a daemon wired without the port publishes nothing rather than an empty
// set that would read as "everything is progressing".
func TestStalledMetricsFailClosedAndTolerateAnAbsentPort(t *testing.T) {
	stalled, err := engineTicker{}.stalledMetrics(context.Background())
	if err != nil || stalled != nil {
		t.Fatalf("absent port = %#v, %v", stalled, err)
	}
	failing := engineTicker{stalledOperations: func(context.Context, time.Time) ([]operations.StalledOperation, error) {
		return nil, errors.New("store unavailable")
	}}
	if _, err := failing.stalledMetrics(context.Background()); err == nil {
		t.Fatal("an unreadable projection was published as no stall at all")
	}
}
