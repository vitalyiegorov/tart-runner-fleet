package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	driver "modernc.org/sqlite"
)

// TestIssue458ReconcileSurvivesALeaseCommitBetweenItsReadAndWrite replays the
// production stage=commit failures: the authority lease renews through its own
// connection, and when that commit lands after the reconcile transaction has
// read but before it first writes, a deferred transaction cannot upgrade its
// stale WAL snapshot and SQLite returns SQLITE_BUSY at once, without honouring
// busy_timeout. A REST snapshot must reconcile regardless of lease renewals,
// without making the store's transactions IMMEDIATE: that would let a long read
// on the shared connection block lease renewal, the starvation of issue #295.
// With the replace as the first statement there is no read before the write, so
// a lease commit landing just before it cannot stale anything and the single
// attempt succeeds.
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
	if attempts != 1 {
		t.Fatalf("attempts = %d, want one: with the write first, a lease commit before it cannot stale a read snapshot", attempts)
	}
}

// TestIssue458ReconcileWaitsForALeaseWriterInsteadOfFailing replays the residual
// Studio failure after #460: the lease connection is still WRITING (its write
// transaction open, not yet committed) when the reconcile transaction issues its
// first statement. A deferred transaction that read first is already a read
// transaction when it tries to write, and SQLite refuses that upgrade with plain
// SQLITE_BUSY (5) at once, skipping busy_timeout; #460 retries only 517. The
// reconcile must instead wait for the brief lease write and succeed, which it
// does when its first statement is the write, taken at TRANS_NONE under the
// busy handler.
func TestIssue458ReconcileWaitsForALeaseWriterInsteadOfFailing(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	held := false
	released := make(chan error, 1)
	store.injectFault = func(point string) error {
		if held || point == "githubjobs.begin" || !strings.HasPrefix(point, "githubjobs.") {
			return nil
		}
		held = true
		// The lease connection holds an uncommitted write when the reconcile's
		// first statement runs, then commits shortly after.
		leaseTx, err := store.lease().BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := leaseTx.ExecContext(ctx, `UPDATE scheduler_state SET updated_at=updated_at+1 WHERE singleton=1`); err != nil {
			_ = leaseTx.Rollback()
			return err
		}
		go func() {
			time.Sleep(150 * time.Millisecond)
			released <- leaseTx.Commit()
		}()
		return nil
	}
	observedAt := time.Date(2026, 11, 2, 6, 3, 55, 0, time.UTC)
	_, err = store.ReconcileGitHubJobSnapshot(ctx, observedAt, map[int64][]operations.GitHubJobObservation{9: nil})
	if !held {
		t.Fatal("the replay never held the lease write")
	}
	if leaseErr := <-released; leaseErr != nil {
		t.Fatalf("the lease write failed to commit: %v", leaseErr)
	}
	if err != nil {
		var refusal *driver.Error
		code := 0
		if errors.As(err, &refusal) {
			code = refusal.Code()
		}
		t.Fatalf("reconcile failed against a brief lease write (code %d): %v", code, err)
	}
}
