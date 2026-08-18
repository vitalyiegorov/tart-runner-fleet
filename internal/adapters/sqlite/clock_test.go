package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// simEpoch is the deterministic simulation's own epoch
// (tests/simulation/world_test.go). It is quoted here rather than derived
// because the point of these tests is that a store driven by a clock thirteen
// real days away from the wall clock still produces durable rows the same clock
// can read back.
var simEpoch = time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)

// TestDemandDrainIsAvailableOnTheStoresOwnClock is issue #249.
//
// The ordinary teardown of a finished job -- a completed demand event projected
// onto a running instance -- enqueues a drain operation, and that operation's
// available_at used to come from time.Now() while every sibling timestamp in
// the operations path came from the injected clock. A worker claiming on the
// store's own clock therefore could not claim it AT ALL: availability lay in
// the process's future, the operation stayed pending forever, and its instance
// sat in draining holding its whole resource vector with nothing left to move
// it.
//
// The claim instant here is the exact instant the drain was enqueued, which is
// the strongest form of the contract: an operation with no backoff is available
// the moment it is written.
func TestDemandDrainIsAvailableOnTheStoresOwnClock(t *testing.T) {
	ctx := context.Background()
	store := clockedStore(t, func() time.Time { return simEpoch })
	instance := operations.Instance{
		ID: "vm", Repo: "owner/repo", Platform: domain.PlatformLinux, Profile: "small", Route: "linux-small",
		Resources: domain.Resources{CPU: 1, MemoryMB: 2048, Slots: 1},
		Demand:    domain.DemandKey{Repo: "owner/repo", RunID: 77, Attempt: 1, JobID: 91},
		State:     operations.StateRunning, CreatedAt: simEpoch,
		Ownership: operations.Ownership{ControllerID: "controller", ResourceID: "demand", OperationID: "spawn"},
	}
	if err := store.CreateInstance(ctx, instance); err != nil {
		t.Fatal(err)
	}
	if err := store.projectDemandRank(ctx, instance, operations.DemandJobCompleted); err != nil {
		t.Fatalf("project completion: %v", err)
	}
	claimed, err := store.Claim(ctx, "worker", 1, simEpoch, time.Minute)
	if err != nil {
		t.Fatalf("claim at the store's own instant: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed %d operations at the store's own instant; want the event drain", len(claimed))
	}
	if got := claimed[0].AvailableAt.UTC(); !got.Equal(simEpoch) {
		t.Fatalf("drain available_at = %s; want the store's clock %s", got, simEpoch)
	}
}

// TestStoreTimestampsComeFromTheInjectedClock covers the rest of the seam in
// one pass: every durable timestamp this package writes on its own initiative
// -- instance creation, a lifecycle advance, an ownership claim, a retry's
// updated_at, a statistics observation with no instant of its own -- must come
// from the store's clock, not the process's.
func TestStoreTimestampsComeFromTheInjectedClock(t *testing.T) {
	ctx := context.Background()
	store := clockedStore(t, func() time.Time { return simEpoch })
	instance := operations.Instance{ID: "vm", State: operations.StatePlanned,
		Ownership: operations.Ownership{ControllerID: "controller", ResourceID: "demand", OperationID: "spawn"}}
	if err := store.CreateInstance(ctx, instance); err != nil {
		t.Fatal(err)
	}
	created, err := store.Instance(ctx, instance.ID)
	if err != nil || !created.CreatedAt.UTC().Equal(simEpoch) || !created.UpdatedAt.UTC().Equal(simEpoch) {
		t.Fatalf("created instance = %s/%s, %v; want %s", created.CreatedAt, created.UpdatedAt, err, simEpoch)
	}
	if err := store.PutOwnership(ctx, "vm", instance.Ownership); err != nil {
		t.Fatalf("put ownership: %v", err)
	}
	var ownershipAt int64
	if err := store.db.QueryRowContext(ctx, `SELECT updated_at FROM ownership WHERE resource_name='vm'`).Scan(&ownershipAt); err != nil {
		t.Fatal(err)
	}
	if ownershipAt != simEpoch.UnixNano() {
		t.Fatalf("ownership updated_at = %d; want %d", ownershipAt, simEpoch.UnixNano())
	}
	statistics := operations.DemandStatistics{MessageID: 1, Available: 1}
	if _, err := store.PutDemandStatistics(ctx, 7, statistics); err != nil {
		t.Fatalf("put statistics: %v", err)
	}
	stored, err := store.DemandStatistics(ctx, 7)
	if err != nil || !stored.ObservedAt.UTC().Equal(simEpoch) {
		t.Fatalf("statistics observed_at = %s, %v; want %s", stored.ObservedAt, err, simEpoch)
	}
}

// TestWithClockRefusesNothingAndDefaultsToTheWallClock states the default: a
// store opened without a clock keeps the process's, and a nil clock is not a
// clock. Production wires nothing and gets time.Now, which is why this change
// is invisible there.
func TestWithClockRefusesNothingAndDefaultsToTheWallClock(t *testing.T) {
	ctx := context.Background()
	store := clockedStore(t, nil)
	before := time.Now().UTC()
	instance := operations.Instance{ID: "vm", State: operations.StatePlanned,
		Ownership: operations.Ownership{ControllerID: "controller", ResourceID: "demand", OperationID: "spawn"}}
	if err := store.CreateInstance(ctx, instance); err != nil {
		t.Fatal(err)
	}
	created, err := store.Instance(ctx, instance.ID)
	if err != nil || created.CreatedAt.Before(before) {
		t.Fatalf("created_at = %s, %v; want at or after %s", created.CreatedAt, err, before)
	}
}

func clockedStore(t *testing.T, now func() time.Time) *Store {
	t.Helper()
	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "fleet.db"), WithClock(now))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}
