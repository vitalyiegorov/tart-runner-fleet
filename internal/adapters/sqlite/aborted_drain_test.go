package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/executor"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/lifecycle"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/reconcile"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

// Issue #447, ubuntu-vitalii on v0.1.793, 2026-10-28. An event drain of
// trf-linux-4x8-3cdd90db7494aebf aborted on fresh runner_busy evidence at
// 11:11:11.074 and the instance returned to running. One millisecond later the
// operations loop failed, so the drain's own completion never landed: the abort
// had committed the rollback in one transaction and left the operation's
// completion to a second one. The operation went back to pending and re-ran
// against a running instance, which the executor can only call a `state
// conflict`, every 30 seconds. At 11:12:07 the job finished and the completion
// drain could not be enqueued either, because it reuses the identity
// `event-drain-<id>` the stale row still held (`demand_commit_conflict`).
const abortedDrainInstance = "trf-linux-4x8-3cdd90db7494aebf"

// abortWorld is the runner and its guest. busy is the runner-scoped evidence
// the drain re-verifies (ADR 0074); everything else succeeds once asked.
type abortWorld struct {
	now     time.Time
	busy    bool
	deleted bool
}

func (w *abortWorld) Create(context.Context, executor.InstanceSpec) error { panic("no create") }
func (w *abortWorld) Start(context.Context, string, operations.Ownership) error {
	panic("no start")
}
func (w *abortWorld) Stop(context.Context, string, operations.Ownership) error      { return nil }
func (w *abortWorld) Terminate(context.Context, string, operations.Ownership) error { return nil }
func (w *abortWorld) Destroy(context.Context, string, operations.Ownership) error   { return nil }
func (w *abortWorld) Delete(context.Context, string, operations.Ownership) error {
	w.deleted = true
	return nil
}
func (w *abortWorld) Power(context.Context, string) (domain.InstancePower, error) {
	return domain.InstancePowerStopped, nil
}
func (w *abortWorld) SafeToDeregister(context.Context, operations.Instance) (bool, error) {
	return true, nil
}
func (w *abortWorld) Deregister(context.Context, operations.Instance) error { return nil }
func (w *abortWorld) ConfirmDeletion(context.Context, string) (operations.DeletionConfirmation, error) {
	return operations.DeletionConfirmation{Fresh: true, RunnerInactive: true, JobsInactive: true, ObservedAt: w.now}, nil
}
func (w *abortWorld) RunnerRegistered(context.Context, operations.Instance) (bool, error) {
	return true, nil
}
func (w *abortWorld) RunnerBusy(context.Context, operations.Instance) (bool, error) {
	return w.busy, nil
}
func (w *abortWorld) StartUnattributed(context.Context, operations.Instance) (bool, error) {
	return false, nil
}

// lostCompletion is the 11:11:11.075 `component loop failure`: the worker's
// completion of the operation it just ran does not land. Anything the executor
// committed before it stays committed.
type lostCompletion struct {
	*Store
	lose bool
}

func (s *lostCompletion) Complete(ctx context.Context, id, owner, effectKey string, now time.Time) (bool, error) {
	if s.lose {
		s.lose = false
		return false, errors.New("injected: the completion after the abort was lost")
	}
	return s.Store.Complete(ctx, id, owner, effectKey, now)
}

type abortFixture struct {
	store  *Store
	world  *abortWorld
	worker operations.Worker
	lossy  *lostCompletion
}

func newAbortFixture(t *testing.T) *abortFixture {
	t.Helper()
	world := &abortWorld{now: time.Date(2026, 10, 28, 11, 9, 57, 0, time.UTC)}
	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "fleet.db"), WithClock(func() time.Time { return world.now }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	instance := runnerDemandInstance(abortedDrainInstance, 91)
	if err := store.CreateInstance(context.Background(), instance); err != nil {
		t.Fatal(err)
	}
	lossy := &lostCompletion{Store: store}
	now := func() time.Time { return world.now }
	drain := lifecycle.DrainExecutor{State: store, VM: world, Control: world, Now: now}
	worker := operations.Worker{Store: lossy, Owner: "controller", MaxConcurrent: 1, Now: now,
		Executors:   map[string]operations.Executor{lifecycle.OperationDrain: drain},
		RetryByKind: map[string]operations.RetryPolicy{lifecycle.OperationDrain: operations.DurableCleanupRetryPolicy(30 * time.Second)}}
	return &abortFixture{store: store, world: world, worker: worker, lossy: lossy}
}

