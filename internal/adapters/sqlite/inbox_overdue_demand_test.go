package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

const overdueScaleSet = int64(3)

func overdueStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func seedQueuedDemand(t *testing.T, store *Store, requestID int64, queued time.Time) {
	t.Helper()
	event := demandEvent(operations.DemandJobAvailable, requestID)
	event.QueueTime = queued
	if _, err := store.ApplyDemandBatch(context.Background(), overdueScaleSet, requestID,
		[]operations.DemandEvent{event}); err != nil {
		t.Fatalf("seed demand: %v", err)
	}
}

func expiredAt(t *testing.T, store *Store, requestID int64) int64 {
	t.Helper()
	var value int64
	if err := store.db.QueryRow(`SELECT expired_at FROM runner_demands WHERE scale_set_id=? AND runner_request_id=?`,
		overdueScaleSet, requestID).Scan(&value); err != nil {
		t.Fatalf("read expired_at: %v", err)
	}
	return value
}

// TestOverdueDemandIsRetiredByGitHubsOwnBound is issue #315.
//
// Three daemon outages on the studio each left JobAvailable rows whose jobs had
// finished or been cancelled while the node held no session. Ghost expiry could
// never touch them -- it requires REST corroboration and the node has no REST
// observer -- so each round was retired by hand with raw SQL against a stopped
// daemon. GitHub fails any job that has not started within 24 hours, so a row
// this old describes a job that no longer exists to run.
func TestOverdueDemandIsRetiredByGitHubsOwnBound(t *testing.T) {
	store := overdueStore(t)
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	seedQueuedDemand(t, store, 901, now.Add(-49*time.Hour))

	count, err := store.ExpireOverdueDemands(context.Background(), overdueScaleSet,
		operations.OverdueDemandCriteria{Now: now, TTL: 48 * time.Hour})

	if err != nil || count != 1 {
		t.Fatalf("expire = %d, %v; want exactly the one overdue row", count, err)
	}
	if expiredAt(t, store, 901) == 0 {
		t.Fatal("the overdue row must carry its expiry")
	}
}

// A row younger than the bound is not the store's to judge, however long it has
// waited relative to anything else.
func TestADemandInsideTheBoundIsKept(t *testing.T) {
	store := overdueStore(t)
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	seedQueuedDemand(t, store, 902, now.Add(-47*time.Hour))

	count, err := store.ExpireOverdueDemands(context.Background(), overdueScaleSet,
		operations.OverdueDemandCriteria{Now: now, TTL: 48 * time.Hour})

	if err != nil || count != 0 || expiredAt(t, store, 902) != 0 {
		t.Fatalf("a 47-hour row inside a 48-hour bound was retired (count=%d err=%v)", count, err)
	}
}

// A demand a runner was ever assigned to left JobAvailable, and the guard is on
// the status exactly as ghost expiry's is: only the queued state is retirable.
func TestAnAssignedDemandIsNotOverdue(t *testing.T) {
	store := overdueStore(t)
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	event := demandEvent(operations.DemandJobAssigned, 903)
	event.QueueTime = now.Add(-72 * time.Hour)
	if _, err := store.ApplyDemandBatch(context.Background(), overdueScaleSet, 903,
		[]operations.DemandEvent{event}); err != nil {
		t.Fatalf("seed assigned demand: %v", err)
	}

	count, err := store.ExpireOverdueDemands(context.Background(), overdueScaleSet,
		operations.OverdueDemandCriteria{Now: now, TTL: 48 * time.Hour})

	if err != nil || count != 0 || expiredAt(t, store, 903) != 0 {
		t.Fatalf("an assigned demand was retired as overdue (count=%d err=%v)", count, err)
	}
}

// A row with no queue time at all is kept: absence of evidence retires nothing,
// which is the same fail-safe answer every expiry in this store gives.
func TestADemandWithoutAQueueTimeIsKept(t *testing.T) {
	store := overdueStore(t)
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	seedQueuedDemand(t, store, 904, time.Time{})

	count, err := store.ExpireOverdueDemands(context.Background(), overdueScaleSet,
		operations.OverdueDemandCriteria{Now: now, TTL: 48 * time.Hour})

	if err != nil || count != 0 || expiredAt(t, store, 904) != 0 {
		t.Fatalf("a row with no queue time was retired (count=%d err=%v)", count, err)
	}
}

// Ungrammatical criteria are refused outright, exactly as ghost expiry refuses
// them: a zero clock or bound must never silently retire everything.
func TestOverdueCriteriaAreValidated(t *testing.T) {
	store := overdueStore(t)
	for name, criteria := range map[string]operations.OverdueDemandCriteria{
		"zero now": {TTL: 48 * time.Hour},
		"zero ttl": {Now: time.Now()},
	} {
		if _, err := store.ExpireOverdueDemands(context.Background(), overdueScaleSet, criteria); !errors.Is(err, operations.ErrInvalid) {
			t.Fatalf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := store.ExpireOverdueDemands(context.Background(), 0,
		operations.OverdueDemandCriteria{Now: time.Now(), TTL: time.Hour}); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("scale set 0: want ErrInvalid, got %v", err)
	}
}

// An expired row stops being active demand: the planner's own read no longer
// sees it, which is what ends the queue-SLO breach the stale rows caused.
func TestAnOverdueExpiryRemovesTheRowFromActiveDemand(t *testing.T) {
	store := overdueStore(t)
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	seedQueuedDemand(t, store, 905, now.Add(-50*time.Hour))
	if _, err := store.ExpireOverdueDemands(context.Background(), overdueScaleSet,
		operations.OverdueDemandCriteria{Now: now, TTL: 48 * time.Hour}); err != nil {
		t.Fatal(err)
	}

	records, err := store.ActiveDemands(context.Background(), overdueScaleSet)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if record.RunnerRequestID == 905 {
			t.Fatal("an expired overdue row is still active demand")
		}
	}
}

// A closed store refuses the expiry instead of pretending it happened.
func TestOverdueExpiryOnAClosedStoreFails(t *testing.T) {
	store := overdueStore(t)
	_ = store.Close()

	if _, err := store.ExpireOverdueDemands(context.Background(), overdueScaleSet,
		operations.OverdueDemandCriteria{Now: time.Now(), TTL: time.Hour}); err == nil {
		t.Fatal("a closed store must refuse the expiry")
	}
}
