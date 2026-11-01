package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// TestIssue458ReconcileSurvivesALeaseCommitBetweenItsReadAndWrite replays the
// production stage=commit failures: the authority lease renews through its own
// connection, and when that commit lands after the reconcile transaction has
// read but before it first writes, a deferred transaction cannot upgrade its
// stale WAL snapshot and SQLite returns SQLITE_BUSY at once, without honouring
// busy_timeout. A REST snapshot must reconcile regardless of lease renewals,
// without making the store's transactions IMMEDIATE: that would let a long read
// on the shared connection block lease renewal, the starvation of issue #295.
func TestIssue458ReconcileSurvivesALeaseCommitBetweenItsReadAndWrite(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	attempts := 0
	store.injectFault = func(point string) error {
		if point != "githubjobs.replace" {
			return nil
		}
		attempts++
		if attempts > 1 {
			return nil
		}
		// A lease renewal committed on the lease connection between the
		// reconcile transaction's reads and its first write. The deferred
		// reconcile holds no write lock yet, so the renewal commits at once and
		// stales the reconcile's WAL snapshot.
		_, err := store.lease().ExecContext(ctx, `UPDATE scheduler_state SET updated_at=updated_at+1 WHERE singleton=1`)
		return err
	}
	observedAt := time.Date(2026, 11, 1, 18, 57, 30, 0, time.UTC)
	if _, err := store.ReconcileGitHubJobSnapshot(ctx, observedAt, map[int64][]operations.GitHubJobObservation{9: nil}); err != nil {
		t.Fatalf("reconcile failed after a concurrent lease commit: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want the stale-snapshot attempt and one retry", attempts)
	}
}

// TestIssue458ReconcileRetriesAreBounded keeps a pathological interleaving, a
// lease commit inside every attempt, from looping: the reconcile gives up after
// its bounded attempts and surfaces the busy-snapshot refusal.
func TestIssue458ReconcileRetriesAreBounded(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	attempts := 0
	store.injectFault = func(point string) error {
		if point != "githubjobs.replace" {
			return nil
		}
		attempts++
		_, err := store.lease().ExecContext(ctx, `UPDATE scheduler_state SET updated_at=updated_at+1 WHERE singleton=1`)
		return err
	}
	observedAt := time.Date(2026, 11, 1, 18, 57, 30, 0, time.UTC)
	_, err = store.ReconcileGitHubJobSnapshot(ctx, observedAt, map[int64][]operations.GitHubJobObservation{9: nil})
	if !busySnapshot(err) || attempts != githubJobReconcileAttempts {
		t.Fatalf("err=%v attempts=%d, want a busy snapshot after %d attempts", err, attempts, githubJobReconcileAttempts)
	}
	if busySnapshot(errors.New("database is locked (517)")) || busySnapshot(nil) {
		t.Fatal("only the driver's own SQLITE_BUSY_SNAPSHOT code is a busy snapshot")
	}
}
