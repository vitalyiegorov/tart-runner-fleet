package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/lifecycle"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func seed(t *testing.T, store *Store, now time.Time) (operations.Instance, operations.Operation) {
	t.Helper()
	ownership := operations.Ownership{ControllerID: "controller", ResourceID: "vm", OperationID: "create"}
	instance := operations.Instance{ID: "vm", State: operations.StatePlanned, Ownership: ownership, CreatedAt: now}
	if err := store.CreateInstance(context.Background(), instance); err != nil {
		t.Fatal(err)
	}
	op := operations.Operation{ID: "clone", IdempotencyKey: "clone-vm", EffectKey: "clone-vm", Kind: "clone", ResourceID: "vm", AvailableAt: now}
	got, queued, err := store.Transition(context.Background(), operations.Transition{InstanceID: "vm", ExpectedState: operations.StatePlanned, ExpectedVersion: 0, NextState: operations.StateCloning, Operation: op})
	if err != nil {
		t.Fatal(err)
	}
	return got, queued
}

func TestMigrateTransitionClaimCompleteAndRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fleet.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0).UTC()
	instance, queued := seed(t, store, now)
	if instance.State != operations.StateCloning || instance.Version != 1 || queued.Status != operations.OperationPending {
		t.Fatalf("unexpected transition: %#v %#v", instance, queued)
	}
	duplicate, duplicateOp, err := store.Transition(ctx, operations.Transition{InstanceID: "vm", ExpectedState: operations.StatePlanned, ExpectedVersion: 0, NextState: operations.StateCloning, Operation: queued})
	if err != nil || duplicate.Version != 1 || duplicateOp.ID != queued.ID {
		t.Fatalf("idempotent transition failed: %#v %#v %v", duplicate, duplicateOp, err)
	}
	claimed, err := store.Claim(ctx, "worker", 1, now, time.Minute)
	if err != nil || len(claimed) != 1 || claimed[0].LeaseOwner != "worker" {
		t.Fatalf("claim failed: %#v %v", claimed, err)
	}
	applied, err := store.Complete(ctx, "clone", "worker", "clone-vm", now)
	if err != nil || !applied {
		t.Fatalf("complete failed: %v %v", applied, err)
	}
	applied, err = store.Complete(ctx, "clone", "worker", "clone-vm", now)
	if err != nil || applied {
		t.Fatalf("duplicate effect was applied: %v %v", applied, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	persisted, err := reopened.Instance(ctx, "vm")
	if err != nil || persisted.State != operations.StateCloning {
		t.Fatalf("restart lost state: %#v %v", persisted, err)
	}
}

func TestAdvanceLifecycleStateUsesStateAndVersionCAS(t *testing.T) {
	store := testStore(t)
	now := time.Unix(100, 0).UTC()
	instance := operations.Instance{ID: "advance", State: operations.StatePlanned, Version: 0,
		Ownership: operations.Ownership{ControllerID: "controller", ResourceID: "demand", OperationID: "spawn"}, CreatedAt: now}
	if err := store.CreateInstance(context.Background(), instance); err != nil {
		t.Fatal(err)
	}
	advanced, err := store.Advance(context.Background(), lifecycle.StateChange{InstanceID: instance.ID, ExpectedState: operations.StatePlanned,
		ExpectedVersion: instance.Version, NextState: operations.StateCloning})
	if err != nil || advanced.State != operations.StateCloning || advanced.Version != instance.Version+1 {
		t.Fatalf("Advance() = %#v, %v", advanced, err)
	}
	if _, err := store.Advance(context.Background(), lifecycle.StateChange{InstanceID: instance.ID, ExpectedState: operations.StatePlanned,
		ExpectedVersion: instance.Version, NextState: operations.StateCloning}); !errors.Is(err, operations.ErrConflict) {
		t.Fatalf("stale Advance error = %v", err)
	}
	failed, err := store.Advance(context.Background(), lifecycle.StateChange{InstanceID: instance.ID, ExpectedState: operations.StateCloning,
		ExpectedVersion: advanced.Version, NextState: operations.StateFailed, FailureCode: "bootstrap"})
	if err != nil || failed.State != operations.StateFailed || failed.LastError != "bootstrap" {
		t.Fatalf("failure Advance() = %#v, %v", failed, err)
	}
	if _, err := store.Advance(context.Background(), lifecycle.StateChange{}); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("invalid Advance error = %v", err)
	}
}

func TestAdvanceRollsBackEveryDurabilityStageFailure(t *testing.T) {
	for _, point := range []string{"advance.instance", "advance.history", "advance.result", "advance.commit"} {
		t.Run(point, func(t *testing.T) {
			store := testStore(t)
			instance := operations.Instance{ID: "advance-failure", State: operations.StatePlanned,
				Ownership: operations.Ownership{ControllerID: "controller", ResourceID: "demand", OperationID: "spawn"}}
			if err := store.CreateInstance(context.Background(), instance); err != nil {
				t.Fatal(err)
			}
			store.injectFault = func(candidate string) error {
				if candidate == point {
					return errors.New("injected")
				}
				return nil
			}
			if _, err := store.Advance(context.Background(), lifecycle.StateChange{InstanceID: instance.ID,
				ExpectedState: operations.StatePlanned, ExpectedVersion: 0, NextState: operations.StateCloning}); err == nil {
				t.Fatal("advance stage failure was ignored")
			}
			store.injectFault = nil
			got, err := store.Instance(context.Background(), instance.ID)
			if err != nil || got.State != operations.StatePlanned || got.Version != 0 {
				t.Fatalf("failed advance partially committed: %#v, %v", got, err)
			}
			var history int
			if err := store.db.QueryRow(`SELECT COUNT(*) FROM transition_history WHERE instance_id=?`, instance.ID).Scan(&history); err != nil || history != 0 {
				t.Fatalf("failed advance persisted history: %d, %v", history, err)
			}
		})
	}
}

func TestOperationCountsFailsClosedWhenDatabaseUnavailable(t *testing.T) {
	store := testStore(t)
	if err := store.db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.OperationCounts(context.Background()); err == nil {
		t.Fatal("closed database operation summary succeeded")
	}
}