func (f *abortFixture) instance(t *testing.T) operations.Instance {
	t.Helper()
	instance, err := f.store.Instance(context.Background(), abortedDrainInstance)
	if err != nil {
		t.Fatal(err)
	}
	return instance
}

// complete is the demand projection of JobCompleted for this runner: the
// message path and #443's re-derivation sweep both reach it.
func (f *abortFixture) complete(t *testing.T) error {
	t.Helper()
	return f.store.projectDemandRank(context.Background(), f.instance(t), operations.DemandJobCompleted)
}

// run lets the clock pass every retry and lease the worker could be waiting on,
// then runs one worker pass.
func (f *abortFixture) run(t *testing.T) {
	t.Helper()
	f.world.now = f.world.now.Add(2 * time.Minute)
	if _, err := f.store.RecoverExpired(context.Background(), f.world.now); err != nil {
		t.Fatal(err)
	}
	_, _ = f.worker.RunOnce(context.Background())
}

type drainRow struct {
	id, status, lastError string
	attempts              int
}

func (f *abortFixture) drains(t *testing.T) []drainRow {
	t.Helper()
	rows, err := f.store.db.Query(`SELECT id,status,last_error,attempts FROM operations WHERE kind=? AND resource_id=? ORDER BY created_at,id`,
		lifecycle.OperationDrain, abortedDrainInstance)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var drains []drainRow
	for rows.Next() {
		var row drainRow
		if err := rows.Scan(&row.id, &row.status, &row.lastError, &row.attempts); err != nil {
			t.Fatal(err)
		}
		drains = append(drains, row)
	}
	return drains
}

func live(drains []drainRow) []drainRow {
	var out []drainRow
	for _, row := range drains {
		if row.status == string(operations.OperationPending) || row.status == string(operations.OperationClaimed) {
			out = append(out, row)
		}
	}
	return out
}

// abortTheEventDrain is the incident up to 11:11:11: the event drain is enqueued
// and runs while the runner is busy. lose drops the worker's completion after
// it, as production did.
func (f *abortFixture) abortTheEventDrain(t *testing.T, lose bool) {
	t.Helper()
	if err := f.complete(t); err != nil {
		t.Fatalf("enqueue the event drain: %v", err)
	}
	f.world.busy, f.lossy.lose = true, lose
	f.run(t)
	if got := f.instance(t); got.State != operations.StateRunning {
		t.Fatalf("a busy runner must abort its drain back to running, got %s", got.State)
	}
	f.world.busy = false
}

// TestAbortedEventDrainIsTerminalAndTheCompletionDrainRuns is the red replay of
// #447: event drain, runner_busy abort with its completion lost, then
// JobCompleted. The abort must leave its drain terminal by itself, so nothing
// retries a state conflict and the completion drain is enqueued and runs.
func TestAbortedEventDrainIsTerminalAndTheCompletionDrainRuns(t *testing.T) {
	f := newAbortFixture(t)
	f.abortTheEventDrain(t, true)

	if pending := live(f.drains(t)); len(pending) != 0 {
		t.Fatalf("the aborted drain must be terminal in the abort's own transaction, still live: %#v", pending)
	}
	for range 3 {
		f.run(t)
	}
	for _, row := range f.drains(t) {
		if row.lastError == operations.ErrConflict.Error() {
			t.Fatalf("drain %s retried a state conflict against a running instance: %#v", row.id, row)
		}
	}

	// 11:12:06: the runner's job completes.
	if err := f.complete(t); err != nil {
		t.Fatalf("the completion drain must not be blocked by the aborted one: %v", err)
	}
	got := f.instance(t)
	if got.State != operations.StateDraining || got.DrainPhase != operations.DrainPhaseEvent {
		t.Fatalf("completion must drain the instance, got %s phase %d", got.State, got.DrainPhase)
	}
	if pending := live(f.drains(t)); len(pending) != 1 {
		t.Fatalf("exactly one live drain must exist, got %#v", pending)
	}
	// A redelivered JobCompleted is idempotent: still exactly one live drain.
	if err := f.complete(t); err != nil {
		t.Fatalf("redelivered completion: %v", err)
	}
	if pending := live(f.drains(t)); len(pending) != 1 {
		t.Fatalf("a redelivered completion must not enqueue a second drain, got %#v", pending)
	}
	f.run(t)
	if got := f.instance(t); got.State != operations.StateDeleted || !f.world.deleted {
		t.Fatalf("the completion drain must reclaim the instance, got %s deleted=%v", got.State, f.world.deleted)
	}
	if pending := live(f.drains(t)); len(pending) != 0 {
		t.Fatalf("no drain may stay live after the reclaim, got %#v", pending)
	}
}

