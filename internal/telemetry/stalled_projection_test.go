package telemetry

import (
	"testing"
	"time"
)

// A stalled operation is what ADR 0039 asks an operator to act on, so the
// versioned projection of one has to carry every field the decision needs.
// Nothing published it before: `stalledRows` was reachable only through a
// snapshot that happened to hold none, which is the shape of a projection that
// looks tested and asserts nothing.
func TestStalledRowsCarryTheWholeFindingIntoTheVersionedSurface(t *testing.T) {
	health := yieldHealth(t)
	if rows := stalledRows(health.Snapshot()); rows != nil {
		t.Fatalf("a fleet with nothing stalled published %d rows", len(rows))
	}

	if err := health.SetStalled([]Stalled{{Operation: "op-7f3a", Kind: "provision", Code: "guest_unreachable",
		Instance: "trf-linux-4x8-a1b2", Attempts: 9, Retrying: 12 * time.Minute,
		DrainState: "draining", Held: 90 * time.Second}}); err != nil {
		t.Fatalf("SetStalled: %v", err)
	}
	rows := stalledRows(health.Snapshot())
	if len(rows) != 1 {
		t.Fatalf("published %d rows for one stalled operation", len(rows))
	}
	row := rows[0]
	if row.Operation != "op-7f3a" || row.Kind != "provision" || row.Code != "guest_unreachable" {
		t.Fatalf("the row does not identify the operation: %+v", row)
	}
	if row.Instance != "trf-linux-4x8-a1b2" || row.Attempts != 9 {
		t.Fatalf("the row does not identify the instance or its attempts: %+v", row)
	}
	// The two durations are what separate "retrying, and has been for a while"
	// from "held in a cleanup state", and ADR 0039 acts on them differently.
	if row.RetryingSeconds != 720 || row.HeldSeconds != 90 {
		t.Fatalf("durations projected as retrying=%v held=%v", row.RetryingSeconds, row.HeldSeconds)
	}
	if row.DrainState != "draining" {
		t.Fatalf("the drain state was lost: %+v", row)
	}
}
