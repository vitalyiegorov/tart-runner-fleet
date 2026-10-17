package simulation_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// A retained message can name a runner whose local instance is unavailable.
// Generate that state beside valid lifecycle events, rather than assuming every
// delivered event can project. This is a fleet defect, not total demand loss:
// ApplyDemandBatch has already durably recorded ALL events before projection.
func TestIssue134ProjectionFailureDoesNotBlockNeighbors(t *testing.T) {
	for badIndex := range 3 {
		t.Run(fmt.Sprintf("bad-at-%d", badIndex), func(t *testing.T) {
			w := newWorld(t, containerNodeWorld(), simTrace{})
			defer w.close()
			ctx := context.Background()
			binding := w.cfg.Bindings[0]
			event := func(request int64, kind githubscaleset.JobEventKind) githubscaleset.JobEvent {
				return githubscaleset.JobEvent{Kind: kind, RunnerRequestID: request, Owner: "a", Repository: "repo", WorkflowRunID: request, QueueTime: w.now}
			}
			good := []githubscaleset.JobEvent{event(71, githubscaleset.JobStarted), event(72, githubscaleset.JobCompleted)}
			bad := event(73, githubscaleset.JobStarted)
			bad.RunnerName = "trf-missing-projection"
			makeInstance := func(request int64, id string) {
				t.Helper()
				instance := operations.Instance{ID: id, Repo: "a/repo", Platform: binding.Profile.Platform,
					Profile: binding.Profile.ID, Route: binding.Profile.Route, Resources: binding.Profile.Resources,
					Demand: domain.DemandKey{Repo: "a/repo", RunID: request, Attempt: 1, JobID: request},
					State:  operations.StateRegistering, Ownership: operations.Ownership{ControllerID: simOwner, ResourceID: id, OperationID: "spawn-" + id}, CreatedAt: w.now}
				if err := w.store.CreateInstance(ctx, instance); err != nil {
					t.Fatal(err)
				}
			}
			makeInstance(71, "trf-projection-start")
			makeInstance(72, "trf-projection-complete")
			events := append([]githubscaleset.JobEvent(nil), good[:badIndex]...)
			events = append(events, bad)
			events = append(events, good[badIndex:]...)
			// A queued neighbor is durable even on the old short-circuit path;
			// assert that separately from lifecycle progress to avoid claiming loss.
			events = append(events, event(74, githubscaleset.JobAvailable))
			source := &projectionReplay{message: githubscaleset.Demand{MessageID: 1, Events: events}}
			for attempt := range 2 {
				changed, err := w.demand.IngestOnceResult(ctx, binding, source)
				if !errors.Is(err, operations.ErrUncertain) || source.acks != 0 || changed != (attempt == 0) {
					t.Fatalf("attempt %d: changed=%v error=%v acks=%d", attempt, changed, err, source.acks)
				}
				for _, e := range events {
					record, err := w.store.DemandRecord(ctx, binding.StoreKey, e.RunnerRequestID)
					if err != nil || record.Status != operations.DemandEventKind(e.Kind) {
						t.Fatalf("event not durable: status=%s error=%v", record.Status, err)
					}
				}
				for id, want := range map[string]operations.State{"trf-projection-start": operations.StateRunning, "trf-projection-complete": operations.StateDraining} {
					got, err := w.store.Instance(ctx, id)
					if err != nil || got.State != want {
						t.Fatalf("attempt %d: %s state=%s want=%s error=%v", attempt, id, got.State, want, err)
					}
				}
			}
			// Repair only the missing observation, then redeliver the identical message.
			// The committed inbox is a duplicate; lifecycle retries must still run.
			makeInstance(73, bad.RunnerName)
			changed, err := w.demand.IngestOnceResult(ctx, binding, source)
			if err != nil || changed || source.acks != 1 {
				t.Fatalf("recovered replay: changed=%v error=%v acks=%d", changed, err, source.acks)
			}
			pending, err := w.store.Claim(ctx, "projection-proof", 10, w.now, simTick)
			if err != nil || len(pending) != 1 || pending[0].ResourceID != "trf-projection-complete" {
				t.Fatalf("idempotent drain count=%d error=%v", len(pending), err)
			}
		})
	}
}

type projectionReplay struct {
	message githubscaleset.Demand
	acks    int
}

func (s *projectionReplay) Handle(ctx context.Context, handle func(context.Context, githubscaleset.Demand) error) error {
	if err := handle(ctx, s.message); err != nil {
		return err
	}
	s.acks++
	return nil
}