// TestStrandedDrainOfARunningInstanceResolves is the row an upgraded node
// already holds: a drain left pending against an instance a pre-fix abort
// returned to running. It must resolve on its next attempt instead of retrying
// a state conflict forever, and must then not block the completion drain.
func TestStrandedDrainOfARunningInstanceResolves(t *testing.T) {
	f := newAbortFixture(t)
	if err := f.complete(t); err != nil {
		t.Fatal(err)
	}
	// The pre-fix abort: the rollback alone, with the drain left behind.
	draining := f.instance(t)
	if _, err := f.store.Advance(context.Background(), lifecycle.StateChange{InstanceID: draining.ID,
		ExpectedState: draining.State, ExpectedVersion: draining.Version, NextState: operations.StateRunning}); err != nil {
		t.Fatal(err)
	}
	f.run(t)

	drains := f.drains(t)
	if len(drains) != 1 || drains[0].status != string(operations.OperationCompleted) {
		t.Fatalf("a drain of a running instance must resolve, not retry: %#v", drains)
	}
	if got := f.instance(t); got.State != operations.StateRunning {
		t.Fatalf("resolving a stranded drain must not move the instance, got %s", got.State)
	}
	if err := f.complete(t); err != nil {
		t.Fatalf("the completion drain must be enqueued once the stranded one resolved: %v", err)
	}
	if pending := live(f.drains(t)); len(pending) != 1 || f.instance(t).State != operations.StateDraining {
		t.Fatalf("completion must drain with exactly one live operation, got %#v", pending)
	}
}

