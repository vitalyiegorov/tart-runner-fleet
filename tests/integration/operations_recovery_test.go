package integration

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/sqlite"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

func TestCrashAfterClaimRecoversDurablePlanAndEffect(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fleet.db")
	now := time.Unix(1_000, 0).UTC()
	store, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	ownership := operations.Ownership{ControllerID: "controller", ResourceID: "vm", OperationID: "plan"}
	operation := operations.Operation{ID: "clone", IdempotencyKey: "clone", EffectKey: "clone", Kind: "clone", ResourceID: "vm", AvailableAt: now}
	plan := operations.Plan{
		ID: "plan", ExpectedSchedulerVersion: 0, CreatedAt: now,
		Scheduler:  operations.SchedulerState{Version: 1, Data: []byte(`{}`), Reservations: []byte(`[]`), DeficitRoundRobin: []byte(`{}`), ObservationCursor: "cursor"},
		Instances:  []operations.InstanceIntent{{ExpectedVersion: -1, Instance: operations.Instance{ID: "vm", State: operations.StatePlanned, Ownership: ownership}}},
		Operations: []operations.Operation{operation},
	}
	if applied, err := store.ApplyPlan(ctx, plan); err != nil || !applied {
		t.Fatalf("apply: %v %v", applied, err)
	}
	if claimed, err := store.Claim(ctx, "dead-worker", 1, now, time.Second); err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %#v %v", claimed, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if recovered, err := store.RecoverExpired(ctx, now.Add(2*time.Second)); err != nil || recovered != 1 {
		t.Fatalf("recover: %d %v", recovered, err)
	}
	claimed, err := store.Claim(ctx, "replacement", 1, now.Add(2*time.Second), time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("replacement claim: %#v %v", claimed, err)
	}
	if applied, err := store.Complete(ctx, "clone", "replacement", "clone", now.Add(3*time.Second)); err != nil || !applied {
		t.Fatalf("complete: %v %v", applied, err)
	}
	if applied, err := store.ApplyPlan(ctx, plan); err != nil || applied {
		t.Fatalf("duplicate plan: %v %v", applied, err)
	}
}
