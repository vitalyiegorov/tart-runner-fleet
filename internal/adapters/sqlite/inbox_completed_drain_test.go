package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

const completedDrainScaleSet = int64(7195473684968813165)

// completedDrainRunner creates a live runner bound to the request the fleet
// spawned it for, in the state under test.
func completedDrainRunner(t *testing.T, store *Store, id string, request int64, state operations.State) operations.Instance {
	t.Helper()
	instance := operations.Instance{
		ID: id, Repo: "budgie-at/budgie", Platform: domain.PlatformLinux, Profile: "linux-4x8", Route: "trf-linux-amd64-4x8",
		Resources: domain.Resources{CPU: 4, MemoryMB: 8192, Slots: 1},
		Demand:    domain.DemandKey{Repo: "budgie-at/budgie", RunID: 40641346140, Attempt: 1, JobID: request}, State: state,
		Ownership: operations.Ownership{ControllerID: "controller", ResourceID: id, OperationID: "spawn-" + id},
	}
	if err := store.CreateInstance(context.Background(), instance); err != nil {
		t.Fatal(err)
	}
	return instance
}

func completedDrainEvent(kind operations.DemandEventKind, request int64, runner string) operations.DemandEvent {
	return operations.DemandEvent{Kind: kind, RunnerRequestID: request, Owner: "budgie-at", Repository: "budgie",
		WorkflowRunID: 40641346140, JobID: "job-" + string(rune('a'+request%26)), DisplayName: "Effect diagnostics and unit tests",
		RunnerName: runner, Result: "canceled"}
}

