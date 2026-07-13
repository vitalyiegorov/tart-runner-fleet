package app

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

type fakeDemandStore struct {
	batches []operations.DemandEvent
	records []operations.DemandRecord
	cursor  int64
	applied bool
	err     error
}

func (f *fakeDemandStore) ApplyDemandBatch(_ context.Context, scaleSetID, messageID int64, events []operations.DemandEvent) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	f.cursor = messageID
	f.batches = append([]operations.DemandEvent(nil), events...)
	return f.applied, nil
}
func (f *fakeDemandStore) ActiveDemands(context.Context, int64) ([]operations.DemandRecord, error) {
	return append([]operations.DemandRecord(nil), f.records...), f.err
}

type fakeMessages struct {
	demand         githubscaleset.Demand
	err            error
	afterCommitErr error
	committed      bool
}

func (f *fakeMessages) Handle(ctx context.Context, commit func(context.Context, githubscaleset.Demand) error) error {
	if f.err != nil {
		return f.err
	}
	if err := commit(ctx, f.demand); err != nil {
		return err
	}
	f.committed = true
	return f.afterCommitErr
}

func TestIngestCommitsSanitizedConcreteEvents(t *testing.T) {
	queue := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	source := &fakeMessages{demand: githubscaleset.Demand{MessageID: 7, Events: []githubscaleset.JobEvent{{
		Kind: githubscaleset.JobAvailable, RunnerRequestID: 11, Owner: "owner", Repository: "repo", WorkflowRunID: 22,
		JobID: "uuid", EventName: "pull_request", Labels: []string{"self-hosted", "linux-small"}, QueueTime: queue,
	}}}}
	store := &fakeDemandStore{applied: true}
	binding := Binding{ScaleSetID: 3, Profile: domain.Profile{ID: "small", Route: "tiered", Platform: domain.PlatformLinux}}
	if err := (DemandCoordinator{Store: store}).IngestOnce(context.Background(), binding, source); err != nil {
		t.Fatal(err)
	}
	if !source.committed || store.cursor != 7 || len(store.batches) != 1 {
		t.Fatalf("commit = %v cursor=%d events=%#v", source.committed, store.cursor, store.batches)
	}
	got := store.batches[0]
	if got.Kind != operations.DemandJobAvailable || got.RunnerRequestID != 11 || !reflect.DeepEqual(got.Labels, []string{"self-hosted", "linux-small"}) {
		t.Fatalf("event = %#v", got)
	}
	source.demand.Events[0].Labels[0] = "mutated"
	if store.batches[0].Labels[0] != "self-hosted" {
		t.Fatal("stored conversion aliases source")
	}
}

func TestIngestResultPreservesDurableChangeAcrossAcknowledgementFailure(t *testing.T) {
	want := errors.New("ack failed")
	store := &fakeDemandStore{applied: true}
	source := &fakeMessages{afterCommitErr: want, demand: githubscaleset.Demand{MessageID: 7}}
	binding := Binding{ScaleSetID: 3, Profile: domain.Profile{ID: "small", Route: "tiered", Platform: domain.PlatformLinux}}
	changed, err := (DemandCoordinator{Store: store}).IngestOnceResult(context.Background(), binding, source)
	if !changed || !errors.Is(err, want) {
		t.Fatalf("IngestOnceResult() = %v, %v; want durable change plus acknowledgement error", changed, err)
	}
	store.applied = false
	source.afterCommitErr = nil
	changed, err = (DemandCoordinator{Store: store}).IngestOnceResult(context.Background(), binding, source)
	if changed || err != nil {
		t.Fatalf("duplicate IngestOnceResult() = %v, %v", changed, err)
	}
}

func TestQueuedDemandsMapsOnlyAvailableRecords(t *testing.T) {
	queue := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	store := &fakeDemandStore{records: []operations.DemandRecord{
		{Status: operations.DemandJobAssigned, RunnerRequestID: 1, Owner: "owner", Repository: "repo", WorkflowRunID: 9},
		{Status: operations.DemandJobAvailable, RunnerRequestID: 2, Owner: "owner", Repository: "repo", WorkflowRunID: 9, EventName: "schedule", QueueTime: queue},
		{Status: operations.DemandJobAvailable, RunnerRequestID: 3, Owner: "owner", Repository: "repo", WorkflowRunID: 9, EventName: "push", QueueTime: queue.Add(time.Second)},
		{Status: operations.DemandJobAvailable, RunnerRequestID: 4, Owner: "owner", Repository: "repo", WorkflowRunID: 9, EventName: "pull_request_target", QueueTime: queue.Add(2 * time.Second)},
	}}
	binding := Binding{ScaleSetID: 3, Profile: domain.Profile{ID: "small", Route: "tiered", Platform: domain.PlatformLinux}}
	got, err := (DemandCoordinator{Store: store}).QueuedDemands(context.Background(), binding)
	if err != nil || len(got) != 3 {
		t.Fatalf("QueuedDemands() = %#v, %v", got, err)
	}
	if got[0].Key.JobID != 2 || got[0].Key.Attempt != 1 || got[0].Event != domain.EventSchedule || got[1].Event != domain.EventPush || got[2].Event != domain.EventPullRequest {
		t.Fatalf("mapped demands = %#v", got)
	}
	if got[0].Profile != "small" || got[0].Route != "tiered" || got[0].Platform != domain.PlatformLinux || got[0].CreatedAt != queue {
		t.Fatalf("profile mapping = %#v", got[0])
	}
}

func TestDemandCoordinatorFailsClosed(t *testing.T) {
	want := errors.New("down")
	validBinding := Binding{ScaleSetID: 1, Profile: domain.Profile{ID: "small", Route: "tiered", Platform: domain.PlatformLinux}}
	tests := []struct {
		name        string
		coordinator DemandCoordinator
		binding     Binding
		source      MessageSource
	}{
		{name: "nil store", binding: validBinding, source: &fakeMessages{}},
		{name: "bad binding", coordinator: DemandCoordinator{Store: &fakeDemandStore{}}, source: &fakeMessages{}},
		{name: "nil source", coordinator: DemandCoordinator{Store: &fakeDemandStore{}}, binding: validBinding},
		{name: "source error", coordinator: DemandCoordinator{Store: &fakeDemandStore{}}, binding: validBinding, source: &fakeMessages{err: want}},
		{name: "store error", coordinator: DemandCoordinator{Store: &fakeDemandStore{err: want}}, binding: validBinding, source: &fakeMessages{demand: githubscaleset.Demand{MessageID: 1}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.coordinator.IngestOnce(context.Background(), tt.binding, tt.source); err == nil {
				t.Fatal("IngestOnce() unexpectedly succeeded")
			}
		})
	}
	if _, err := (DemandCoordinator{}).QueuedDemands(context.Background(), validBinding); err == nil {
		t.Fatal("nil-store QueuedDemands succeeded")
	}
	if _, err := (DemandCoordinator{Store: &fakeDemandStore{err: want}}).QueuedDemands(context.Background(), validBinding); !errors.Is(err, want) {
		t.Fatalf("QueuedDemands error = %v", err)
	}
	badRecords := &fakeDemandStore{records: []operations.DemandRecord{{Status: operations.DemandJobAvailable, RunnerRequestID: 1}}}
	if _, err := (DemandCoordinator{Store: badRecords}).QueuedDemands(context.Background(), validBinding); err == nil {
		t.Fatal("incomplete durable demand accepted")
	}
}