func TestRetryRecoveryOwnershipAndErrors(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Unix(200, 0).UTC()
	_, _ = seed(t, store, now)
	if _, err := store.Instance(ctx, "missing"); !errors.Is(err, operations.ErrNotFound) {
		t.Fatalf("missing instance: %v", err)
	}
	claimed, err := store.Claim(ctx, "worker", 1, now, time.Second)
	if err != nil || len(claimed) != 1 {
		t.Fatal(err)
	}
	if err := store.Retry(ctx, "clone", "other", "failed", now, false); !errors.Is(err, operations.ErrLeaseLost) {
		t.Fatalf("wrong owner retry: %v", err)
	}
	if err := store.Retry(ctx, "clone", "worker", "failed", now.Add(time.Minute), false); err != nil {
		t.Fatal(err)
	}
	if claimed, err = store.Claim(ctx, "worker", 1, now, time.Second); err != nil || len(claimed) != 0 {
		t.Fatalf("early claim: %#v %v", claimed, err)
	}
	claimed, err = store.Claim(ctx, "worker", 1, now.Add(time.Minute), time.Second)
	if err != nil || len(claimed) != 1 || claimed[0].Attempts != 1 {
		t.Fatalf("retry claim: %#v %v", claimed, err)
	}
	if count, err := store.RecoverExpired(ctx, now.Add(2*time.Minute)); err != nil || count != 1 {
		t.Fatalf("recovery: %d %v", count, err)
	}
	ownership := operations.Ownership{ControllerID: "c", ResourceID: "r", OperationID: "o"}
	if err := store.PutOwnership(ctx, "vm-name", ownership); err != nil {
		t.Fatal(err)
	}
	if err := store.PutOwnership(ctx, "vm-name", ownership); err != nil {
		t.Fatal(err)
	}
	got, err := store.Ownership(ctx, "vm-name")
	if err != nil || got != ownership {
		t.Fatalf("ownership: %#v %v", got, err)
	}
	other := ownership
	other.OperationID = "other"
	if err := store.PutOwnership(ctx, "vm-name", other); !errors.Is(err, operations.ErrConflict) {
		t.Fatalf("ownership conflict: %v", err)
	}
}

func TestOperationCountsExcludeCompletedAndSeparateDead(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Unix(250, 0).UTC()
	_, _ = seed(t, store, now)
	if retrying, dead, err := store.OperationCounts(ctx); err != nil || retrying != 0 || dead != 0 {
		t.Fatalf("initial retrying=%d dead=%d err=%v", retrying, dead, err)
	}
	claimed, err := store.Claim(ctx, "worker", 1, now, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatal(err)
	}
	if err := store.Retry(ctx, claimed[0].ID, "worker", "retry", now.Add(time.Minute), false); err != nil {
		t.Fatal(err)
	}
	if retrying, dead, err := store.OperationCounts(ctx); err != nil || retrying != 1 || dead != 0 {
		t.Fatalf("retrying=%d dead=%d err=%v", retrying, dead, err)
	}
	claimed, err = store.Claim(ctx, "worker", 1, now.Add(time.Minute), time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatal(err)
	}
	if err := store.Retry(ctx, claimed[0].ID, "worker", "terminal", now.Add(2*time.Minute), true); err != nil {
		t.Fatal(err)
	}
	if retrying, dead, err := store.OperationCounts(ctx); err != nil || retrying != 0 || dead != 1 {
		t.Fatalf("terminal retrying=%d dead=%d err=%v", retrying, dead, err)
	}
}

func TestConcurrentLeaseClaimUsesFencing(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Unix(300, 0).UTC()
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, owner := range []string{"one", "two"} {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			<-start
			_, err := store.AcquireLease(ctx, "fleet", owner, now, time.Minute)
			results <- err
		}(owner)
	}
	close(start)
	wg.Wait()
	close(results)
	success, held := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, operations.ErrLeaseHeld) {
			held++
		} else {
			t.Fatalf("unexpected lease error: %v", err)
		}
	}
	if success != 1 || held != 1 {
		t.Fatalf("lease results success=%d held=%d", success, held)
	}
	lease, err := store.AcquireLease(ctx, "fleet", "three", now.Add(2*time.Minute), time.Minute)
	if err != nil || lease.Token != 2 {
		t.Fatalf("fenced lease: %#v %v", lease, err)
	}
	lease, err = store.RenewLease(ctx, lease, now.Add(2*time.Minute+time.Second), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReleaseLease(ctx, lease); err != nil {
		t.Fatal(err)
	}
	if err := store.ReleaseLease(ctx, lease); !errors.Is(err, operations.ErrLeaseLost) {
		t.Fatalf("double release: %v", err)
	}
}

func TestApplyPlanIsAtomicIdempotentAndConcurrentSafe(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Unix(400, 0).UTC()
	ownership := operations.Ownership{ControllerID: "controller", ResourceID: "vm-plan", OperationID: "plan-1"}
	operation := operations.Operation{ID: "plan-op", IdempotencyKey: "plan-op", EffectKey: "plan-op", Kind: "clone", ResourceID: "vm-plan", AvailableAt: now}
	plan := operations.Plan{
		ID: "plan-1", ExpectedSchedulerVersion: 0, CreatedAt: now,
		Scheduler:  operations.SchedulerState{Version: 1, Data: []byte(`{"mode":"linux"}`), Reservations: []byte(`[{"job":1}]`), DeficitRoundRobin: []byte(`{"repo":2}`), ObservationCursor: "cursor-1"},
		Instances:  []operations.InstanceIntent{{ExpectedVersion: -1, Instance: operations.Instance{ID: "vm-plan", State: operations.StatePlanned, Ownership: ownership}}},
		Operations: []operations.Operation{operation},
	}
	start := make(chan struct{})
	results := make(chan struct {
		applied bool
		err     error
	}, 2)
	for range 2 {
		go func() {
			<-start
			applied, err := store.ApplyPlan(ctx, plan)
			results <- struct {
				applied bool
				err     error
			}{applied, err}
		}()
	}
	close(start)
	applied, duplicate := 0, 0
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatalf("concurrent plan: %v", result.err)
		}
		if result.applied {
			applied++
		} else {
			duplicate++
		}
	}
	if applied != 1 || duplicate != 1 {
		t.Fatalf("applied=%d duplicate=%d", applied, duplicate)
	}
	state, err := store.SchedulerState(ctx)
	if err != nil || state.Version != 1 || state.ObservationCursor != "cursor-1" || string(state.Reservations) != `[{"job":1}]` {
		t.Fatalf("scheduler state: %#v %v", state, err)
	}
	claimed, err := store.Claim(ctx, "worker", 10, now, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("outbox not atomic: %#v %v", claimed, err)
	}
	conflict := plan
	conflict.Scheduler.ObservationCursor = "different"
	if _, err := store.ApplyPlan(ctx, conflict); !errors.Is(err, operations.ErrConflict) {
		t.Fatalf("conflicting duplicate accepted: %v", err)
	}
	next := plan
	next.ID = "plan-2"
	next.ExpectedSchedulerVersion = 0
	next.Scheduler.Version = 1
	if _, err := store.ApplyPlan(ctx, next); !errors.Is(err, operations.ErrConflict) {
		t.Fatalf("stale scheduler state accepted: %v", err)
	}
}

