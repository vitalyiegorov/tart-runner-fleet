package integration

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/sqlite"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/discharge"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/executor"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/lifecycle"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// deadStopWorld is ubuntu-vitalii after #423's inode exhaustion cleared: the
// container the dead drain could not stop is still running, its runner is gone
// from GitHub, and only the backend's own verbs change its power.
type deadStopWorld struct {
	now          time.Time
	power        domain.InstancePower
	confirmation operations.DeletionConfirmation
	stopFails    bool
	stops        int
	deleted      bool
}

func (w *deadStopWorld) Create(context.Context, executor.InstanceSpec) error { panic("no create") }
func (w *deadStopWorld) Start(context.Context, string, operations.Ownership) error {
	panic("no start")
}
func (w *deadStopWorld) Stop(context.Context, string, operations.Ownership) error {
	w.stops++
	if w.stopFails {
		return operations.ErrUncertain
	}
	w.power = domain.InstancePowerStopped
	return nil
}
func (w *deadStopWorld) Terminate(ctx context.Context, id string, o operations.Ownership) error {
	return w.Stop(ctx, id, o)
}
func (w *deadStopWorld) Destroy(ctx context.Context, id string, o operations.Ownership) error {
	return w.Stop(ctx, id, o)
}
func (w *deadStopWorld) Delete(context.Context, string, operations.Ownership) error {
	w.deleted, w.power = true, domain.InstancePowerAbsent
	return nil
}
func (w *deadStopWorld) Reap(context.Context, string, operations.Ownership) error {
	panic("automatic recovery must never reap directly")
}
func (w *deadStopWorld) Power(context.Context, string) (domain.InstancePower, error) {
	return w.power, nil
}
func (w *deadStopWorld) ConfirmDeletion(context.Context, string) (operations.DeletionConfirmation, error) {
	return w.confirmation, nil
}
func (w *deadStopWorld) SafeToDeregister(context.Context, operations.Instance) (bool, error) {
	return true, nil
}
func (w *deadStopWorld) Deregister(context.Context, operations.Instance) error { return nil }
func (w *deadStopWorld) RunnerRegistered(context.Context, operations.Instance) (bool, error) {
	return false, nil
}
func (w *deadStopWorld) RunnerBusy(context.Context, operations.Instance) (bool, error) {
	return false, nil
}
func (w *deadStopWorld) StartUnattributed(context.Context, operations.Instance) (bool, error) {
	return false, nil
}

const deadStopInstance = "trf-linux-4x8-83404cc4efa01437"