func completedDrainState(t *testing.T, store *Store, id string) operations.State {
	t.Helper()
	instance, err := store.Instance(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return instance.State
}

// claimedDrains is every event-drain operation the outbox would hand a worker.
func claimedDrains(t *testing.T, store *Store) []string {
	t.Helper()
	claimed, err := store.Claim(context.Background(), "worker", 10, time.Now().UTC().Add(time.Hour), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(claimed))
	for _, operation := range claimed {
		ids = append(ids, operation.ID)
	}
	return ids
}

// TestIssue442DurableCompletionDrainsWithoutRedelivery is the Omarchy replay of
// 2026-10-28. Message 100003501 carried one event, JobCompleted for request
// 5871030784497826206 on runner trf-linux-4x8-0ab3245aa6dfc6d6. The batch
// committed, which advanced the durable cursor past the message. The projection
// that should have enqueued event-drain-<id> then failed. The ingest loop
// replaced the session, the replacement opened at that cursor, and GitHub never
// delivered the message again. The demand row said JobCompleted for eight
// minutes while the instance stayed running.
//
// The drain has to come from the durable row, not from a redelivery.
func TestIssue442DurableCompletionDrainsWithoutRedelivery(t *testing.T) {
	const runner = "trf-linux-4x8-0ab3245aa6dfc6d6"
	const request = int64(5871030784497826206)
	ctx := context.Background()
	store := testStore(t)
	completedDrainRunner(t, store, runner, request, operations.StateRunning)

	completed := completedDrainEvent(operations.DemandJobCompleted, request, runner)
	if _, err := store.ApplyDemandBatch(ctx, completedDrainScaleSet, 100003501, []operations.DemandEvent{completed}); err != nil {
		t.Fatal(err)
	}
	store.injectFault = func(point string) error {
		if point == "transition.begin" {
			return errors.New("database is locked")
		}
		return nil
	}
	if err := store.ProjectDemandEvent(ctx, completedDrainScaleSet, completed); err == nil {
		t.Fatal("the message-path projection must fail for this replay")
	}
	store.injectFault = nil
	if cursor, err := store.DemandCursor(ctx, completedDrainScaleSet); err != nil || cursor != 100003501 {
		t.Fatalf("cursor = %d, %v; the batch commit must already have passed the message", cursor, err)
	}
	if got := completedDrainState(t, store, runner); got != operations.StateRunning {
		t.Fatalf("state after the failed projection = %s, want running", got)
	}

	drained, err := store.DrainCompletedRunners(ctx, completedDrainScaleSet)
	if err != nil {
		t.Fatal(err)
	}
	if len(drained) != 1 || drained[0] != runner {
		t.Fatalf("drained = %v, want [%s]", drained, runner)
	}
	if got := completedDrainState(t, store, runner); got != operations.StateDraining {
		t.Fatalf("state = %s, want draining", got)
	}
	instance, err := store.Instance(ctx, runner)
	if err != nil || instance.DrainPhase != operations.DrainPhaseEvent {
		t.Fatalf("drain phase = %v, %v; want the event drain", instance.DrainPhase, err)
	}
	if ids := claimedDrains(t, store); len(ids) != 1 || ids[0] != "event-drain-"+runner {
		t.Fatalf("operations = %v, want the one event drain the message path would have enqueued", ids)
	}
}

// Every live runner state the message path drains from is covered, and nothing
// outside the invariant is touched.
func TestDrainCompletedRunnersCoversEveryLiveRunnerState(t *testing.T) {
	for _, test := range []struct {
		state operations.State
		want  bool
	}{
		{state: operations.StateOnlineIdle, want: true},
		{state: operations.StateAssigned, want: true},
		{state: operations.StateRunning, want: true},
		// The message path owns these: a pre-registration completion orders its
		// drain behind provisioning, and a teardown already in flight is the drain.
		{state: operations.StateRegistering},
		{state: operations.StateDraining},
		{state: operations.StateDeregistering},
		{state: operations.StateStopping},
	} {
		t.Run(string(test.state), func(t *testing.T) {
			ctx := context.Background()
			store := testStore(t)
			completedDrainRunner(t, store, "trf-runner", 1, test.state)
			if _, err := store.ApplyDemandBatch(ctx, completedDrainScaleSet, 1,
				[]operations.DemandEvent{completedDrainEvent(operations.DemandJobCompleted, 1, "trf-runner")}); err != nil {
				t.Fatal(err)
			}
			drained, err := store.DrainCompletedRunners(ctx, completedDrainScaleSet)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(drained) == 1; got != test.want {
				t.Fatalf("drained = %v, want drain %v", drained, test.want)
			}
			want := test.state
			if test.want {
				want = operations.StateDraining
			}
			if got := completedDrainState(t, store, "trf-runner"); got != want {
				t.Fatalf("state = %s, want %s", got, want)
			}
		})
	}
}

// The spawning reservation completing is not evidence about this runner. GitHub
// brokered the reservation's job to a different runner and handed this one a
// different job, which is still executing. Draining here would cancel live work.
func TestDrainCompletedRunnersIgnoresACompletionBrokeredToAnotherRunner(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	completedDrainRunner(t, store, "trf-busy", 1, operations.StateRunning)
	events := []operations.DemandEvent{
		completedDrainEvent(operations.DemandJobCompleted, 1, "trf-sibling"),
		completedDrainEvent(operations.DemandJobStarted, 2, "trf-busy"),
	}
	if _, err := store.ApplyDemandBatch(ctx, completedDrainScaleSet, 1, events); err != nil {
		t.Fatal(err)
	}
	drained, err := store.DrainCompletedRunners(ctx, completedDrainScaleSet)
	if err != nil || len(drained) != 0 {
		t.Fatalf("drained = %v, %v; a busy runner must not drain", drained, err)
	}
	if got := completedDrainState(t, store, "trf-busy"); got != operations.StateRunning {
		t.Fatalf("state = %s, want running", got)
	}
	if ids := claimedDrains(t, store); len(ids) != 0 {
		t.Fatalf("operations = %v, want none", ids)
	}
}

// A completion named for this runner never interrupts a job GitHub also says
// is executing on it. RunnerActiveJob is the same evidence the drain executor
// re-checks before deregistering (ADR 0074); asking it here keeps the sweep
// from enqueueing a drain that would only abort back to Running every tick.
func TestDrainCompletedRunnersNeverInterruptsAStartedJob(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	completedDrainRunner(t, store, "trf-runner", 1, operations.StateRunning)
	events := []operations.DemandEvent{
		completedDrainEvent(operations.DemandJobCompleted, 1, "trf-runner"),
		completedDrainEvent(operations.DemandJobStarted, 2, "trf-runner"),
	}
	if _, err := store.ApplyDemandBatch(ctx, completedDrainScaleSet, 1, events); err != nil {
		t.Fatal(err)
	}
	drained, err := store.DrainCompletedRunners(ctx, completedDrainScaleSet)
	if err != nil || len(drained) != 0 {
		t.Fatalf("drained = %v, %v; a started job must not be interrupted", drained, err)
	}
	if got := completedDrainState(t, store, "trf-runner"); got != operations.StateRunning {
		t.Fatalf("state = %s, want running", got)
	}
}

// The normal message path already drained the runner. The sweep is then a
// no-op, and repeating it changes nothing.
func TestDrainCompletedRunnersIsIdempotentWithTheMessagePath(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	completedDrainRunner(t, store, "trf-runner", 1, operations.StateRunning)
	completed := completedDrainEvent(operations.DemandJobCompleted, 1, "trf-runner")
	if _, err := store.ApplyDemandBatch(ctx, completedDrainScaleSet, 1, []operations.DemandEvent{completed}); err != nil {
		t.Fatal(err)
	}
	if err := store.ProjectDemandEvent(ctx, completedDrainScaleSet, completed); err != nil {
		t.Fatal(err)
	}
	if got := completedDrainState(t, store, "trf-runner"); got != operations.StateDraining {
		t.Fatalf("message path state = %s, want draining", got)
	}
	for attempt := range 2 {
		drained, err := store.DrainCompletedRunners(ctx, completedDrainScaleSet)
		if err != nil || len(drained) != 0 {
			t.Fatalf("attempt %d: drained = %v, %v; an in-flight drain is already the answer", attempt, drained, err)
		}
	}
	if ids := claimedDrains(t, store); len(ids) != 1 || ids[0] != "event-drain-trf-runner" {
		t.Fatalf("operations = %v, want the single message-path drain", ids)
	}
}

// Another scale set's completion names a runner this set does not serve.
func TestDrainCompletedRunnersStaysInsideItsScaleSet(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	completedDrainRunner(t, store, "trf-runner", 1, operations.StateRunning)
	if _, err := store.ApplyDemandBatch(ctx, completedDrainScaleSet+1, 1,
		[]operations.DemandEvent{completedDrainEvent(operations.DemandJobCompleted, 1, "trf-runner")}); err != nil {
		t.Fatal(err)
	}
	drained, err := store.DrainCompletedRunners(ctx, completedDrainScaleSet)
	if err != nil || len(drained) != 0 {
		t.Fatalf("drained = %v, %v", drained, err)
	}
}

// Unreadable evidence fails the sweep. It never reads as "nothing to drain"
// and never drains on a guess.
func TestDrainCompletedRunnersFailsClosed(t *testing.T) {
	for _, point := range []string{"inbox.completed.runners", "inbox.runner.active", "inbox.completed.instance", "transition.begin"} {
		t.Run(point, func(t *testing.T) {
			ctx := context.Background()
			store := testStore(t)
			completedDrainRunner(t, store, "trf-runner", 1, operations.StateRunning)
			if _, err := store.ApplyDemandBatch(ctx, completedDrainScaleSet, 1,
				[]operations.DemandEvent{completedDrainEvent(operations.DemandJobCompleted, 1, "trf-runner")}); err != nil {
				t.Fatal(err)
			}
			store.injectFault = func(candidate string) error {
				if candidate == point {
					return errors.New("unavailable")
				}
				return nil
			}
			if drained, err := store.DrainCompletedRunners(ctx, completedDrainScaleSet); err == nil || len(drained) != 0 {
				t.Fatalf("drained = %v, %v; want an error", drained, err)
			}
			store.injectFault = nil
			if got := completedDrainState(t, store, "trf-runner"); got != operations.StateRunning {
				t.Fatalf("state = %s, want running", got)
			}
		})
	}
	if _, err := testStore(t).DrainCompletedRunners(context.Background(), 0); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("missing scale set = %v, want ErrInvalid", err)
	}
}

