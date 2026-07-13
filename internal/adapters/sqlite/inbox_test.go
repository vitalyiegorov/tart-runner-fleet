package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

func demandEvent(kind operations.DemandEventKind, requestID int64) operations.DemandEvent {
	return operations.DemandEvent{
		Kind: kind, RunnerRequestID: requestID, Owner: "owner", Repository: "repo", WorkflowRunID: 77,
		JobID: "job-uuid", EventName: "push", Labels: []string{"self-hosted", "linux"}, QueueTime: time.Unix(100, 0).UTC(),
	}
}

func TestDemandInboxAtomicDuplicateConflictRestartAndSanitization(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fleet.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	events := []operations.DemandEvent{demandEvent(operations.DemandJobAvailable, 10), demandEvent(operations.DemandJobAssigned, 11)}
	if applied, err := store.ApplyDemandBatch(ctx, 9, 42, events); err != nil || !applied {
		t.Fatalf("first batch: %v %v", applied, err)
	}
	if applied, err := store.ApplyDemandBatch(ctx, 9, 42, events); err != nil || applied {
		t.Fatalf("duplicate batch: %v %v", applied, err)
	}
	conflict := append([]operations.DemandEvent(nil), events...)
	conflict[0].Repository = "different"
	if applied, err := store.ApplyDemandBatch(ctx, 9, 42, conflict); applied || !errors.Is(err, operations.ErrConflict) {
		t.Fatalf("conflicting redelivery: %v %v", applied, err)
	}
	var encoded string
	if err := store.db.QueryRow(`SELECT events FROM scale_set_inbox WHERE scale_set_id=9 AND message_id=42`).Scan(&encoded); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(encoded), "acquirejob") || strings.Contains(strings.ToLower(encoded), "jit") || strings.Contains(encoded, "secret") {
		t.Fatalf("secret-bearing protocol data persisted: %s", encoded)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	active, err := store.ActiveDemands(ctx, 9)
	if err != nil || len(active) != 2 || active[0].RunnerRequestID != 10 || active[1].Status != operations.DemandJobAssigned {
		t.Fatalf("restart snapshot: %#v %v", active, err)
	}
	if cursor, err := store.DemandCursor(ctx, 9); err != nil || cursor != 42 {
		t.Fatalf("restart cursor: %d %v", cursor, err)
	}
}

func TestDemandInboxMixedLifecycleOutOfOrderAndCursorMonotonic(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	completed := demandEvent(operations.DemandJobCompleted, 20)
	completed.RunnerID, completed.RunnerName, completed.Result = 5, "runner", "success"
	mixed := []operations.DemandEvent{
		demandEvent(operations.DemandJobAvailable, 21),
		demandEvent(operations.DemandJobAssigned, 21),
		demandEvent(operations.DemandJobStarted, 21),
		completed,
	}
	if _, err := store.ApplyDemandBatch(ctx, 1, 20, mixed); err != nil {
		t.Fatal(err)
	}
	// A late lower-lifecycle event enriches metadata without resurrecting a
	// completed demand.
	late := demandEvent(operations.DemandJobAvailable, 20)
	late.Repository = "enriched"
	if _, err := store.ApplyDemandBatch(ctx, 1, 19, []operations.DemandEvent{late}); err != nil {
		t.Fatal(err)
	}
	active, err := store.ActiveDemands(ctx, 1)
	if err != nil || len(active) != 1 || active[0].RunnerRequestID != 21 || active[0].Status != operations.DemandJobStarted {
		t.Fatalf("active lifecycle projection: %#v %v", active, err)
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.demandRecord(ctx, tx, 1, 20)
	_ = tx.Rollback()
	if err != nil || record.Status != operations.DemandJobCompleted || record.Repository != "enriched" || record.Result != "success" {
		t.Fatalf("out-of-order projection: %#v %v", record, err)
	}
	if cursor, err := store.DemandCursor(ctx, 1); err != nil || cursor != 20 {
		t.Fatalf("cursor regressed: %d %v", cursor, err)
	}
	finish := demandEvent(operations.DemandJobCompleted, 21)
	finish.Result = "cancelled"
	if _, err := store.ApplyDemandBatch(ctx, 1, 21, []operations.DemandEvent{finish}); err != nil {
		t.Fatal(err)
	}
	if active, err := store.ActiveDemands(ctx, 1); err != nil || len(active) != 0 {
		t.Fatalf("completed demand remains active: %#v %v", active, err)
	}
}

func TestDemandInboxValidationAndEmptySnapshot(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	for _, test := range []struct {
		scaleSet int64
		message  int64
		events   []operations.DemandEvent
	}{
		{0, 1, nil}, {1, 0, nil}, {1, 1, []operations.DemandEvent{{Kind: "bad", RunnerRequestID: 1}}},
	} {
		if _, err := store.ApplyDemandBatch(ctx, test.scaleSet, test.message, test.events); !errors.Is(err, operations.ErrInvalid) {
			t.Fatalf("invalid batch accepted: %v", err)
		}
	}
	if active, err := store.ActiveDemands(ctx, 1); err != nil || len(active) != 0 {
		t.Fatalf("empty active snapshot: %#v %v", active, err)
	}
	if cursor, err := store.DemandCursor(ctx, 1); err != nil || cursor != 0 {
		t.Fatalf("empty cursor: %d %v", cursor, err)
	}
	if _, err := store.ActiveDemands(ctx, 0); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("invalid active request: %v", err)
	}
	if _, err := store.DemandCursor(ctx, 0); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("invalid cursor request: %v", err)
	}
	invalidTime := demandEvent(operations.DemandJobAvailable, 1)
	invalidTime.QueueTime = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := store.ApplyDemandBatch(ctx, 1, 2, []operations.DemandEvent{invalidTime}); err == nil {
		t.Fatal("unencodable demand event accepted")
	}
}

