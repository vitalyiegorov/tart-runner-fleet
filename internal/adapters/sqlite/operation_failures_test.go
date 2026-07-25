package sqlite

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// The 2026-07-25 phantom burned 397 attempts while `fleet operations` could
// only say "retrying 1". The durable rows always knew more; the aggregate must
// carry the closed failure code and the worst attempt count so an operator sees
// the cause and its age without opening the database (which is forbidden while
// the daemon runs).
func TestOperationFailuresAggregateClosedCodesAndWorstAttempts(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Unix(400, 0).UTC()

	failures, err := store.OperationFailures(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(failures) != 0 {
		t.Fatalf("healthy fleet must report no failures, got %#v", failures)
	}

	insert := func(id, kind, lastError string, status operations.OperationStatus, attempts int) {
		t.Helper()
		if _, err := store.db.ExecContext(ctx, `INSERT INTO operations(
			id,idempotency_key,effect_key,kind,resource_id,payload,status,attempts,available_at,
			lease_owner,lease_until,last_error,created_at,updated_at)
			VALUES(?,?,?,?,?,'{}',?,?,?,'',0,?,?,?)`, id, id, kind+":"+id, kind, id, status, attempts,
			now.UnixNano(), lastError, now.UnixNano(), now.UnixNano()); err != nil {
			t.Fatal(err)
		}
	}
	insert("op-1", "deregister", "runner lifecycle failed at deregister (runner_busy)", operations.OperationPending, 397)
	insert("op-2", "deregister", "runner lifecycle failed at deregister (runner_busy)", operations.OperationPending, 12)
	insert("op-3", "deregister", "runner lifecycle failed at deregister (runner_forbidden)", operations.OperationDead, 5)
	insert("op-4", "clone", "runner lifecycle failed at acquire_jit", operations.OperationPending, 3)
	// Completed work and anything the executors did not author must never reach an
	// operator surface as free-form text.
	insert("op-5", "deregister", "runner lifecycle failed at deregister (runner_busy)", operations.OperationCompleted, 40)
	insert("op-6", "clone", "executor panic: runtime error: index out of range", operations.OperationPending, 1)

	failures, err = store.OperationFailures(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []operations.OperationFailure{
		{Kind: "clone", Code: "acquire_jit", Count: 1, Attempts: 3},
		{Kind: "clone", Code: "unclassified", Count: 1, Attempts: 1},
		{Kind: "deregister", Code: "deregister:runner_busy", Count: 2, Attempts: 397},
		{Kind: "deregister", Code: "deregister:runner_forbidden", Count: 1, Attempts: 5},
	}
	if !reflect.DeepEqual(failures, want) {
		t.Fatalf("failures=%#v", failures)
	}
}

func TestOperationFailuresFailsClosedWhenDatabaseUnavailable(t *testing.T) {
	store := testStore(t)
	if err := store.db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.OperationFailures(context.Background()); err == nil {
		t.Fatal("closed database failure aggregate succeeded")
	}
}

// Rule 4: a partially readable aggregate is an unavailable observation, never a
// shorter list that reads as a healthier fleet.
func TestOperationFailuresFailClosedOnRowFaults(t *testing.T) {
	for name, rows := range map[string]*injectedRows{
		"scan":    {next: true, scanErr: errors.New("scan")},
		"iterate": {rowsErr: errors.New("iterate")},
	} {
		t.Run(name, func(t *testing.T) {
			store := testStore(t)
			store.injectRows = func(point string) rowsScanner {
				if point == "operations.failures.query" {
					return rows
				}
				return nil
			}
			if _, err := store.OperationFailures(context.Background()); err == nil {
				t.Fatal("row failure ignored")
			}
		})
	}
}
