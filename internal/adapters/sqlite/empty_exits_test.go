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

// The evidence comes from the broker alone: a job can write neither fact.
func TestInstanceTookJobReadsOnlyWhatTheBrokerSaid(t *testing.T) {
	ctx := context.Background()
	store := overdueStore(t)
	took := func(id string) bool {
		t.Helper()
		result, err := store.InstanceTookJob(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	if _, err := store.db.Exec(`INSERT INTO transition_history(instance_id,from_state,to_state,version,created_at)
		VALUES('assigned-only',?,?,1,1),('ran',?,?,2,1)`, operations.StateRegistering, operations.StateAssigned,
		operations.StateAssigned, operations.StateRunning); err != nil {
		t.Fatal(err)
	}
	if took("assigned-only") {
		t.Fatal("provisioning reaches Assigned with no job at all")
	}
	if !took("ran") {
		t.Fatal("an instance that entered Running took a job")
	}
	assigned := demandEvent(operations.DemandJobAssigned, 7)
	assigned.RunnerName = "named"
	if _, err := store.ApplyDemandBatch(ctx, overdueScaleSet, 7, []operations.DemandEvent{assigned}); err != nil {
		t.Fatal(err)
	}
	if !took("named") {
		t.Fatal("GitHub naming the runner on a job's row is a job, whatever the instance row says")
	}
	if took("stranger") {
		t.Fatal("no evidence is no evidence")
	}
	_ = store.Close()
	if _, err := store.InstanceTookJob(ctx, "ran"); err == nil {
		t.Fatal("an unreadable store must not read as no job")
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
