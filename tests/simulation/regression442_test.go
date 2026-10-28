package simulation_test

import (
	"context"
	"errors"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// ---------------------------------------------------------------------------
// Issue #442 -- a durable JobCompleted never drained its running instance
// ---------------------------------------------------------------------------
//
// Omarchy, 2026-10-28 06:09:18Z. Message 100003501 carried one event, a
// JobCompleted (canceled) for the request trf-linux-4x8-0ab3245aa6dfc6d6 was
// running. ApplyDemandBatch committed it and advanced the durable cursor. The
// projection that should have enqueued event-drain-<id> failed, and the ingest
// loop replaced the session. The replacement opened at the durable cursor, which
// was already past the message, so it was never delivered again. The runner had
// exited, the podman guest's PID 1 is `sleep infinity`, and the instance held
// 4 CPU / 8 GiB until the idle-runner deadline eight minutes later.

// lostProjection fails every projection, the way the 06:09:18 one failed.
type lostProjection struct{}

func (lostProjection) ProjectDemandEvent(context.Context, int64, operations.DemandEvent) error {
	return errors.New("database is locked")
}

func TestIssue442DurableCompletionDrainsOnTheTick(t *testing.T) {
	const runner = "trf-linux-4x8-0ab3245aa6dfc6d6"
	const request = int64(5871030784497826206)
	w := newWorld(t, containerNodeWorld(), simTrace{})
	defer w.close()
	ctx := context.Background()
	binding := w.cfg.Bindings[0]
	instance := operations.Instance{ID: runner, Repo: "a/repo", Platform: binding.Profile.Platform,
		Profile: binding.Profile.ID, Route: binding.Profile.Route, Resources: binding.Profile.Resources,
		Demand: domain.DemandKey{Repo: "a/repo", RunID: 40641346140, Attempt: 1, JobID: request},
		State:  operations.StateRunning, Ownership: operations.Ownership{ControllerID: simOwner, ResourceID: runner, OperationID: "spawn-" + runner},
		CreatedAt: w.now}
	if err := w.store.CreateInstance(ctx, instance); err != nil {
		t.Fatal(err)
	}
	w.vms[runner] = true

	// The broker hands the completion over once. Its projection fails, and the
	// source is never asked to deliver it again.
	coordinator := w.demand
	coordinator.Projector = lostProjection{}
	source := &projectionReplay{message: githubscaleset.Demand{MessageID: 100003501, Events: []githubscaleset.JobEvent{{
		Kind: githubscaleset.JobCompleted, RunnerRequestID: request, Owner: "a", Repository: "repo",
		WorkflowRunID: 40641346140, JobID: "effect-diagnostics", RunnerName: runner, Result: "canceled", QueueTime: w.now}}}}
	if changed, err := coordinator.IngestOnceResult(ctx, binding, source); err == nil || !changed || source.acks != 0 {
		t.Fatalf("ingest = %v, %v, acks %d; want a durable commit whose projection failed", changed, err, source.acks)
	}
	if record, err := w.store.DemandRecord(ctx, binding.StoreKey, request); err != nil || record.Status != operations.DemandJobCompleted {
		t.Fatalf("demand = %s, %v; want a durable JobCompleted", record.Status, err)
	}

	if _, err := w.engine.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := w.store.Instance(ctx, runner)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != operations.StateDraining || got.DrainPhase != operations.DrainPhaseEvent {
		t.Fatalf("instance = %s phase %v; want the event drain on the first tick", got.State, got.DrainPhase)
	}
	claimed, err := w.store.Claim(ctx, "issue-442", 10, w.now, simTick)
	if err != nil || len(claimed) != 1 || claimed[0].ID != "event-drain-"+runner {
		t.Fatalf("operations = %#v, %v; want only event-drain-%s", claimed, err, runner)
	}
}
