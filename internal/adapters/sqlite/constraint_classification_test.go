package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

func constraintStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestApplyPlanReportsARefusedConstraintAsMalformed pins the operator-facing
// meaning of a refused write.
//
// During the 2026-08-02 wedge on host vitalii-mac-mini the scheduler kept
// authoring a plan whose durable write SQLite refused with
// `UNIQUE constraint failed: instances.id`. ApplyPlan wrapped that bare, so it
// matched neither operations.ErrConflict nor operations.ErrInvalid, and
// app.commitFailureReason fell through to plan_commit_failed — documented as "the
// durable store was unavailable or refused the write. Check the database".
//
// The database was healthy: PRAGMA integrity_check reported ok and every other
// read and write succeeded. A constraint violation is the durable layer refusing
// a plan as malformed, which is exactly what plan_commit_rejected means:
// "repeats every tick until the inputs change ... this is not a database fault".
func TestApplyPlanReportsARefusedConstraintAsMalformed(t *testing.T) {
	now := time.Unix(50_000, 0).UTC()
	operation := func(id string) operations.Operation {
		return operations.Operation{ID: id, IdempotencyKey: id, EffectKey: "clone:" + id, Kind: "clone",
			ResourceID: id, AvailableAt: now}
	}
	instance := func(id string) operations.InstanceIntent {
		return operations.InstanceIntent{ExpectedVersion: -1, Instance: operations.Instance{ID: id,
			Repo: "owner/repo", Platform: "linux", Profile: "small", Route: "linux-small",
			Resources: operations.Instance{}.Resources, State: operations.StatePlanned,
			Ownership: operations.Ownership{ControllerID: "c", ResourceID: "owner/repo/1/1/1", OperationID: id}}}
	}

	for _, testCase := range []struct {
		name string
		plan operations.Plan
	}{
		{
			name: "one plan enqueuing the same operation twice",
			plan: operations.Plan{ID: "plan-op", CreatedAt: now, Scheduler: operations.SchedulerState{Version: 1},
				Operations: []operations.Operation{operation("dup-op"), operation("dup-op")}},
		},
		{
			name: "one plan inserting the same instance twice",
			plan: operations.Plan{ID: "plan-vm", CreatedAt: now, Scheduler: operations.SchedulerState{Version: 1},
				Instances:  []operations.InstanceIntent{instance("trf-small-dup"), instance("trf-small-dup")},
				Operations: []operations.Operation{operation("vm-op")}},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store := constraintStore(t)
			applied, err := store.ApplyPlan(context.Background(), testCase.plan)
			if applied || err == nil {
				t.Fatalf("ApplyPlan = %v, %v; want the write refused", applied, err)
			}
			if !errors.Is(err, operations.ErrInvalid) {
				t.Fatalf("ApplyPlan error = %v; want it to match operations.ErrInvalid so the tick reports plan_commit_rejected", err)
			}
			if errors.Is(err, operations.ErrConflict) {
				t.Fatalf("ApplyPlan error = %v; a malformed plan is not an optimistic-concurrency loss", err)
			}
		})
	}
}

// TestApplyPlanKeepsANonConstraintFailureUnclassified is the fail-closed half.
// An unavailable store must keep meaning "look at the database": mislabelling it
// as a malformed plan would send an operator to the planner during a real
// outage, which is the inverse of the incident this classification repairs.
func TestApplyPlanKeepsANonConstraintFailureUnclassified(t *testing.T) {
	store := constraintStore(t)
	if err := store.db.Close(); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(60_000, 0).UTC()
	plan := operations.Plan{ID: "plan-closed", CreatedAt: now, Scheduler: operations.SchedulerState{Version: 1}}

	_, err := store.ApplyPlan(context.Background(), plan)

	if err == nil {
		t.Fatal("ApplyPlan on a closed database succeeded")
	}
	if errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("ApplyPlan error = %v; an unreachable store must not read as a malformed plan", err)
	}
}

// TestRefusedByConstraintIsNarrow pins the predicate itself against the driver's
// result codes. SQLite carries the specific constraint in the extended code's
// high bits, so only the low byte is a stable classification.
func TestRefusedByConstraintIsNarrow(t *testing.T) {
	for _, testCase := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil is not a refusal"},
		{name: "a plain error is not a refusal", err: errors.New("disk offline")},
		{name: "a sentinel is not a refusal", err: operations.ErrConflict},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := refusedByConstraint(testCase.err); got != testCase.want {
				t.Fatalf("refusedByConstraint(%v) = %v, want %v", testCase.err, got, testCase.want)
			}
		})
	}
	store := constraintStore(t)
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO plans(id,digest,scheduler_version,created_at) VALUES('p','d',1,1)`); err != nil {
		t.Fatal(err)
	}
	_, err := store.db.ExecContext(ctx, `INSERT INTO plans(id,digest,scheduler_version,created_at) VALUES('p','d',1,1)`)
	if !refusedByConstraint(err) {
		t.Fatalf("refusedByConstraint(%v) = false, want true for a real primary-key violation", err)
	}
}
