package noexecutor

import (
	"context"
	"errors"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/executor"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// TestBackendSatisfiesThePort keeps the stand-in and the real backends
// interchangeable: the day issue #139's container adapter lands, the wiring
// swaps one value for another and nothing above the port changes.
func TestBackendSatisfiesThePort(t *testing.T) {
	var backend executor.Backend = Backend{}
	if backend == nil {
		t.Fatal("nil backend")
	}
}

// TestEveryMutationIsRefused is the safety property. A node in Phase 1 Part A
// must never appear to have provisioned something, so each verb that acts fails
// loudly and the durable operation retries and eventually parks for an operator.
func TestEveryMutationIsRefused(t *testing.T) {
	ctx := context.Background()
	backend := Backend{}
	ownership := operations.Ownership{}
	refusals := map[string]error{
		"create": backend.Create(ctx, executor.InstanceSpec{Name: "trf-small-1", Image: "base"}),
		"start":  backend.Start(ctx, "trf-small-1", ownership),
		"stop":   backend.Stop(ctx, "trf-small-1", ownership),
		"delete": backend.Delete(ctx, "trf-small-1", ownership),
		"reap":   backend.Reap(ctx, "trf-small-1", ownership),
	}
	for verb, err := range refusals {
		if !errors.Is(err, ErrNoBackend) {
			t.Errorf("%s returned %v, want ErrNoBackend", verb, err)
		}
	}
}

// TestObservationsAreTruthfulRatherThanRefused covers the other half. A node
// that cannot create an instance provably holds none, so the empty answer is a
// measurement — refusing it would fail-close the one mode this node may run in.
func TestObservationsAreTruthfulRatherThanRefused(t *testing.T) {
	ctx := context.Background()
	instances, err := Backend{}.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if instances == nil || len(instances) != 0 {
		t.Fatalf("instances = %#v, want a non-nil empty slice", instances)
	}
	running, err := Backend{}.Running(ctx, "trf-small-1")
	if err != nil || running {
		t.Fatalf("running = %v, %v", running, err)
	}
}