func TestDemandInboxInjectedDatabaseFailuresAreAtomic(t *testing.T) {
	for _, point := range []string{"inbox.begin", "inbox.load", "inbox.demand.load", "inbox.project", "inbox.record", "inbox.cursor", "inbox.commit"} {
		t.Run(point, func(t *testing.T) {
			store := testStore(t)
			store.injectFault = func(candidate string) error {
				if candidate == point {
					return errors.New("injected")
				}
				return nil
			}
			if applied, err := store.ApplyDemandBatch(context.Background(), 1, 1, []operations.DemandEvent{demandEvent(operations.DemandJobAvailable, 1)}); applied || err == nil {
				t.Fatalf("fault %s was ignored: %v %v", point, applied, err)
			}
			store.injectFault = nil
			var count int
			if err := store.db.QueryRow(`SELECT COUNT(*) FROM scale_set_inbox`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("fault %s partially committed: %d %v", point, count, err)
			}
		})
	}
	store := testStore(t)
	store.injectFault = func(point string) error {
		if point == "inbox.active.query" || point == "inbox.cursor.load" {
			return errors.New("injected")
		}
		return nil
	}
	if _, err := store.ActiveDemands(context.Background(), 1); err == nil {
		t.Fatal("active query failure ignored")
	}
	if _, err := store.DemandCursor(context.Background(), 1); err == nil {
		t.Fatal("cursor query failure ignored")
	}
}

func TestDemandInboxCorruptProjectionFailsClosed(t *testing.T) {
	store := testStore(t)
	event := demandEvent(operations.DemandJobAvailable, 1)
	if _, err := store.ApplyDemandBatch(context.Background(), 1, 1, []operations.DemandEvent{event}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE runner_demands SET labels='{'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ActiveDemands(context.Background(), 1); err == nil {
		t.Fatal("corrupt demand projection accepted")
	}
	if _, err := scanDemand(scanError{errors.New("scan")}); err == nil {
		t.Fatal("demand scan error ignored")
	}
	if _, err := scanDemand(scanError{sql.ErrNoRows}); !errors.Is(err, operations.ErrNotFound) {
		t.Fatalf("demand not found mapping: %v", err)
	}
}

func TestProjectDemandEventDrivesRunnerLifecycleAndQueuesDrain(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	events := []operations.DemandEvent{{Kind: operations.DemandJobAvailable, RunnerRequestID: 71, Owner: "owner", Repository: "repo", WorkflowRunID: 22, QueueTime: now}}
	if _, err := store.ApplyDemandBatch(ctx, 99, 1, events); err != nil {
		t.Fatal(err)
	}
	instance := operations.Instance{ID: "trf-small-project", Repo: "owner/repo", Platform: domain.PlatformLinux, Profile: "small", Route: "linux-small",
		Resources: domain.Resources{CPU: 1, MemoryMB: 2048, Slots: 1}, Demand: domain.DemandKey{Repo: "owner/repo", RunID: 22, Attempt: 1, JobID: 71},
		State: operations.StateRegistering, Ownership: operations.Ownership{ControllerID: "c", ResourceID: "d", OperationID: "spawn"}, CreatedAt: now}
	if err := store.CreateInstance(ctx, instance); err != nil {
		t.Fatal(err)
	}
	for index, kind := range []operations.DemandEventKind{operations.DemandJobAssigned, operations.DemandJobStarted, operations.DemandJobCompleted} {
		event := events[0]
		event.Kind = kind
		if _, err := store.ApplyDemandBatch(ctx, 99, int64(index+2), []operations.DemandEvent{event}); err != nil {
			t.Fatal(err)
		}
		if err := store.ProjectDemandEvent(ctx, 99, event); err != nil {
			t.Fatalf("project %s: %v", kind, err)
		}
	}
	got, err := store.Instance(ctx, instance.ID)
	if err != nil || got.State != operations.StateDraining {
		t.Fatalf("projected instance = %#v, %v", got, err)
	}
	claimed, err := store.Claim(ctx, "worker", 2, time.Now().UTC().Add(time.Minute), time.Minute)
	if err != nil || len(claimed) != 1 || claimed[0].Kind != "deregister" || claimed[0].ResourceID != instance.ID {
		t.Fatalf("drain operation = %#v, %v", claimed, err)
	}
	if err := store.ProjectDemandEvent(ctx, 99, events[0]); err != nil {
		t.Fatalf("available projection = %v", err)
	}
}
