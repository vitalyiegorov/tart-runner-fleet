package sqlite

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

func seedInstanceRow(t *testing.T, store *Store, id string, state operations.State, updatedAt time.Time, key domain.DemandKey, image string) {
	t.Helper()
	metadata, err := json.Marshal(schedulingMetadata{SchemaVersion: 1, Repo: key.Repo, Demand: key, Image: image})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO instances(id,state,version,drain_phase,ownership,scheduling_metadata,last_error,created_at,updated_at)
		VALUES(?,?,1,0,'{}',?,'',?,?)`, id, state, metadata, updatedAt.UnixNano(), updatedAt.UnixNano()); err != nil {
		t.Fatal(err)
	}
}

func TestEmptyExitsCountsDeletedIncarnationsOnly(t *testing.T) {
	store := overdueStore(t)
	now := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	poisoned := domain.DemandKey{Repo: "o/r", RunID: 41979555666, Attempt: 1, JobID: 5099379140235310121}
	other := domain.DemandKey{Repo: "o/r", RunID: 2, Attempt: 1, JobID: 8}

	seedInstanceRow(t, store, "a", operations.StateDeleted, now.Add(-time.Hour), poisoned, "base@1")
	seedInstanceRow(t, store, "b", operations.StateDeleted, now.Add(-time.Minute), poisoned, "base@1")
	seedInstanceRow(t, store, "c", operations.StateFailed, now, poisoned, "base@1")
	seedInstanceRow(t, store, "d", operations.StateRunning, now, poisoned, "base@1")
	seedInstanceRow(t, store, "e", operations.StateDeleted, now.Add(-72*time.Hour), other, "base@1")
	seedInstanceRow(t, store, "f", operations.StateDeleted, now, poisoned, "base@2")
	seedInstanceRow(t, store, "g", operations.StateDeleted, now, poisoned, "")
	if _, err := store.db.Exec(`INSERT INTO instances(id,state,version,drain_phase,ownership,scheduling_metadata,last_error,created_at,updated_at)
		VALUES('legacy',?,1,0,'{}','{}','',?,?)`, operations.StateDeleted, now.UnixNano(), now.UnixNano()); err != nil {
		t.Fatal(err)
	}

	exits, err := store.EmptyExits(context.Background(), now.Add(-48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(exits) != 3 {
		t.Fatalf("got %+v", exits)
	}
	got := exits[domain.EmptyExitKey{Demand: poisoned, Image: "base@1"}]
	if got.Count != 2 || !got.Last.Equal(now.Add(-time.Minute)) {
		t.Fatalf("got %+v", got)
	}
	if rebuilt := exits[domain.EmptyExitKey{Demand: poisoned, Image: "base@2"}]; rebuilt.Count != 1 {
		t.Fatalf("each generation counts its own: %+v", rebuilt)
	}
	if older := exits[domain.EmptyExitKey{Demand: poisoned}]; older.Count != 1 {
		t.Fatalf("a row older than the field is its own, empty, generation: %+v", older)
	}
}

func TestEmptyExitsReportsARowItCannotReadAndAStoreItCannotQuery(t *testing.T) {
	store := overdueStore(t)
	now := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	// A text ID satisfies the >0 filter (SQLite orders text above numbers) and
	// cannot be scanned, so the malformed row fails the read rather than vanishing.
	if _, err := store.db.Exec(`INSERT INTO instances(id,state,version,drain_phase,ownership,scheduling_metadata,last_error,created_at,updated_at)
		VALUES('bad',?,1,0,'{}','{"demand":{"Repo":"o/r","RunID":1,"Attempt":1,"JobID":"x"}}','',?,?)`,
		operations.StateDeleted, now.UnixNano(), now.UnixNano()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EmptyExits(context.Background(), now.Add(-time.Hour)); err == nil {
		t.Fatal("a row that cannot be read must not read as no exits")
	}
	_ = store.Close()
	if _, err := store.EmptyExits(context.Background(), now.Add(-time.Hour)); err == nil {
		t.Fatal("an unreadable store must not read as no exits")
	}
}

// One job named on a row of this generation clears everything before it, on this
// generation only, and launches are dated from every row whatever its state.
func TestAnAssignmentOnAGenerationEndsItsUnassignedRun(t *testing.T) {
	ctx := context.Background()
	store := overdueStore(t)
	now := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	queued := domain.DemandKey{Repo: "o/r", RunID: 1, Attempt: 1, JobID: 8}
	seedInstanceRow(t, store, "a", operations.StateDeleted, now.Add(-time.Hour), queued, "base@1")
	seedInstanceRow(t, store, "b", operations.StateDeleted, now.Add(-time.Minute), queued, "base@1")
	seedInstanceRow(t, store, "c", operations.StateDeleted, now.Add(-time.Minute), queued, "base@2")
	seedInstanceRow(t, store, "ran", operations.StateRunning, now.Add(-time.Second), domain.DemandKey{Repo: "o/r", RunID: 2, Attempt: 1, JobID: 9}, "base@1")
	read := func() map[domain.EmptyExitKey]domain.EmptyExits {
		t.Helper()
		exits, err := store.EmptyExits(ctx, now.Add(-2*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		return exits
	}
	before := read()[domain.EmptyExitKey{Demand: queued, Image: "base@1"}]
	if before.Count != 2 || before.Unassigned != 2 || !before.Launched.Equal(now.Add(-time.Second)) {
		t.Fatalf("got %+v", before)
	}

	store.clock = func() time.Time { return now.Add(-30 * time.Minute) }
	started := demandEvent(operations.DemandJobStarted, 9)
	started.RunnerName = "ran"
	if _, err := store.ApplyDemandBatch(ctx, overdueScaleSet, 9, []operations.DemandEvent{started}); err != nil {
		t.Fatal(err)
	}
	after := read()
	if got := after[domain.EmptyExitKey{Demand: queued, Image: "base@1"}]; got.Count != 2 || got.Unassigned != 1 {
		t.Fatalf("only the exit after the assignment still counts: %+v", got)
	}
	if got := after[domain.EmptyExitKey{Demand: queued, Image: "base@2"}]; got.Unassigned != 1 {
		t.Fatalf("an assignment on another generation clears nothing here: %+v", got)
	}

	unnamed := demandEvent(operations.DemandJobAssigned, 10)
	if _, err := store.ApplyDemandBatch(ctx, overdueScaleSet, 10, []operations.DemandEvent{unnamed}); err != nil {
		t.Fatal(err)
	}
	if got := read()[domain.EmptyExitKey{Demand: queued, Image: "base@1"}]; got.Unassigned != 1 {
		t.Fatalf("a row naming no runner says nothing about a generation: %+v", got)
	}
}

// An operator reaping a Failed instance is clearing up a provisioning failure that
// never reached a runner, and it must not read as that demand's runner exiting empty.
func TestAReapedFailedInstanceIsNotAnEmptyExit(t *testing.T) {
	ctx := context.Background()
	store := overdueStore(t)
	now := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	key := domain.DemandKey{Repo: "o/r", RunID: 1, Attempt: 1, JobID: 8}
	metadata, err := json.Marshal(schedulingMetadata{SchemaVersion: 1, Repo: key.Repo, Demand: key, Image: "base@1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		id        string
		state     operations.State
		lastError string
	}{{"failed", operations.StateFailed, "acquire"}, {"stopping", operations.StateStopping, ""}} {
		if _, err := store.db.Exec(`INSERT INTO instances(id,state,version,drain_phase,ownership,scheduling_metadata,last_error,created_at,updated_at)
			VALUES(?,?,1,0,'{}',?,?,?,?)`, row.id, row.state, metadata, row.lastError, now.UnixNano(), now.UnixNano()); err != nil {
			t.Fatal(err)
		}
		tx, err := store.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.reapInstance(ctx, tx, operations.Instance{ID: row.id, State: row.state, Version: 1}, now.UnixNano()); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	exits, err := store.EmptyExits(ctx, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got := exits[domain.EmptyExitKey{Demand: key, Image: "base@1"}]; got.Count != 1 {
		t.Fatalf("only the instance that reached a runner counts: %+v", got)
	}
}
