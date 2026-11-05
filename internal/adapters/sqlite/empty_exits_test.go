package sqlite

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

func seedInstanceRow(t *testing.T, store *Store, id string, state operations.State, updatedAt time.Time, key domain.DemandKey) {
	t.Helper()
	metadata, err := json.Marshal(schedulingMetadata{SchemaVersion: 1, Repo: key.Repo, Demand: key})
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

	seedInstanceRow(t, store, "a", operations.StateDeleted, now.Add(-time.Hour), poisoned)
	seedInstanceRow(t, store, "b", operations.StateDeleted, now.Add(-time.Minute), poisoned)
	seedInstanceRow(t, store, "c", operations.StateFailed, now, poisoned)
	seedInstanceRow(t, store, "d", operations.StateRunning, now, poisoned)
	seedInstanceRow(t, store, "e", operations.StateDeleted, now.Add(-72*time.Hour), other)
	if _, err := store.db.Exec(`INSERT INTO instances(id,state,version,drain_phase,ownership,scheduling_metadata,last_error,created_at,updated_at)
		VALUES('legacy',?,1,0,'{}','{}','',?,?)`, operations.StateDeleted, now.UnixNano(), now.UnixNano()); err != nil {
		t.Fatal(err)
	}

	exits, err := store.EmptyExits(context.Background(), now.Add(-48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(exits) != 1 {
		t.Fatalf("got %+v", exits)
	}
	got := exits[poisoned]
	if got.Count != 2 || !got.Last.Equal(now.Add(-time.Minute)) {
		t.Fatalf("got %+v", got)
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
