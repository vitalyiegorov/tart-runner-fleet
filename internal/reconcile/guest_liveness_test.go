package reconcile

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

// Issue #236's eight production deaths left no record naming any of them. The
// durable operation is the fleet's own account of the reclaim, so it must select
// the phase that stops the guest first and carry the job that died with it —
// otherwise a runner lost to a panicked kernel is still indistinguishable from
// a flake after the fix.
func TestAGuestUnresponsiveDrainSelectsItsPhaseAndNamesTheLostJob(t *testing.T) {
	lost := domain.DemandKey{Repo: "rnw-community/rnw-community", RunID: 31_939_037_119, Attempt: 1, JobID: 93_540_000_001}
	instance := operations.Instance{ID: "trf-xl-0aacdbcc6653bd8a", State: operations.StateRunning, Version: 3,
		Demand: lost, Ownership: operations.Ownership{ControllerID: "controller", ResourceID: lost.String(), OperationID: "spawn"}}
	store := &fakeStore{instances: map[string]operations.Instance{instance.ID: instance}}
	controller := Controller{Store: store, ControllerID: "controller", Mode: Authority}
	reclaim := scheduler.Operation{ID: "reclaim", Kind: scheduler.OperationDrain, Instance: instance.ID,
		Profile: "xl", Route: "linux-xl", Demand: lost, Recovery: true, GuestUnresponsive: true}

	if applied, err := controller.Commit(context.Background(), readyPlan(reclaim), "", controllerNow); err != nil || !applied {
		t.Fatalf("guest-liveness reclaim commit = %v, %v", applied, err)
	}
	if got := store.applied[0].Instances[0].Instance.DrainPhase; got != operations.DrainPhaseGuestUnresponsive {
		t.Fatalf("drain phase = %d, want the guest-unresponsive phase", got)
	}
	var payload struct {
		Repo    string `json:"repo"`
		RunID   int64  `json:"run_id"`
		JobID   int64  `json:"job_id"`
		Attempt int    `json:"attempt"`
	}
	if err := json.Unmarshal(store.applied[0].Operations[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Repo != lost.Repo || payload.RunID != lost.RunID || payload.JobID != lost.JobID || payload.Attempt != lost.Attempt {
		t.Fatalf("the operation record must name the job that died with the guest; got %#v", payload)
	}
}

// A dead guest and a long hold are different facts about the same instance, and
// an operator who reads the drain phase must be told which one acted.
func TestADeadGuestOutranksTheBudgetInTheDurablePhase(t *testing.T) {
	instance := operations.Instance{ID: "trf-xl-1", State: operations.StateRunning, Version: 1,
		Ownership: operations.Ownership{ControllerID: "controller", ResourceID: "r", OperationID: "spawn"}}
	store := &fakeStore{instances: map[string]operations.Instance{instance.ID: instance}}
	controller := Controller{Store: store, ControllerID: "controller", Mode: Authority}
	both := scheduler.Operation{ID: "both", Kind: scheduler.OperationDrain, Instance: instance.ID,
		Profile: "xl", Route: "linux-xl", Recovery: true, GuestUnresponsive: true, OccupancyExceeded: true}

	if applied, err := controller.Commit(context.Background(), readyPlan(both), "", controllerNow); err != nil || !applied {
		t.Fatalf("commit = %v, %v", applied, err)
	}
	if got := store.applied[0].Instances[0].Instance.DrainPhase; got != operations.DrainPhaseGuestUnresponsive {
		t.Fatalf("drain phase = %d, want the guest-unresponsive phase to win", got)
	}
}