func TestOperationDependenciesGateClaimsAndPropagateDeadLetters(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Unix(450, 0).UTC()
	operation := func(id string, dependencies ...string) operations.Operation {
		return operations.Operation{ID: id, IdempotencyKey: id, EffectKey: id, Kind: "test", ResourceID: "vm", AvailableAt: now, DependsOn: dependencies}
	}
	plan := operations.Plan{ID: "dependencies", CreatedAt: now, Scheduler: operations.SchedulerState{Version: 1}, Operations: []operations.Operation{operation("root"), operation("child", "root")}}
	if applied, err := store.ApplyPlan(ctx, plan); err != nil || !applied {
		t.Fatalf("apply dependency plan: %v %v", applied, err)
	}
	claimed, err := store.Claim(ctx, "worker", 10, now, time.Minute)
	if err != nil || len(claimed) != 1 || claimed[0].ID != "root" {
		t.Fatalf("dependency did not gate claim: %#v %v", claimed, err)
	}
	if _, err := store.Complete(ctx, "root", "worker", "root", now); err != nil {
		t.Fatal(err)
	}
	claimed, err = store.Claim(ctx, "worker", 10, now, time.Minute)
	if err != nil || len(claimed) != 1 || claimed[0].ID != "child" {
		t.Fatalf("completed dependency did not release child: %#v %v", claimed, err)
	}

	second := operations.Plan{ID: "dead-dependencies", ExpectedSchedulerVersion: 1, CreatedAt: now.Add(time.Second), Scheduler: operations.SchedulerState{Version: 2}, Operations: []operations.Operation{operation("dead-root"), operation("dead-child", "dead-root")}}
	if _, err := store.ApplyPlan(ctx, second); err != nil {
		t.Fatal(err)
	}
	claimed, err = store.Claim(ctx, "worker", 10, now.Add(time.Second), time.Minute)
	if err != nil || len(claimed) != 1 || claimed[0].ID != "dead-root" {
		t.Fatalf("claim dead root: %#v %v", claimed, err)
	}
	if err := store.Retry(ctx, "dead-root", "worker", "terminal", now, true); err != nil {
		t.Fatal(err)
	}
	if claimed, err = store.Claim(ctx, "worker", 10, now.Add(time.Second), time.Minute); err != nil || len(claimed) != 0 {
		t.Fatalf("dead child claimed: %#v %v", claimed, err)
	}
	var status, lastError string
	if err := store.db.QueryRowContext(ctx, `SELECT status,last_error FROM operations WHERE id='dead-child'`).Scan(&status, &lastError); err != nil || status != string(operations.OperationDead) || lastError != "dependency dead" {
		t.Fatalf("dead dependency was not propagated: %q %q %v", status, lastError, err)
	}
}