// deadStopFixture reproduces one of #424's rows: an event drain that reached
// deregistering, then spent its whole stop budget during the inode outage and
// died (not parked: the container is still running).
func deadStopFixture(t *testing.T) (*sqlite.Store, *deadStopWorld, string) {
	t.Helper()
	ctx := context.Background()
	died := time.Date(2026, 10, 25, 15, 35, 0, 0, time.UTC)
	world := &deadStopWorld{now: died, power: domain.InstancePowerRunning}
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "fleet.db"), sqlite.WithClock(func() time.Time { return world.now }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ownership := operations.Ownership{ControllerID: "tart-runner-fleet", ResourceID: deadStopInstance, OperationID: "op-provision"}
	if err := store.CreateInstance(ctx, operations.Instance{ID: deadStopInstance, State: operations.StateDeregistering,
		DrainPhase: operations.DrainPhaseEvent, Ownership: ownership, CreatedAt: died.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	drain := "event-drain-" + deadStopInstance
	plan := operations.Plan{ID: "drain-plan", CreatedAt: died.Add(-3 * time.Minute),
		Scheduler: operations.SchedulerState{Version: 1, Data: []byte(`{}`), Reservations: []byte(`[]`), DeficitRoundRobin: []byte(`{}`), ObservationCursor: "cursor"},
		Operations: []operations.Operation{{ID: drain, IdempotencyKey: drain, EffectKey: "deregister:" + deadStopInstance,
			Kind: lifecycle.OperationDrain, ResourceID: deadStopInstance, AvailableAt: died.Add(-3 * time.Minute)}}}
	if applied, err := store.ApplyPlan(ctx, plan); err != nil || !applied {
		t.Fatalf("apply drain: %v %v", applied, err)
	}
	killDrain(t, store, died)
	world.now = died.Add(50 * time.Minute)
	world.confirmation = operations.DeletionConfirmation{Fresh: true, RunnerInactive: true, JobsInactive: true, ObservedAt: world.now}
	return store, world, drain
}

// killDrain makes the one claimable drain die exactly as an exhausted stop
// ladder does: dead, with the plain stop code.
func killDrain(t *testing.T, store *sqlite.Store, at time.Time) {
	t.Helper()
	claimed, err := store.Claim(context.Background(), "worker", 1, at, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim drain: %#v %v", claimed, err)
	}
	if err := store.Retry(context.Background(), claimed[0].ID, "worker", "runner lifecycle failed at stop", time.Time{}, true); err != nil {
		t.Fatal(err)
	}
}

// recoveryTick is the authority tick's cleanup-recovery pass: every durable
// dead letter is offered with the tick's own instance and host observation.
func recoveryTick(t *testing.T, store *sqlite.Store, world *deadStopWorld, hostHealthy bool, power domain.InstancePower) {
	t.Helper()
	ctx := context.Background()
	service := discharge.Service{Store: store, VM: world, Authority: true, Now: func() time.Time { return world.now }}
	letters, err := store.DeadLetters(ctx)
	if err != nil {
		t.Fatal(err)
	}
	row, err := store.Instance(ctx, deadStopInstance)
	if err != nil {
		t.Fatal(err)
	}
	instance := domain.Instance{ID: row.ID, State: row.State, Power: power}
	for _, letter := range letters {
		if err := service.RecoverCleanup(ctx, letter, instance, hostHealthy, world); err != nil {
			t.Fatal(err)
		}
	}
}

func deadStopWorker(store *sqlite.Store, world *deadStopWorld) operations.Worker {
	return operations.Worker{Store: store, Owner: "worker", MaxConcurrent: 1, Now: func() time.Time { return world.now },
		Executors: map[string]operations.Executor{lifecycle.OperationDrain: lifecycle.DrainExecutor{State: store, VM: world,
			Control: world, Now: func() time.Time { return world.now }}}}
}

func operationsFor(t *testing.T, store *sqlite.Store) []operations.DeadLetter {
	t.Helper()
	letters, err := store.DeadLetters(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return letters
}

// TestDeadStopDrainWithRunningGuestIsResumedByTheWorker is #424's red replay:
// before the fix the dead letter stayed dead forever, the container kept
// running and the instance held its slot in deregistering.
func TestDeadStopDrainWithRunningGuestIsResumedByTheWorker(t *testing.T) {
	ctx := context.Background()
	store, world, drain := deadStopFixture(t)
	recoveryTick(t, store, world, true, domain.InstancePowerRunning)
	// Idempotent: a second tick before the worker runs enqueues nothing more.
	recoveryTick(t, store, world, true, domain.InstancePowerRunning)
	if world.stops != 0 {
		t.Fatal("the tick itself stopped the guest; only the worker may")
	}
	if letters := operationsFor(t, store); len(letters) != 0 {
		t.Fatalf("dead letter was not superseded: %#v", letters)
	}
	ran, err := deadStopWorker(store, world).RunOnce(ctx)
	if err != nil || ran != 1 {
		t.Fatalf("worker ran=%d err=%v; want exactly one successor", ran, err)
	}
	if ran, err := deadStopWorker(store, world).RunOnce(ctx); err != nil || ran != 0 {
		t.Fatalf("a second successor ran: %d %v", ran, err)
	}
	instance, err := store.Instance(ctx, deadStopInstance)
	if err != nil || instance.State != operations.StateDeleted {
		t.Fatalf("instance=%#v err=%v; want deleted", instance, err)
	}
	if world.stops != 1 || !world.deleted {
		t.Fatalf("stops=%d deleted=%v", world.stops, world.deleted)
	}
	// The dead attempt is preserved as discharged: resuming it again is the
	// idempotent no-op only a discharged record answers with.
	if err := store.ResumeCleanup(ctx, drain, deadStopInstance, false, world.now); err != nil {
		t.Fatalf("old attempt was not preserved as discharged: %v", err)
	}
}

// TestDeadStopDrainStaysDeadWithoutEveryFreshProof keeps the dead letter for
// every unproved premise: a host still at fault (or unobserved), a runner GitHub
// still reports busy, and a power the backend could not read.
func TestDeadStopDrainStaysDeadWithoutEveryFreshProof(t *testing.T) {
	for _, name := range []string{"stale host", "busy runner", "unknown power", "unknown inventory power"} {
		t.Run(name, func(t *testing.T) {
			store, world, _ := deadStopFixture(t)
			healthy, power := true, domain.InstancePowerRunning
			switch name {
			case "stale host":
				healthy = false
			case "busy runner":
				world.confirmation.JobsInactive = false
			case "unknown power":
				world.power = domain.InstancePowerUnknown
			case "unknown inventory power":
				power = domain.InstancePowerUnknown
			}
			recoveryTick(t, store, world, healthy, power)
			if letters := operationsFor(t, store); len(letters) != 1 {
				t.Fatalf("dead letter was resumed without proof: %#v", letters)
			}
			if world.stops != 0 {
				t.Fatal("guest was stopped without proof")
			}
		})
	}
}

// TestDeadStopDrainResumptionBacksOffAndIsBounded lets every successor die at
// stop again. The tick must not loop hot, and must hand the dead letter to the
// operator after a bounded number of resumptions.
func TestDeadStopDrainResumptionBacksOffAndIsBounded(t *testing.T) {
	store, world, _ := deadStopFixture(t)
	resumed := 0
	for round := range 12 {
		recoveryTick(t, store, world, true, domain.InstancePowerRunning)
		if len(operationsFor(t, store)) == 0 {
			resumed++
			killDrain(t, store, world.now)
			// Immediately after a successor dies the tick must wait.
			recoveryTick(t, store, world, true, domain.InstancePowerRunning)
			if len(operationsFor(t, store)) != 1 {
				t.Fatalf("round %d resumed a just-died successor without backoff", round)
			}
		}
		world.now = world.now.Add(30 * time.Minute)
		world.confirmation.ObservedAt = world.now
	}
	if resumed != operations.MaxCleanupResumptions {
		t.Fatalf("resumed %d times, want the bound %d", resumed, operations.MaxCleanupResumptions)
	}
}
