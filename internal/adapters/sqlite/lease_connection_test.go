package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// The authority lease must not queue behind scheduling.
//
// The daemon's shared connection is capped at one, so every statement it issues
// waits for the one before it. A renewal is a single small UPDATE with a
// five-second budget, and on a saturated host it spent that budget waiting in
// Go's queue without reaching SQLite at all — the daemon then exited with
// "controller authority lost", and launchd restarted it into re-establishing
// every scale-set session on a host already in trouble (issue #295).
//
// This test holds the shared connection the way a long inventory read does and
// asserts the renewal still completes. Before the dedicated connection it
// blocked for as long as the reader held on.
func TestLeaseRenewalDoesNotQueueBehindTheSharedConnection(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	lease, err := store.AcquireLease(ctx, "controller", "node-b", now, 30*time.Second)
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}

	// Occupy the shared connection exactly as a long-running read does.
	held, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx on the shared connection: %v", err)
	}
	defer func() { _ = held.Rollback() }()
	if _, err := held.ExecContext(ctx, `SELECT COUNT(*) FROM leases`); err != nil {
		t.Fatalf("occupy shared connection: %v", err)
	}

	renewed := make(chan error, 1)
	go func() {
		_, err := store.RenewLease(ctx, lease, time.Now().UTC(), 30*time.Second)
		renewed <- err
	}()

	select {
	case err := <-renewed:
		if err != nil {
			t.Fatalf("RenewLease while the shared connection was busy: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("RenewLease queued behind the shared connection: this is the starvation that kills the daemon under load")
	}
}

// The fallback matters as much as the fix: a store without its own lease
// connection must still renew, because that is every in-memory and test store.
func TestLeaseFallsBackToTheSharedConnection(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	lease, err := store.AcquireLease(ctx, "controller", "node-b", now, 30*time.Second)
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}

	shared := store.leaseDB
	store.leaseDB = nil
	if store.lease() != store.db {
		t.Fatal("a store with no lease connection did not fall back to the shared one")
	}
	if _, err := store.RenewLease(ctx, lease, time.Now().UTC(), 30*time.Second); err != nil {
		t.Fatalf("RenewLease on the fallback connection: %v", err)
	}
	store.leaseDB = shared

	// And the dedicated connection is the one a real store uses.
	if store.lease() == store.db {
		t.Fatal("an opened store did not give the lease its own connection")
	}
}

// Losing the lease must still be reported as a loss through the new connection:
// the fencing guarantee is the reason the lease exists at all.
func TestLeaseLossIsStillDetectedOnItsOwnConnection(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	lease, err := store.AcquireLease(ctx, "controller", "node-b", now, 30*time.Second)
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	if _, err := store.AcquireLease(ctx, "controller", "node-b", now.Add(time.Minute), 30*time.Second); err != nil {
		t.Fatalf("re-acquire: %v", err)
	}
	// The first lease's token is now stale.
	if _, err := store.RenewLease(ctx, lease, now.Add(time.Minute), 30*time.Second); !errors.Is(err, operations.ErrLeaseLost) {
		t.Fatalf("RenewLease on a superseded lease = %v, want ErrLeaseLost", err)
	}
}
