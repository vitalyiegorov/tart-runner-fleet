package reconcile

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

// A reaped job fails on GitHub with a lost-communication error, which looks
// exactly like a flake. The operation record is what tells the two apart, so a
// budget reclaim — alone among drains — must carry the job it cut into the
// durable payload rather than leaving the demand fields zero (issue #223).
func TestOccupancyBudgetDrainRecordsTheJobItCuts(t *testing.T) {
	cut := domain.DemandKey{Repo: "rnw-community/rnw-community", RunID: 31_325_708_527, Attempt: 1, JobID: 93_275_690_093}
	instance := operations.Instance{ID: "trf-xl-05bbe1c83f21fcd6", State: operations.StateRunning, Version: 7,
		Demand: cut, Ownership: operations.Ownership{ControllerID: "controller", ResourceID: cut.String(), OperationID: "spawn"}}
	store := &fakeStore{instances: map[string]operations.Instance{instance.ID: instance}}
	controller := Controller{Store: store, ControllerID: "controller", Mode: Authority}
	reap := scheduler.Operation{ID: "reap", Kind: scheduler.OperationDrain, Instance: instance.ID,
		Profile: "xl", Route: "linux-xl", Demand: cut, Recovery: true, OccupancyExceeded: true}

	if applied, err := controller.Commit(context.Background(), readyPlan(reap), "", controllerNow); err != nil || !applied {
		t.Fatalf("occupancy reclaim commit = %v, %v", applied, err)
	}

	var payload struct {
		Repo    string `json:"repo"`
		Profile string `json:"profile"`
		RunID   int64  `json:"run_id"`
		JobID   int64  `json:"job_id"`
		Attempt int    `json:"attempt"`
	}
	if err := json.Unmarshal(store.applied[0].Operations[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Repo != cut.Repo || payload.RunID != cut.RunID || payload.JobID != cut.JobID ||
		payload.Attempt != cut.Attempt || payload.Profile != "xl" {
		t.Fatalf("the operation record must name the job that was cut; got %#v", payload)
	}
}