// TestCompletedRunnerSweepReDrainsAfterAnAbort is #443's path without #443: its
// re-derivation sweep projects JobCompleted through projectDemandRank onto an
// online-idle, assigned or running instance and treats ErrConflict as "skip this
// tick". An aborted drain's identity must not turn that into "skip forever".
//
// The completion is kept here: even an abort whose operation completed cleanly
// left its identity behind, and the next event drain collided with it.
func TestCompletedRunnerSweepReDrainsAfterAnAbort(t *testing.T) {
	for _, lose := range []bool{false, true} {
		f := newAbortFixture(t)
		f.abortTheEventDrain(t, lose)
		for attempt := range 3 {
			err := f.complete(t)
			if errors.Is(err, operations.ErrConflict) {
				t.Fatalf("lost completion %v: sweep attempt %d skipped by the aborted drain's identity", lose, attempt)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		if pending := live(f.drains(t)); len(pending) != 1 || f.instance(t).State != operations.StateDraining {
			t.Fatalf("lost completion %v: sweep must drain with exactly one live operation, got %#v", lose, pending)
		}
	}
}

// TestRepeatedAbortsEachSettleTheirOwnDrain keeps identities distinct across
// more than one abort of the same instance, so the generation is not a one-off.
func TestRepeatedAbortsEachSettleTheirOwnDrain(t *testing.T) {
	f := newAbortFixture(t)
	f.abortTheEventDrain(t, false)
	f.abortTheEventDrain(t, true)
	drains := f.drains(t)
	if len(drains) != 2 || drains[0].id == drains[1].id || len(live(drains)) != 0 {
		t.Fatalf("each abort must settle its own drain: %#v", drains)
	}
	for _, row := range drains {
		if row.status != string(operations.OperationCompleted) {
			t.Fatalf("aborted drain %s is %s", row.id, row.status)
		}
	}
	if drains[0].id != "event-drain-"+abortedDrainInstance {
		t.Fatalf("a first event drain keeps its identity byte-for-byte, got %q", drains[0].id)
	}
}

// TestPlainEventDrainIsUnchanged pins the path no abort touches: one identity,
// one operation, one reclaim.
func TestPlainEventDrainIsUnchanged(t *testing.T) {
	f := newAbortFixture(t)
	if err := f.complete(t); err != nil {
		t.Fatal(err)
	}
	f.run(t)
	drains := f.drains(t)
	if len(drains) != 1 || drains[0].id != "event-drain-"+abortedDrainInstance || drains[0].status != string(operations.OperationCompleted) {
		t.Fatalf("plain drain rows = %#v", drains)
	}
	if got := f.instance(t); got.State != operations.StateDeleted || !f.world.deleted {
		t.Fatalf("plain drain must reclaim, got %s", got.State)
	}
}

// TestLingeringBackstopAfterAnAbortIsTheOnlyLiveDrain answers the effect-key
// question of #447. A recovery drain never collides with the event drain: its
// identity is the scheduler's content address, and the shared effect key
// `deregister:<id>` is not unique on operations (operation_effects is written
// with INSERT OR IGNORE at completion). So before the fix the backstop was
// admitted BESIDE the stranded event drain: two live drains of one runner, and
// the stranded one would then run the backstop's teardown a second time. After
// the fix the aborted drain is terminal and the backstop is the only one.
func TestLingeringBackstopAfterAnAbortIsTheOnlyLiveDrain(t *testing.T) {
	f := newAbortFixture(t)
	f.abortTheEventDrain(t, true)

	controller := reconcile.Controller{Store: f.store, ControllerID: "controller", Mode: reconcile.Authority}
	drain := scheduler.Operation{ID: "drain-lingering-runner", Kind: scheduler.OperationDrain, Instance: abortedDrainInstance,
		Profile: "maestro", Route: "macos-maestro", Recovery: true, LingeringRunner: true}
	plan := scheduler.Plan{ID: "lingering", Status: scheduler.PlanReady, Operations: []scheduler.Operation{drain}}
	if applied, err := controller.Commit(context.Background(), plan, "", f.world.now); err != nil || !applied {
		t.Fatalf("lingering backstop = %v, %v", applied, err)
	}
	if got := f.instance(t); got.State != operations.StateDraining || got.DrainPhase != operations.DrainPhaseLingeringRunner {
		t.Fatalf("backstop must drain the instance, got %s phase %d", got.State, got.DrainPhase)
	}
	pending := live(f.drains(t))
	if len(pending) != 1 || pending[0].id == "event-drain-"+abortedDrainInstance {
		t.Fatalf("the backstop must be the one live drain, got %#v", pending)
	}
	f.run(t)
	if got := f.instance(t); got.State != operations.StateDeleted {
		t.Fatalf("backstop must reclaim, got %s", got.State)
	}
}

// TestAdvanceSettlesAClaimedDrainAtomically pins the store half: the rollback
// and the settled drain land together or not at all, the settled drain records
// no effect, and a change that names a drain without an outcome is refused.
func TestAdvanceSettlesAClaimedDrainAtomically(t *testing.T) {
	ctx := context.Background()
	f := newAbortFixture(t)
	if err := f.complete(t); err != nil {
		t.Fatal(err)
	}
	claimed, err := f.store.Claim(ctx, "controller", 1, f.world.now.Add(time.Second), time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim drain: %#v %v", claimed, err)
	}
	drain := claimed[0]
	draining := f.instance(t)
	change := func(settles operations.Operation, outcome string) lifecycle.StateChange {
		return lifecycle.StateChange{InstanceID: draining.ID, ExpectedState: draining.State, ExpectedVersion: draining.Version,
			NextState: operations.StateRunning, Settles: settles, Outcome: outcome}
	}
	for name, invalid := range map[string]lifecycle.StateChange{
		"settles without an outcome": change(drain, ""),
		"outcome without a settle":   change(operations.Operation{}, "aborted:runner_busy"),
		"unbounded outcome":          change(drain, "aborted:"+string(make([]byte, 64))),
	} {
		if _, err := f.store.Advance(ctx, invalid); !errors.Is(err, operations.ErrInvalid) {
			t.Fatalf("%s: error = %v", name, err)
		}
	}
	stolen := drain
	stolen.LeaseOwner = "another-worker"
	if _, err := f.store.Advance(ctx, change(stolen, "aborted:runner_busy")); !errors.Is(err, operations.ErrLeaseLost) {
		t.Fatalf("a drain the caller no longer holds must refuse the whole abort, got %v", err)
	}
	if got := f.instance(t); got.State != operations.StateDraining || got.Version != draining.Version {
		t.Fatalf("a refused settle must roll the rollback back too, got %s v%d", got.State, got.Version)
	}

	if _, err := f.store.Advance(ctx, change(drain, "aborted:runner_busy")); err != nil {
		t.Fatal(err)
	}
	drains := f.drains(t)
	if len(drains) != 1 || drains[0].status != string(operations.OperationCompleted) || drains[0].lastError != "aborted:runner_busy" {
		t.Fatalf("settled drain = %#v", drains)
	}
	var effects int
	if err := f.store.db.QueryRow(`SELECT COUNT(*) FROM operation_effects WHERE operation_id=?`, drain.ID).Scan(&effects); err != nil || effects != 0 {
		t.Fatalf("an aborted drain deregistered nothing and must record no effect: %d %v", effects, err)
	}
	// The worker's own completion afterwards is a no-op, not a lease error.
	if _, err := f.store.Complete(ctx, drain.ID, "controller", drain.EffectKey, f.world.now); err != nil {
		t.Fatalf("completing a settled drain = %v", err)
	}
}