func TestTransitionDependencyGatesOperation(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Unix(455, 0).UTC()
	_, _ = seed(t, store, now)
	ownership := operations.Ownership{ControllerID: "c", ResourceID: "second", OperationID: "create"}
	if err := store.CreateInstance(ctx, operations.Instance{ID: "second", State: operations.StatePlanned, Ownership: ownership, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	op := operations.Operation{ID: "dependent", IdempotencyKey: "dependent", EffectKey: "dependent", Kind: "test", ResourceID: "second", AvailableAt: now, DependsOn: []string{"clone"}}
	if _, _, err := store.Transition(ctx, operations.Transition{InstanceID: "second", ExpectedState: operations.StatePlanned, NextState: operations.StateCloning, Operation: op}); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.Claim(ctx, "worker", 10, now, time.Minute)
	if err != nil || len(claimed) != 1 || claimed[0].ID != "clone" {
		t.Fatalf("transition dependency did not gate: %#v %v", claimed, err)
	}
	missingOwnership := operations.Ownership{ControllerID: "c", ResourceID: "missing", OperationID: "create"}
	if err := store.CreateInstance(ctx, operations.Instance{ID: "missing-dep", State: operations.StatePlanned, Ownership: missingOwnership, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	missing := op
	missing.ID, missing.IdempotencyKey, missing.EffectKey = "missing-op", "missing-op", "missing-op"
	missing.ResourceID = "missing-dep"
	missing.DependsOn = []string{"does-not-exist"}
	if _, _, err := store.Transition(ctx, operations.Transition{InstanceID: "missing-dep", ExpectedState: operations.StatePlanned, NextState: operations.StateCloning, Operation: missing}); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("missing transition dependency accepted: %v", err)
	}
	instance, err := store.Instance(ctx, "missing-dep")
	if err != nil || instance.State != operations.StatePlanned {
		t.Fatalf("failed transition was not rolled back: %#v %v", instance, err)
	}
}

func TestApplyPlanRejectsMissingDependencyAtomically(t *testing.T) {
	store := testStore(t)
	now := time.Unix(460, 0).UTC()
	operation := operations.Operation{ID: "child", IdempotencyKey: "child", EffectKey: "child", Kind: "test", ResourceID: "vm", AvailableAt: now, DependsOn: []string{"missing"}}
	plan := operations.Plan{ID: "missing-dependency", CreatedAt: now, Scheduler: operations.SchedulerState{Version: 1}, Operations: []operations.Operation{operation}}
	if applied, err := store.ApplyPlan(context.Background(), plan); applied || !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("missing dependency accepted: %v %v", applied, err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM operations WHERE id='child'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed plan was not rolled back: %d %v", count, err)
	}
}

func TestMigrationVersionWALPermissionsAndUpgrade(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "private", "fleet.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	var journal string
	if err := store.db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != 9 {
		t.Fatalf("migration version=%d err=%v", version, err)
	}
	if err := store.db.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil || journal != "wal" {
		t.Fatalf("journal mode=%q err=%v", journal, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("database permissions=%v err=%v", info.Mode().Perm(), err)
	}
	if _, err := store.db.Exec(`DROP TABLE operation_dependencies`); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"scale_set_inbox", "scale_set_cursors", "runner_demands"} {
		if _, err := store.db.Exec(`DROP TABLE ` + table); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.Exec(`ALTER TABLE instances DROP COLUMN scheduling_metadata`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DELETE FROM schema_migrations WHERE version>=3`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	if err := upgraded.db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != 9 {
		t.Fatalf("upgrade version=%d err=%v", version, err)
	}
}

func TestMigrationNineOrdersDeadAcquireProvisionIntoCleanup(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	now := time.Unix(464, 0).UTC().UnixNano()
	ownership := `{"controller_id":"tart-runner-fleet","resource_id":"owner/repo/1/1/2","operation_id":"dead-clone"}`
	metadata := `{"schema_version":1,"repo":"owner/repo","platform":"linux","profile":"small","route":"linux-small","resources":{"CPU":1,"MemoryMB":2048,"Slots":1},"demand":{"Repo":"owner/repo","RunID":1,"Attempt":1,"JobID":2}}`
	if _, err := store.db.Exec(`INSERT INTO instances(id,state,version,drain_phase,ownership,scheduling_metadata,last_error,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		"orphan", operations.StateReachable, 3, 0, ownership, metadata, "", now, now); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []struct{ id, resource, effect, failure string }{
		{"dead-clone", "orphan", "clone:orphan", "runner lifecycle failed at acquire_jit"},
		{"unrelated-clone", "orphan", "clone:other", "runner lifecycle failed at acquire_jit"},
	} {
		if _, err := store.db.Exec(`INSERT INTO operations(id,idempotency_key,effect_key,kind,resource_id,payload,status,attempts,available_at,lease_owner,lease_until,last_error,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			operation.id, operation.id, operation.effect, lifecycle.OperationProvision, operation.resource, `{}`, operations.OperationDead, 5,
			now, "", 0, operation.failure, now, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := store.db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != 9 {
		t.Fatalf("migration version=%d err=%v", version, err)
	}
	instance, err := store.Instance(ctx, "orphan")
	if err != nil || instance.State != operations.StateDraining || instance.DrainPhase != 1 {
		t.Fatalf("recovered instance=%#v err=%v", instance, err)
	}
	var status string
	var attempts int
	if err := store.db.QueryRow(`SELECT status,attempts FROM operations WHERE id='dead-clone'`).Scan(&status, &attempts); err != nil || status != string(operations.OperationPending) || attempts != 0 {
		t.Fatalf("provision recovery status=%q attempts=%d err=%v", status, attempts, err)
	}
	if err := store.db.QueryRow(`SELECT status,attempts FROM operations WHERE id='unrelated-clone'`).Scan(&status, &attempts); err != nil || status != string(operations.OperationDead) || attempts != 5 {
		t.Fatalf("unrelated operation changed: status=%q attempts=%d err=%v", status, attempts, err)
	}
	var drainKind, dependency string
	if err := store.db.QueryRow(`SELECT kind FROM operations WHERE id='recovery-drain-orphan'`).Scan(&drainKind); err != nil || drainKind != lifecycle.OperationDrain {
		t.Fatalf("recovery drain=%q err=%v", drainKind, err)
	}
	if err := store.db.QueryRow(`SELECT depends_on FROM operation_dependencies WHERE operation_id='recovery-drain-orphan'`).Scan(&dependency); err != nil || dependency != "dead-clone" {
		t.Fatalf("recovery dependency=%q err=%v", dependency, err)
	}
}

func TestMigrationSixRevivesOnlyKnownEphemeralDeregisterDeadLetter(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	now := time.Unix(465, 0).UTC().UnixNano()
	ownership := `{"controller_id":"controller","resource_id":"job","operation_id":"spawn"}`
	metadata := `{"schema_version":1,"repo":"owner/repo","platform":"darwin","profile":"maestro","route":"macos-maestro","resources":{"cpu":4,"memory_mb":7168,"slots":1},"demand":{"repo":"owner/repo","run_id":1,"job_id":2,"attempt":1}}`
	if _, err := store.db.Exec(`INSERT INTO instances(id,state,version,drain_phase,ownership,scheduling_metadata,last_error,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		"ephemeral", operations.StateDraining, 7, 1, ownership, metadata, "", now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO instances(id,state,version,drain_phase,ownership,scheduling_metadata,last_error,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		"running", operations.StateRunning, 7, 0, ownership, metadata, "", now, now); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []struct{ id, kind, resource, effect, lastError string }{
		{"ephemeral-drain", "deregister", "ephemeral", "deregister:ephemeral", legacyStageDeregisterError},
		{"unrelated-dead", "clone", "ephemeral", "clone:ephemeral", "runner lifecycle failed at clone"},
		{"wrong-effect", "deregister", "ephemeral", "deregister:other", legacyStageDeregisterError},
		{"wrong-state", "deregister", "running", "deregister:running", legacyStageDeregisterError},
		{"wrong-stage", "deregister", "ephemeral", "deregister:ephemeral:other", "runner lifecycle failed at confirm_inactive"},
	} {
		if _, err := store.db.Exec(`INSERT INTO operations(id,idempotency_key,effect_key,kind,resource_id,payload,status,attempts,available_at,lease_owner,lease_until,last_error,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			operation.id, operation.id, operation.effect, operation.kind, operation.resource, `{}`, operations.OperationDead, 5, now, "", 0, operation.lastError, now, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.Exec(`DELETE FROM schema_migrations WHERE version>=6`); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var status string
	var attempts int
	if err := store.db.QueryRow(`SELECT status,attempts FROM operations WHERE id='ephemeral-drain'`).Scan(&status, &attempts); err != nil || status != string(operations.OperationPending) || attempts != 0 {
		t.Fatalf("ephemeral drain was not revived once: status=%q attempts=%d err=%v", status, attempts, err)
	}
	for _, id := range []string{"unrelated-dead", "wrong-effect", "wrong-state", "wrong-stage"} {
		if err := store.db.QueryRow(`SELECT status,attempts FROM operations WHERE id=?`, id).Scan(&status, &attempts); err != nil || status != string(operations.OperationDead) || attempts != 5 {
			t.Fatalf("unrelated dead letter %s changed: status=%q attempts=%d err=%v", id, status, attempts, err)
		}
	}
	if _, err := store.db.Exec(`UPDATE operations SET status=?,attempts=5,last_error=? WHERE id='ephemeral-drain'`, operations.OperationDead, legacyStageDeregisterError); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT status,attempts FROM operations WHERE id='ephemeral-drain'`).Scan(&status, &attempts); err != nil || status != string(operations.OperationDead) || attempts != 5 {
		t.Fatalf("migration 6 was not one-shot: status=%q attempts=%d err=%v", status, attempts, err)
	}
}

func TestMigrationSevenRequeuesV071DeregisterDeadLetterExactlyOnce(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	now := time.Unix(468, 0).UTC().UnixNano()
	ownership := `{"controller_id":"controller","resource_id":"job","operation_id":"spawn"}`
	seedOwnedDeadDrain(t, store, "v071-ephemeral", "v071-drain", ownership, 7, 5, now)
	if _, err := store.db.Exec(`DELETE FROM schema_migrations WHERE version>=7`); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	assertDrainMigrationRequeuedOnce(t, store, "v071-drain", 5, 9)
}

func TestMigrationEightRequeuesExhaustedOwnedDrainAfterReplacementJob(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	now := time.Unix(472, 0).UTC().UnixNano()
	ownership := `{"controller_id":"controller","resource_id":"canceled-request","operation_id":"spawn"}`
	seedOwnedDeadDrain(t, store, "replacement-job-runner", "replacement-job-drain", ownership, 6, 12, now)
	if _, err := store.db.Exec(`DELETE FROM schema_migrations WHERE version=8`); err != nil {
		t.Fatal(err)
	}

	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	assertDrainMigrationRequeuedOnce(t, store, "replacement-job-drain", 12, 9)
}

func seedOwnedDeadDrain(t *testing.T, store *Store, instanceID string, operationID string, ownership string, instanceVersion int, attempts int, now int64) {
	t.Helper()
	metadata := `{"schema_version":1,"repo":"owner/repo","platform":"darwin","profile":"maestro","route":"macos-maestro","resources":{"cpu":4,"memory_mb":7168,"slots":1},"demand":{"repo":"owner/repo","run_id":1,"job_id":2,"attempt":1}}`
	if _, err := store.db.Exec(`INSERT INTO instances(id,state,version,drain_phase,ownership,scheduling_metadata,last_error,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		instanceID, operations.StateDraining, instanceVersion, 1, ownership, metadata, "", now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO operations(id,idempotency_key,effect_key,kind,resource_id,payload,status,attempts,available_at,lease_owner,lease_until,last_error,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		operationID, operationID, "deregister:"+instanceID, lifecycle.OperationDrain,
		instanceID, `{}`, operations.OperationDead, attempts, now, "", 0, legacyStageDeregisterError, now, now); err != nil {
		t.Fatal(err)
	}
}

func assertDrainMigrationRequeuedOnce(t *testing.T, store *Store, operationID string, attemptsBefore int, expectedVersion int) {
	t.Helper()
	ctx := context.Background()
	var status string
	var attempts int
	var version int
	if err := store.db.QueryRow(`SELECT status,attempts FROM operations WHERE id=?`, operationID).Scan(&status, &attempts); err != nil || status != string(operations.OperationPending) || attempts != 0 {
		t.Fatalf("drain %s was not requeued: status=%q attempts=%d err=%v", operationID, status, attempts, err)
	}
	if err := store.db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != expectedVersion {
		t.Fatalf("migration version=%d err=%v", version, err)
	}
	if _, err := store.db.Exec(`UPDATE operations SET status=?,attempts=?,last_error=? WHERE id=?`, operations.OperationDead, attemptsBefore, legacyStageDeregisterError, operationID); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT status,attempts FROM operations WHERE id=?`, operationID).Scan(&status, &attempts); err != nil || status != string(operations.OperationDead) || attempts != attemptsBefore {
		t.Fatalf("migration was not one-shot for %s: status=%q attempts=%d err=%v", operationID, status, attempts, err)
	}
}

func TestRenewOperationLeaseFencing(t *testing.T) {
	store := testStore(t)
	now := time.Unix(470, 0).UTC()
	_, _ = seed(t, store, now)
	if err := store.RenewOperation(context.Background(), "", "worker", now, time.Minute); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("invalid renewal: %v", err)
	}
	claimed, err := store.Claim(context.Background(), "worker", 1, now, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatal(err)
	}
	if err := store.RenewOperation(context.Background(), claimed[0].ID, "worker", now.Add(time.Second), time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := store.RenewOperation(context.Background(), claimed[0].ID, "other", now.Add(2*time.Second), time.Minute); !errors.Is(err, operations.ErrLeaseLost) {
		t.Fatalf("owner fencing failed: %v", err)
	}
}

func TestOpenRejectsUnsafePaths(t *testing.T) {
	if _, err := Open(context.Background(), ""); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("empty path: %v", err)
	}
	directory := t.TempDir()
	target := filepath.Join(directory, "target.db")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "link.db")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), link); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("symlink accepted: %v", err)
	}
}

func TestValidationConflictAndCorruptionBranches(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Unix(480, 0).UTC()
	if err := store.CreateInstance(ctx, operations.Instance{}); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("invalid instance: %v", err)
	}
	ownership := operations.Ownership{ControllerID: "c", ResourceID: "r", OperationID: "o"}
	if err := store.CreateInstance(ctx, operations.Instance{ID: "default-time", State: operations.StatePlanned, Ownership: ownership}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateInstance(ctx, operations.Instance{ID: "default-time", State: operations.StatePlanned, Ownership: ownership}); err == nil {
		t.Fatal("duplicate instance accepted")
	}
	if _, _, err := store.Transition(ctx, operations.Transition{}); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("invalid transition: %v", err)
	}
	if _, err := store.Claim(ctx, "", 0, now, 0); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("invalid claim: %v", err)
	}
	if _, err := store.Complete(ctx, "missing", "worker", "effect", now); !errors.Is(err, operations.ErrNotFound) {
		t.Fatalf("missing complete: %v", err)
	}
	_, _ = seed(t, store, now)
	claimed, err := store.Claim(ctx, "worker", 1, now, time.Second)
	if err != nil || len(claimed) != 1 {
		t.Fatal(err)
	}
	if _, err := store.Complete(ctx, "clone", "worker", "wrong", now); !errors.Is(err, operations.ErrConflict) {
		t.Fatalf("effect key conflict: %v", err)
	}
	if _, err := store.Complete(ctx, "clone", "other", "clone-vm", now); !errors.Is(err, operations.ErrLeaseLost) {
		t.Fatalf("completion fencing: %v", err)
	}
	if _, err := store.AcquireLease(ctx, "", "", now, 0); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("invalid lease: %v", err)
	}
	lease, err := store.AcquireLease(ctx, "same", "worker", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	lease2, err := store.AcquireLease(ctx, "same", "worker", now.Add(time.Second), time.Minute)
	if err != nil || lease2.Token != lease.Token+1 {
		t.Fatalf("same owner reacquire: %#v %v", lease2, err)
	}
	if _, err := store.RenewLease(ctx, operations.Lease{Name: "wrong", Owner: "worker", Token: 1}, now, time.Minute); !errors.Is(err, operations.ErrLeaseLost) {
		t.Fatalf("renew fencing: %v", err)
	}
	if err := store.PutOwnership(ctx, "", ownership); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("invalid ownership: %v", err)
	}
	if _, err := store.Ownership(ctx, "missing"); !errors.Is(err, operations.ErrNotFound) {
		t.Fatalf("missing ownership: %v", err)
	}
	if _, err := store.db.Exec(`INSERT INTO ownership(resource_name,metadata,updated_at) VALUES('corrupt','{',0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Ownership(ctx, "corrupt"); err == nil {
		t.Fatal("corrupt ownership accepted")
	}
	if _, err := store.db.Exec(`UPDATE instances SET ownership='{' WHERE id='default-time'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Instance(ctx, "default-time"); err == nil {
		t.Fatal("corrupt instance ownership accepted")
	}
}

func TestSchedulingMetadataRestartRoundTripLiveListAndImmutability(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "metadata.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(485, 0).UTC()
	ownership := operations.Ownership{ControllerID: "c", ResourceID: "vm", OperationID: "create"}
	instance := operations.Instance{
		ID: "vm", Repo: "owner/repo", Platform: domain.PlatformLinux, Profile: "large", Route: "tiered",
		Resources: domain.Resources{CPU: 4, MemoryMB: 8192, Slots: 1}, Demand: domain.DemandKey{Repo: "owner/repo", RunID: 7, Attempt: 2, JobID: 9},
		State: operations.StatePlanned, Ownership: ownership, CreatedAt: now,
	}
	if err := store.CreateInstance(ctx, instance); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateInstance(ctx, operations.Instance{ID: "partial", Repo: "owner/repo", State: operations.StatePlanned, Ownership: ownership}); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("partial metadata accepted: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	got, err := store.Instance(ctx, "vm")
	if err != nil || got.Repo != instance.Repo || got.Platform != instance.Platform || got.Profile != instance.Profile || got.Resources != instance.Resources || got.Demand != instance.Demand {
		t.Fatalf("metadata roundtrip: %#v %v", got, err)
	}
	live, err := store.LiveInstances(ctx)
	if err != nil || len(live) != 1 || live[0].ID != "vm" {
		t.Fatalf("live inventory: %#v %v", live, err)
	}
	if _, err := store.db.Exec(`UPDATE instances SET state=? WHERE id='vm'`, operations.StateDeleted); err != nil {
		t.Fatal(err)
	}
	if live, err := store.LiveInstances(ctx); err != nil || len(live) != 0 {
		t.Fatalf("deleted instance listed: %#v %v", live, err)
	}

	planInstance := instance
	planInstance.ID = "planned"
	plan := operations.Plan{ID: "metadata-plan", CreatedAt: now, Scheduler: operations.SchedulerState{Version: 1}, Instances: []operations.InstanceIntent{{ExpectedVersion: -1, Instance: planInstance}}}
	if _, err := store.ApplyPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	mutated := planInstance
	mutated.State, mutated.Version, mutated.Profile = operations.StateCloning, 1, "small"
	mutation := operations.Plan{ID: "metadata-mutation", ExpectedSchedulerVersion: 1, CreatedAt: now.Add(time.Second), Scheduler: operations.SchedulerState{Version: 2}, Instances: []operations.InstanceIntent{{ExpectedVersion: 0, ExpectedState: operations.StatePlanned, Instance: mutated}}}
	if _, err := store.ApplyPlan(ctx, mutation); !errors.Is(err, operations.ErrConflict) {
		t.Fatalf("immutable scheduling metadata mutation accepted: %v", err)
	}
}

func TestApplyPlanUpdateAndFailureBranches(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Unix(490, 0).UTC()
	ownership := operations.Ownership{ControllerID: "c", ResourceID: "vm", OperationID: "plan"}
	first := operations.Plan{ID: "one", CreatedAt: now, Scheduler: operations.SchedulerState{Version: 1}, Instances: []operations.InstanceIntent{{ExpectedVersion: -1, Instance: operations.Instance{ID: "vm", State: operations.StatePlanned, Ownership: ownership}}}}
	if _, err := store.ApplyPlan(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := operations.Plan{ID: "two", ExpectedSchedulerVersion: 1, CreatedAt: now.Add(time.Second), Scheduler: operations.SchedulerState{Version: 2}, Instances: []operations.InstanceIntent{{ExpectedVersion: 0, ExpectedState: operations.StatePlanned, Instance: operations.Instance{ID: "vm", State: operations.StateCloning, Version: 1, Ownership: ownership}}}}
	if applied, err := store.ApplyPlan(ctx, second); err != nil || !applied {
		t.Fatalf("update intent: %v %v", applied, err)
	}
	stale := second
	stale.ID = "three"
	stale.ExpectedSchedulerVersion = 2
	stale.Scheduler.Version = 3
	if _, err := store.ApplyPlan(ctx, stale); !errors.Is(err, operations.ErrConflict) {
		t.Fatalf("stale instance intent: %v", err)
	}
	if _, err := store.ApplyPlan(ctx, operations.Plan{}); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("invalid plan: %v", err)
	}
	badJSON := operations.Plan{ID: "json", ExpectedSchedulerVersion: 2, CreatedAt: now, Scheduler: operations.SchedulerState{Version: 3, Data: json.RawMessage(`{`)}}
	if _, err := store.ApplyPlan(ctx, badJSON); err == nil {
		t.Fatal("digest error ignored")
	}
	duplicateOps := operations.Plan{ID: "duplicates", ExpectedSchedulerVersion: 2, CreatedAt: now.Add(2 * time.Second), Scheduler: operations.SchedulerState{Version: 3}, Operations: []operations.Operation{
		{ID: "same", IdempotencyKey: "one", EffectKey: "one", Kind: "test", ResourceID: "vm", AvailableAt: now},
		{ID: "same", IdempotencyKey: "two", EffectKey: "two", Kind: "test", ResourceID: "vm", AvailableAt: now},
	}}
	if _, err := store.ApplyPlan(ctx, duplicateOps); err == nil {
		t.Fatal("duplicate operation IDs accepted")
	}
}

type scanError struct{ err error }

func (s scanError) Scan(...any) error { return s.err }

func TestScannerErrorsAndClosedDatabaseErrors(t *testing.T) {
	if _, err := scanInstance(scanError{sql.ErrNoRows}); !errors.Is(err, operations.ErrNotFound) {
		t.Fatalf("instance no rows: %v", err)
	}
	if _, err := scanInstance(scanError{errors.New("scan failed")}); err == nil {
		t.Fatal("instance scan error ignored")
	}
	if _, err := scanOperation(scanError{sql.ErrNoRows}); !errors.Is(err, operations.ErrNotFound) {
		t.Fatalf("operation no rows: %v", err)
	}
	if _, err := scanOperation(scanError{errors.New("scan failed")}); err == nil {
		t.Fatal("operation scan error ignored")
	}
	store := testStore(t)
	if err := store.db.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now().UTC()
	ownership := operations.Ownership{ControllerID: "c", ResourceID: "r", OperationID: "o"}
	operation := operations.Operation{ID: "op", IdempotencyKey: "op", EffectKey: "op", Kind: "test", ResourceID: "r", AvailableAt: now}
	plan := operations.Plan{ID: "plan", CreatedAt: now, Scheduler: operations.SchedulerState{Version: 1}}
	checks := []func() error{
		func() error { return store.Migrate(ctx) },
		func() error { _, err := store.SchedulerState(ctx); return err },
		func() error { _, err := store.ApplyPlan(ctx, plan); return err },
		func() error {
			return store.CreateInstance(ctx, operations.Instance{ID: "vm", State: operations.StatePlanned, Ownership: ownership})
		},
		func() error { _, err := store.Instance(ctx, "vm"); return err },
		func() error {
			_, _, err := store.Transition(ctx, operations.Transition{InstanceID: "vm", ExpectedState: operations.StatePlanned, NextState: operations.StateCloning, Operation: operation})
			return err
		},
		func() error { _, err := store.Claim(ctx, "worker", 1, now, time.Minute); return err },
		func() error { return store.RenewOperation(ctx, "op", "worker", now, time.Minute) },
		func() error { _, err := store.Complete(ctx, "op", "worker", "op", now); return err },
		func() error { return store.Retry(ctx, "op", "worker", "failed", now, false) },
		func() error { _, err := store.RecoverExpired(ctx, now); return err },
		func() error { _, err := store.AcquireLease(ctx, "fleet", "worker", now, time.Minute); return err },
		func() error {
			_, err := store.RenewLease(ctx, operations.Lease{Name: "fleet", Owner: "worker", Token: 1}, now, time.Minute)
			return err
		},
		func() error {
			return store.ReleaseLease(ctx, operations.Lease{Name: "fleet", Owner: "worker", Token: 1})
		},
		func() error { return store.PutOwnership(ctx, "vm", ownership) },
		func() error { _, err := store.Ownership(ctx, "vm"); return err },
	}
	for index, check := range checks {
		if err := check(); err == nil {
			t.Fatalf("closed database check %d unexpectedly succeeded", index)
		}
	}
}

func TestMigrationFaultInjectionRollsBackEveryStage(t *testing.T) {
	points := []string{
		"migrate.pragma", "migrate.begin", "migrate.table", "migrate.version", "migrate.v1", "migrate.v1.record",
		"migrate.v2", "migrate.v2.record", "migrate.v3", "migrate.v3.record", "migrate.v4", "migrate.v4.record",
		"migrate.v5", "migrate.v5.record", "migrate.v6", "migrate.v6.record", "migrate.commit", "migrate.quick",
	}
	for _, point := range points {
		t.Run(point, func(t *testing.T) {
			db, err := sql.Open("sqlite", "file:"+strings.ReplaceAll(point, ".", "-")+"?mode=memory&cache=shared")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			store := &Store{db: db, injectFault: func(candidate string) error {
				if candidate == point {
					return errors.New("injected " + point)
				}
				return nil
			}}
			if err := store.Migrate(context.Background()); err == nil {
				t.Fatal("injected migration failure ignored")
			}
		})
	}
}

func injectStoreFault(store *Store, point string) {
	store.injectFault = func(candidate string) error {
		if candidate == point {
			return errors.New("injected " + point)
		}
		return nil
	}
}

func TestApplyPlanFaultInjection(t *testing.T) {
	now := time.Unix(800, 0).UTC()
	owner := operations.Ownership{ControllerID: "c", ResourceID: "vm", OperationID: "plan"}
	op := func(id string, dependencies ...string) operations.Operation {
		return operations.Operation{ID: id, IdempotencyKey: id, EffectKey: id, Kind: "test", ResourceID: "vm", AvailableAt: now, DependsOn: dependencies}
	}
	for _, point := range []string{"apply.begin", "apply.load", "apply.version", "apply.scheduler", "apply.record", "apply.commit"} {
		t.Run(point, func(t *testing.T) {
			store := testStore(t)
			injectStoreFault(store, point)
			plan := operations.Plan{ID: point, CreatedAt: now, Scheduler: operations.SchedulerState{Version: 1}}
			if applied, err := store.ApplyPlan(context.Background(), plan); applied || err == nil {
				t.Fatalf("fault ignored: %v %v", applied, err)
			}
		})
	}
	t.Run("insert", func(t *testing.T) {
		store := testStore(t)
		injectStoreFault(store, "apply.instance.insert")
		plan := operations.Plan{ID: "insert", CreatedAt: now, Scheduler: operations.SchedulerState{Version: 1}, Instances: []operations.InstanceIntent{{ExpectedVersion: -1, Instance: operations.Instance{ID: "vm", State: operations.StatePlanned, Ownership: owner}}}}
		if _, err := store.ApplyPlan(context.Background(), plan); err == nil {
			t.Fatal("instance insert fault ignored")
		}
	})
	t.Run("update", func(t *testing.T) {
		store := testStore(t)
		first := operations.Plan{ID: "first", CreatedAt: now, Scheduler: operations.SchedulerState{Version: 1}, Instances: []operations.InstanceIntent{{ExpectedVersion: -1, Instance: operations.Instance{ID: "vm", State: operations.StatePlanned, Ownership: owner}}}}
		if _, err := store.ApplyPlan(context.Background(), first); err != nil {
			t.Fatal(err)
		}
		injectStoreFault(store, "apply.instance.update")
		second := operations.Plan{ID: "second", ExpectedSchedulerVersion: 1, CreatedAt: now.Add(time.Second), Scheduler: operations.SchedulerState{Version: 2}, Instances: []operations.InstanceIntent{{ExpectedVersion: 0, ExpectedState: operations.StatePlanned, Instance: operations.Instance{ID: "vm", State: operations.StateCloning, Version: 1, Ownership: owner}}}}
		if _, err := store.ApplyPlan(context.Background(), second); err == nil {
			t.Fatal("instance update fault ignored")
		}
	})
	for _, point := range []string{"apply.dependency.check", "apply.dependency.record"} {
		t.Run(point, func(t *testing.T) {
			store := testStore(t)
			injectStoreFault(store, point)
			plan := operations.Plan{ID: point, CreatedAt: now, Scheduler: operations.SchedulerState{Version: 1}, Operations: []operations.Operation{op("root"), op("child", "root")}}
			if _, err := store.ApplyPlan(context.Background(), plan); err == nil {
				t.Fatal("dependency fault ignored")
			}
		})
	}
}

func TestTransitionClaimCompleteAndLeaseFaultInjection(t *testing.T) {
	now := time.Unix(810, 0).UTC()
	for _, point := range []string{"transition.begin", "transition.operation.load", "transition.instance", "transition.operation", "transition.dependency.check", "transition.dependency.record", "transition.history", "transition.result", "transition.commit"} {
		t.Run(point, func(t *testing.T) {
			store := testStore(t)
			_, _ = seed(t, store, now)
			owner := operations.Ownership{ControllerID: "c", ResourceID: "second", OperationID: "create"}
			if err := store.CreateInstance(context.Background(), operations.Instance{ID: "second", State: operations.StatePlanned, Ownership: owner, CreatedAt: now}); err != nil {
				t.Fatal(err)
			}
			injectStoreFault(store, point)
			op := operations.Operation{ID: "dependent", IdempotencyKey: "dependent", EffectKey: "dependent", Kind: "test", ResourceID: "second", AvailableAt: now, DependsOn: []string{"clone"}}
			_, _, err := store.Transition(context.Background(), operations.Transition{InstanceID: "second", ExpectedState: operations.StatePlanned, NextState: operations.StateCloning, Operation: op})
			if err == nil {
				t.Fatal("transition fault ignored")
			}
		})
	}
	for _, point := range []string{"claim.begin", "claim.propagate", "claim.query", "claim.update", "claim.load", "claim.commit"} {
		t.Run(point, func(t *testing.T) {
			store := testStore(t)
			_, _ = seed(t, store, now)
			injectStoreFault(store, point)
			if _, err := store.Claim(context.Background(), "worker", 1, now, time.Minute); err == nil {
				t.Fatal("claim fault ignored")
			}
		})
	}
	for _, point := range []string{"complete.begin", "complete.load", "complete.effect", "complete.update", "complete.commit"} {
		t.Run(point, func(t *testing.T) {
			store := testStore(t)
			_, _ = seed(t, store, now)
			if _, err := store.Claim(context.Background(), "worker", 1, now, time.Minute); err != nil {
				t.Fatal(err)
			}
			injectStoreFault(store, point)
			if _, err := store.Complete(context.Background(), "clone", "worker", "clone-vm", now); err == nil {
				t.Fatal("complete fault ignored")
			}
		})
	}
	for _, point := range []string{"lease.begin", "lease.load", "lease.insert", "lease.commit"} {
		t.Run(point, func(t *testing.T) {
			store := testStore(t)
			injectStoreFault(store, point)
			if _, err := store.AcquireLease(context.Background(), "fleet", "worker", now, time.Minute); err == nil {
				t.Fatal("lease fault ignored")
			}
		})
	}
	t.Run("lease.update", func(t *testing.T) {
		store := testStore(t)
		if _, err := store.AcquireLease(context.Background(), "fleet", "worker", now, time.Minute); err != nil {
			t.Fatal(err)
		}
		injectStoreFault(store, "lease.update")
		if _, err := store.AcquireLease(context.Background(), "fleet", "worker", now.Add(time.Second), time.Minute); err == nil {
			t.Fatal("lease update fault ignored")
		}
	})
}

func TestRemainingMetadataInventoryConflictAndTriggerFailures(t *testing.T) {
	if toNanos(time.Time{}) != 0 {
		t.Fatal("zero time did not encode as zero")
	}
	if _, err := encodeSchedulingMetadata(operations.Instance{Repo: "partial"}); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("invalid metadata encoded: %v", err)
	}
	var instance operations.Instance
	for _, encoded := range [][]byte{[]byte(`{`), []byte(`{"schema_version":2}`), []byte(`{"schema_version":1,"repo":"partial"}`)} {
		if err := decodeSchedulingMetadata(encoded, &instance); err == nil {
			t.Fatalf("bad metadata decoded: %s", encoded)
		}
	}
	store := testStore(t)
	ctx := context.Background()
	now := time.Unix(820, 0).UTC()
	_, queued := seed(t, store, now)
	// The same idempotency key cannot bless a transition to a different state.
	if _, _, err := store.Transition(ctx, operations.Transition{InstanceID: "vm", ExpectedState: operations.StatePlanned, ExpectedVersion: 0, NextState: operations.StateFailed, Operation: queued}); !errors.Is(err, operations.ErrConflict) {
		t.Fatalf("idempotent transition conflict ignored: %v", err)
	}
	op := operations.Operation{ID: "stale", IdempotencyKey: "stale", EffectKey: "stale", Kind: "test", ResourceID: "vm", AvailableAt: now}
	if _, _, err := store.Transition(ctx, operations.Transition{InstanceID: "vm", ExpectedState: operations.StatePlanned, ExpectedVersion: 0, NextState: operations.StateCloning, Operation: op}); !errors.Is(err, operations.ErrConflict) {
		t.Fatalf("stale transition accepted: %v", err)
	}
	if _, err := store.db.Exec(`UPDATE instances SET scheduling_metadata='{' WHERE id='vm'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LiveInstances(ctx); err == nil {
		t.Fatal("corrupt live inventory accepted")
	}
	if err := store.db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LiveInstances(ctx); err == nil {
		t.Fatal("closed inventory database ignored")
	}

	store = testStore(t)
	if _, err := store.db.Exec(`CREATE TRIGGER ignore_scheduler BEFORE UPDATE ON scheduler_state BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	plan := operations.Plan{ID: "ignored", CreatedAt: now, Scheduler: operations.SchedulerState{Version: 1}}
	if _, err := store.ApplyPlan(ctx, plan); !errors.Is(err, operations.ErrConflict) {
		t.Fatalf("ignored scheduler update accepted: %v", err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER fail_ownership BEFORE INSERT ON ownership BEGIN SELECT RAISE(FAIL,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	owner := operations.Ownership{ControllerID: "c", ResourceID: "r", OperationID: "o"}
	if err := store.PutOwnership(ctx, "triggered", owner); err == nil {
		t.Fatal("ownership insert failure ignored")
	}
}

func TestOpenFilesystemFailureBranches(t *testing.T) {
	parentFile := filepath.Join(t.TempDir(), "parent")
	if err := os.WriteFile(parentFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), filepath.Join(parentFile, "fleet.db")); err == nil {
		t.Fatal("database under non-directory parent opened")
	}
	if _, err := Open(context.Background(), "file:/definitely/missing/directory/fleet.db?mode=rw"); err == nil {
		t.Fatal("invalid file DSN opened")
	}
}

type injectedRows struct {
	id       string
	next     bool
	scanErr  error
	closeErr error
	rowsErr  error
}

func (r *injectedRows) Next() bool {
	if !r.next {
		return false
	}
	r.next = false
	return true
}

func (r *injectedRows) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	*(dest[0].(*string)) = r.id
	return nil
}

func (r *injectedRows) Close() error { return r.closeErr }
func (r *injectedRows) Err() error   { return r.rowsErr }

func TestInjectedRowIterationFailuresAndLostClaimRace(t *testing.T) {
	store := testStore(t)
	store.injectRows = func(point string) rowsScanner {
		if point == "inbox.active.query" {
			return &injectedRows{rowsErr: errors.New("iterate")}
		}
		return nil
	}
	if _, err := store.ActiveDemands(context.Background(), 1); err == nil {
		t.Fatal("active iteration error ignored")
	}
	store.injectRows = func(point string) rowsScanner {
		if point == "instances.live.query" {
			return &injectedRows{rowsErr: errors.New("iterate")}
		}
		return nil
	}
	if _, err := store.LiveInstances(context.Background()); err == nil {
		t.Fatal("instance iteration error ignored")
	}
	now := time.Unix(830, 0).UTC()
	for name, rows := range map[string]*injectedRows{
		"scan":  {next: true, scanErr: errors.New("scan")},
		"close": {closeErr: errors.New("close")},
		"race":  {next: true, id: "already-claimed"},
	} {
		t.Run(name, func(t *testing.T) {
			store := testStore(t)
			store.injectRows = func(point string) rowsScanner {
				if point == "claim.query" {
					return rows
				}
				return nil
			}
			claimed, err := store.Claim(context.Background(), "worker", 1, now, time.Minute)
			if name == "race" {
				if err != nil || len(claimed) != 0 {
					t.Fatalf("lost claim race: %#v %v", claimed, err)
				}
			} else if err == nil {
				t.Fatal("row failure ignored")
			}
		})
	}
}

func TestIdempotentTransitionResultReadFailure(t *testing.T) {
	store := testStore(t)
	now := time.Unix(840, 0).UTC()
	_, operation := seed(t, store, now)
	injectStoreFault(store, "transition.idempotent.instance")
	if _, _, err := store.Transition(context.Background(), operations.Transition{InstanceID: "vm", ExpectedState: operations.StatePlanned, ExpectedVersion: 0, NextState: operations.StateCloning, Operation: operation}); err == nil {
		t.Fatal("idempotent result read failure ignored")
	}
}