// Unreadable rows fail the sweep the same way an unreadable query does.
func TestDrainCompletedRunnersFailsClosedOnUnreadableRows(t *testing.T) {
	for name, rows := range map[string]*injectedRows{
		"scan":    {next: true, scanErr: errors.New("scan")},
		"iterate": {rowsErr: errors.New("iterate")},
		"close":   {closeErr: errors.New("close")},
	} {
		t.Run(name, func(t *testing.T) {
			store := testStore(t)
			store.injectRows = func(point string) rowsScanner {
				if point == "inbox.completed.runners" {
					return rows
				}
				return nil
			}
			if drained, err := store.DrainCompletedRunners(context.Background(), completedDrainScaleSet); err == nil || len(drained) != 0 {
				t.Fatalf("drained = %v, %v; want an error", drained, err)
			}
		})
	}
}

// A row that moves between the sweep's read and its write is left alone, and
// reported as nothing drained. The next tick reads it again.
func TestDrainCompletedRunnersSkipsARowThatMoved(t *testing.T) {
	for _, test := range []struct {
		name  string
		point string
		move  string
		want  operations.State
	}{
		// Another path started the teardown after the query listed the runner.
		{name: "already draining", point: "inbox.runner.active", move: `UPDATE instances SET state='draining' WHERE id=?`,
			want: operations.StateDraining},
		// Another writer bumped the version under the compare-and-set.
		{name: "version race", point: "transition.begin", move: `UPDATE instances SET version=version+1 WHERE id=?`,
			want: operations.StateRunning},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store := testStore(t)
			completedDrainRunner(t, store, "trf-runner", 1, operations.StateRunning)
			if _, err := store.ApplyDemandBatch(ctx, completedDrainScaleSet, 1,
				[]operations.DemandEvent{completedDrainEvent(operations.DemandJobCompleted, 1, "trf-runner")}); err != nil {
				t.Fatal(err)
			}
			moved := false
			store.injectFault = func(point string) error {
				if point == test.point && !moved {
					moved = true
					if _, err := store.db.Exec(test.move, "trf-runner"); err != nil {
						return err
					}
				}
				return nil
			}
			drained, err := store.DrainCompletedRunners(ctx, completedDrainScaleSet)
			if err != nil || len(drained) != 0 || !moved {
				t.Fatalf("drained = %v, %v, moved %v; want a quiet skip", drained, err, moved)
			}
			store.injectFault = nil
			if got := completedDrainState(t, store, "trf-runner"); got != test.want {
				t.Fatalf("state = %s, want %s", got, test.want)
			}
			if test.want != operations.StateRunning {
				return
			}
			if drained, err := store.DrainCompletedRunners(ctx, completedDrainScaleSet); err != nil || len(drained) != 1 {
				t.Fatalf("next sweep drained = %v, %v; want the runner", drained, err)
			}
		})
	}
}
