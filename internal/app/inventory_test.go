package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/executor"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

type fakeInstances struct {
	values []operations.Instance
	err    error
}

func (f fakeInstances) LiveInstances(context.Context) ([]operations.Instance, error) {
	return f.values, f.err
}

type fakeTart struct {
	values []executor.Instance
	err    error
}

func (f fakeTart) List(context.Context) ([]executor.Instance, error) { return f.values, f.err }

type fakeHost struct{ value executor.HostSnapshot }

func (f fakeHost) Snapshot(context.Context) executor.HostSnapshot { return f.value }

type fakeRecoveryObserver struct {
	confirmation operations.DeletionConfirmation
	err          error
	jobActive    bool
	jobActiveErr error
}

func (f fakeRecoveryObserver) ConfirmDeletion(context.Context, string) (operations.DeletionConfirmation, error) {
	return f.confirmation, f.err
}

func (f fakeRecoveryObserver) JobActive(context.Context, operations.Instance) (bool, error) {
	return f.jobActive, f.jobActiveErr
}

func inventoryInstance(state operations.State) operations.Instance {
	return operations.Instance{ID: "trf-small-1", Repo: "o/r", Platform: domain.PlatformLinux, Profile: "small", Route: "tiered",
		Resources: domain.Resources{CPU: 2, MemoryMB: 4096, Slots: 1}, Demand: domain.DemandKey{Repo: "o/r", RunID: 1, Attempt: 1, JobID: 1},
		State: state, Ownership: operations.Ownership{ControllerID: "c", ResourceID: "r", OperationID: "o"}}
}

func healthySnapshot(now time.Time) executor.HostSnapshot {
	return executor.HostSnapshot{Freshness: executor.Fresh, ObservedAt: now, AvailableMemoryMB: 12000, FreeDiskGB: 100, CPUidlePercent: 80, LoadAverage: 1}
}

func TestProductionInventoryFreshSnapshot(t *testing.T) {
	now := time.Now().UTC()
	inv := ProductionInventory{Store: fakeInstances{values: []operations.Instance{inventoryInstance(operations.StateRunning)}},
		Executor: fakeTart{values: []executor.Instance{{Name: "trf-small-1", Power: domain.InstancePowerRunning}}}, Host: fakeHost{healthySnapshot(now)},
		Capacity: domain.Resources{CPU: 8, MemoryMB: 16384, Slots: 4}, Guards: executor.Guardrails{MinFreeDiskGB: 60, MinAvailableMemoryMB: 2048}}
	instances, host := inv.Observe(context.Background())
	if !instances.Usable() || len(instances.Value) != 1 || instances.Value[0].State != domain.InstanceRunning || instances.Value[0].Power != domain.InstancePowerRunning {
		t.Fatalf("instances = %#v", instances)
	}
	if !host.Usable() || host.Value.Available.CPU != 8 || host.Value.Available.MemoryMB != 9952 || host.Value.Available.Slots != 4 {
		t.Fatalf("host = %#v", host)
	}
	if host.Value.Pressure.FreeDiskGB != 100 || host.Value.Pressure.AvailableMemoryMB != 12000 ||
		host.Value.Pressure.CPUIdlePercent != 80 || host.Value.Pressure.AdmissionReason != "capacity available" {
		t.Fatalf("host pressure = %#v", host.Value.Pressure)
	}
}

func TestProductionInventoryMarksStoppedOwnedRunner(t *testing.T) {
	now := time.Now().UTC()
	inv := ProductionInventory{Store: fakeInstances{values: []operations.Instance{inventoryInstance(operations.StateAssigned)}},
		Executor: fakeTart{values: []executor.Instance{{Name: "trf-small-1", Power: domain.InstancePowerStopped}}}, Host: fakeHost{healthySnapshot(now)},
		Capacity: domain.Resources{CPU: 8, MemoryMB: 16384, Slots: 4}}
	instances, _ := inv.Observe(context.Background())
	if !instances.Usable() || len(instances.Value) != 1 || instances.Value[0].Power != domain.InstancePowerStopped {
		t.Fatalf("stopped instance = %#v", instances)
	}
}

// A VM that a SUCCESSFUL `tart list` does not enumerate is a proven per-instance
// fact, not a host-wide probe failure. While an instance is tearing down it is
// also an expected intermediate observation: DrainExecutor deletes the VM and
// only then advances the row to deleted, and the tick reads the durable rows and
// Tart at two different instants, so any interleaving observes a live cleanup row
// with no VM. Turning the whole observation Unavailable there stops planning for
// every profile on the host over one instance whose cleanup is already finishing
// (Stop and Delete are absent-idempotent), so absence during cleanup is reported
// per instance and the rest of the inventory stays fresh.
func TestAbsentOwnedVMDuringCleanupIsPerInstanceNotHostWide(t *testing.T) {
	now := time.Now().UTC()
	for _, state := range []operations.State{operations.StateDraining, operations.StateDeregistering, operations.StateStopping} {
		t.Run(string(state), func(t *testing.T) {
			absent := inventoryInstance(state)
			healthy := inventoryInstance(operations.StateRunning)
			healthy.ID = "trf-small-2"
			inv := ProductionInventory{Store: fakeInstances{values: []operations.Instance{absent, healthy}},
				Executor: fakeTart{values: []executor.Instance{{Name: "trf-small-2", Power: domain.InstancePowerRunning}}}, Host: fakeHost{healthySnapshot(now)},
				Capacity: domain.Resources{CPU: 8, MemoryMB: 16384, Slots: 4}}
			instances, _ := inv.Observe(context.Background())
			if instances.State != domain.ObservationFresh || len(instances.Value) != 2 {
				t.Fatalf("observation = %#v", instances)
			}
			if got := instances.Value[0]; got.Power != domain.InstancePowerAbsent || got.ConsumesHostResources() {
				t.Fatalf("absent instance = %#v", got)
			}
			if got := instances.Value[1]; got.Power != domain.InstancePowerRunning {
				t.Fatalf("healthy instance = %#v", got)
			}
		})
	}
}

// The narrowing is bounded by the controller invariant that produces it: a row
// leaves Planned only after Clone succeeded, so every other live state means the
// VM was removed out of band with no cleanup operation reconciling it and no
// non-destructive reclamation available. That stays host-wide fail-closed, and so
// does a Tart probe that failed rather than proved absence.
func TestAbsentOwnedVMOutsideCleanupStaysHostWideUnavailable(t *testing.T) {
	now := time.Now().UTC()
	for _, state := range []operations.State{operations.StateCloning, operations.StateBooting, operations.StateReachable,
		operations.StateRegistering, operations.StateOnlineIdle, operations.StateAssigned, operations.StateRunning, operations.StateFailed} {
		t.Run(string(state), func(t *testing.T) {
			inv := ProductionInventory{Store: fakeInstances{values: []operations.Instance{inventoryInstance(state)}},
				Executor: fakeTart{}, Host: fakeHost{healthySnapshot(now)}, Capacity: domain.Resources{CPU: 8, MemoryMB: 16384, Slots: 4}}
			instances, _ := inv.Observe(context.Background())
			if instances.State != domain.ObservationUnavailable || instances.Reason != "owned VM trf-small-1 missing from Tart" {
				t.Fatalf("observation = %#v", instances)
			}
		})
	}
	inv := ProductionInventory{Store: fakeInstances{values: []operations.Instance{inventoryInstance(operations.StateDraining)}},
		Executor: fakeTart{err: errors.New("tart is unreachable")}, Host: fakeHost{healthySnapshot(now)},
		Capacity: domain.Resources{CPU: 8, MemoryMB: 16384, Slots: 4}}
	instances, _ := inv.Observe(context.Background())
	if instances.State != domain.ObservationUnavailable || instances.Reason != "Tart inventory unavailable" {
		t.Fatalf("unreadable probe = %#v", instances)
	}
}

func TestProductionInventoryMarksRunningCompletedRunnerRecoverableFromFreshEvidence(t *testing.T) {
	now := time.Now().UTC()
	confirmation := operations.DeletionConfirmation{Fresh: true, RunnerInactive: true, JobsInactive: true, ObservedAt: now}
	inv := ProductionInventory{Store: fakeInstances{values: []operations.Instance{inventoryInstance(operations.StateRunning)}},
		Executor: fakeTart{values: []executor.Instance{{Name: "trf-small-1", Power: domain.InstancePowerRunning}}}, Host: fakeHost{healthySnapshot(now)},
		Recovery: fakeRecoveryObserver{confirmation: confirmation}, RecoveryConfirmationMaxAge: time.Minute,
		Capacity: domain.Resources{CPU: 8, MemoryMB: 16384, Slots: 4}}
	instances, _ := inv.Observe(context.Background())
	if !instances.Usable() || len(instances.Value) != 1 || !instances.Value[0].RecoveryReady {
		t.Fatalf("fresh completed recovery evidence = %#v", instances)
	}
	inv.Recovery = fakeRecoveryObserver{err: errors.New("unavailable")}
	instances, _ = inv.Observe(context.Background())
	if !instances.Usable() || instances.Value[0].RecoveryReady {
		t.Fatalf("unavailable evidence inferred inactive = %#v", instances)
	}
}

func TestProductionInventoryObservesLingeringRunnerEvidence(t *testing.T) {
	now := time.Now().UTC()
	entered := now.Add(-30 * time.Minute)
	running := inventoryInstance(operations.StateRunning)
	running.UpdatedAt = entered
	base := func() ProductionInventory {
		return ProductionInventory{Store: fakeInstances{values: []operations.Instance{running}},
			Executor: fakeTart{values: []executor.Instance{{Name: "trf-small-1", Power: domain.InstancePowerRunning}}}, Host: fakeHost{healthySnapshot(now)},
			RecoveryConfirmationMaxAge: time.Minute, Capacity: domain.Resources{CPU: 8, MemoryMB: 16384, Slots: 4}}
	}
	for _, test := range []struct {
		name            string
		recovery        RecoveryObserver
		wantJobInactive bool
	}{
		// A job actively executing (JobActive true) is never a lingerer, whatever
		// its age; a terminal/absent demand (JobActive false) marks it inactive; an
		// unreadable demand fails closed to "active job present, do not touch".
		{name: "active job is healthy", recovery: fakeRecoveryObserver{jobActive: true}, wantJobInactive: false},
		{name: "no active job is a lingerer", recovery: fakeRecoveryObserver{jobActive: false}, wantJobInactive: true},
		{name: "unreadable demand fails closed", recovery: fakeRecoveryObserver{jobActiveErr: errors.New("uncertain")}, wantJobInactive: false},
		{name: "absent recovery observer fails closed", recovery: nil, wantJobInactive: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			inv := base()
			inv.Recovery = test.recovery
			instances, _ := inv.Observe(context.Background())
			if !instances.Usable() || len(instances.Value) != 1 {
				t.Fatalf("observation = %#v", instances)
			}
			got := instances.Value[0]
			if !got.RunningSince.Equal(entered) || got.JobInactive != test.wantJobInactive {
				t.Fatalf("runningSince=%s jobInactive=%v; want since=%s inactive=%v", got.RunningSince, got.JobInactive, entered, test.wantJobInactive)
			}
		})
	}
}

func TestProductionInventoryUsesBoundedDefaultRecoveryEvidenceAge(t *testing.T) {
	if got := (ProductionInventory{}).recoveryConfirmationMaxAge(); got != 30*time.Second {
		t.Fatalf("default recovery evidence age = %v", got)
	}
}

func TestProductionInventoryKnownPressureReturnsFreshZeroCapacity(t *testing.T) {
	now := time.Now().UTC()
	snapshot := healthySnapshot(now)
	snapshot.FreeDiskGB = 1
	inv := ProductionInventory{Store: fakeInstances{}, Executor: fakeTart{}, Host: fakeHost{snapshot}, Capacity: domain.Resources{CPU: 8, MemoryMB: 16384, Slots: 4}, Guards: executor.Guardrails{MinFreeDiskGB: 60}}
	_, host := inv.Observe(context.Background())
	if !host.Usable() || host.Value.Available != (domain.Resources{}) || host.Reason != "disk reserve" {
		t.Fatalf("host = %#v", host)
	}
	if host.Value.Pressure.AdmissionAllowed || host.Value.Pressure.AdmissionReason != "disk reserve" || host.Value.Pressure.FreeDiskGB != 1 {
		t.Fatalf("pressure = %#v", host.Value.Pressure)
	}
}

func TestHostObservationCapsMemoryAndRejectsInvalidFreshness(t *testing.T) {
	now := time.Now().UTC()
	snapshot := healthySnapshot(now)
	snapshot.AvailableMemoryMB = 99_999
	got := hostObservation(snapshot, domain.Resources{CPU: 8, MemoryMB: 16_000, Slots: 4}, executor.Guardrails{}, false, domain.Resources{}, false)
	if got.Value.Available.MemoryMB != 16_000 {
		t.Fatalf("available memory = %d", got.Value.Available.MemoryMB)
	}
	snapshot.Freshness = "bad"
	if got := hostObservation(snapshot, domain.Resources{}, executor.Guardrails{}, false, domain.Resources{}, false); got.State != domain.ObservationUnavailable {
		t.Fatalf("invalid freshness = %#v", got)
	}
}

func TestProductionInventoryFailsClosedOnUncertainty(t *testing.T) {
	now := time.Now().UTC()
	want := errors.New("down")
	tests := []struct {
		name      string
		inv       ProductionInventory
		wantState domain.ObservationState
	}{
		{name: "store", inv: ProductionInventory{Store: fakeInstances{err: want}, Executor: fakeTart{}, Host: fakeHost{healthySnapshot(now)}}, wantState: domain.ObservationUnavailable},
		{name: "tart", inv: ProductionInventory{Store: fakeInstances{}, Executor: fakeTart{err: want}, Host: fakeHost{healthySnapshot(now)}}, wantState: domain.ObservationUnavailable},
		{name: "stale host", inv: ProductionInventory{Store: fakeInstances{}, Executor: fakeTart{}, Host: fakeHost{executor.HostSnapshot{Freshness: executor.Stale, ObservedAt: now}}}, wantState: domain.ObservationStale},
		{name: "unavailable host", inv: ProductionInventory{Store: fakeInstances{}, Executor: fakeTart{}, Host: fakeHost{executor.HostSnapshot{Freshness: executor.Unavailable}}}, wantState: domain.ObservationUnavailable},
		{name: "missing vm", inv: ProductionInventory{Store: fakeInstances{values: []operations.Instance{inventoryInstance(operations.StateRunning)}}, Executor: fakeTart{}, Host: fakeHost{healthySnapshot(now)}}, wantState: domain.ObservationUnavailable},
		{name: "orphan vm", inv: ProductionInventory{Store: fakeInstances{}, Executor: fakeTart{values: []executor.Instance{{Name: "trf-orphan", Power: domain.InstancePowerRunning}}}, Host: fakeHost{healthySnapshot(now)}}, wantState: domain.ObservationUnavailable},
		{name: "bad metadata", inv: ProductionInventory{Store: fakeInstances{values: []operations.Instance{{ID: "trf-bad", State: operations.StatePlanned}}}, Executor: fakeTart{}, Host: fakeHost{healthySnapshot(now)}}, wantState: domain.ObservationUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.inv.Capacity = domain.Resources{CPU: 8, MemoryMB: 16000, Slots: 4}
			instances, host := tt.inv.Observe(context.Background())
			if tt.name == "stale host" || tt.name == "unavailable host" {
				if host.State != tt.wantState {
					t.Fatalf("host=%#v", host)
				}
			} else if instances.State != tt.wantState {
				t.Fatalf("instances=%#v", instances)
			}
		})
	}
}

func TestPlannedInstanceMayPrecedeTartCloneAndValidation(t *testing.T) {
	now := time.Now().UTC()
	planned := inventoryInstance(operations.StatePlanned)
	inv := ProductionInventory{Store: fakeInstances{values: []operations.Instance{planned}}, Executor: fakeTart{}, Host: fakeHost{healthySnapshot(now)}, Capacity: domain.Resources{CPU: 8, MemoryMB: 16000, Slots: 4}}
	instances, _ := inv.Observe(context.Background())
	if !instances.Usable() || len(instances.Value) != 1 {
		t.Fatalf("instances=%#v", instances)
	}
	if got, _ := (ProductionInventory{}).Observe(context.Background()); got.State != domain.ObservationUnavailable {
		t.Fatalf("nil adapters=%#v", got)
	}
}

// The occupancy clock is the durable row's creation instant, and it must survive
// the state change that resets every other per-instance deadline. Reading
// UpdatedAt here is what made the 2026-08-09 leak unmeasurable: an instance that
// had held six cores for seventy-five minutes looked one state change old
// (issue #223).
func TestInventoryCarriesTheOccupancyClockFromRowCreation(t *testing.T) {
	now := time.Now().UTC()
	stored := inventoryInstance(operations.StateRunning)
	stored.CreatedAt = now.Add(-75 * time.Minute)
	stored.UpdatedAt = now.Add(-90 * time.Second)
	inv := ProductionInventory{Store: fakeInstances{values: []operations.Instance{stored}},
		Executor: fakeTart{values: []executor.Instance{{Name: stored.ID, Power: domain.InstancePowerRunning}}}, Host: fakeHost{healthySnapshot(now)},
		Capacity: domain.Resources{CPU: 8, MemoryMB: 16384, Slots: 4}}

	instances, _ := inv.Observe(context.Background())

	observed := instances.Value[0]
	if !observed.OccupiedSince.Equal(stored.CreatedAt) {
		t.Fatalf("occupancy must start at row creation; got %s, want %s", observed.OccupiedSince, stored.CreatedAt)
	}
	if !observed.RunningSince.Equal(stored.UpdatedAt) {
		t.Fatalf("the idle-runner deadline must still read the last state change; got %s", observed.RunningSince)
	}
	if age, measured := observed.Occupancy(now); !measured || age < 75*time.Minute {
		t.Fatalf("occupancy = %s, measured=%t", age, measured)
	}
}

// TestTheBackendsUnreadablePowerTravelsIntactThroughTheInventory is issue #252 at
// the seam that used to manufacture the premise.
//
// This function derived `Stopped` from a bool that meant either "powered off" or
// "I could not look", because the port had nowhere to put the second one. It now
// carries the backend's own classification through untouched — including the
// reason and the latency, which nothing decides on and which are the only
// evidence an operator will have the next time the trigger appears.
func TestTheBackendsUnreadablePowerTravelsIntactThroughTheInventory(t *testing.T) {
	now := time.Now().UTC()
	unreadable := domain.PowerReadFailure{Reason: domain.PowerReadDescriptors, Latency: 250 * time.Millisecond}
	inv := ProductionInventory{Store: fakeInstances{values: []operations.Instance{inventoryInstance(operations.StateRunning)}},
		Executor: fakeTart{values: []executor.Instance{{Name: "trf-small-1",
			Power: domain.InstancePowerUnknown, Unreadable: unreadable}}},
		Host: fakeHost{healthySnapshot(now)}, Power: &PowerCorroborator{},
		Capacity: domain.Resources{CPU: 8, MemoryMB: 16384, Slots: 4}}
	instances, _ := inv.Observe(context.Background())
	if !instances.Usable() || len(instances.Value) != 1 {
		t.Fatalf("observation = %#v", instances)
	}
	got := instances.Value[0]
	if got.Power != domain.InstancePowerUnknown {
		t.Fatalf("power = %q; a reading the backend could not take is not a stopped VM", got.Power)
	}
	if got.PowerUnreadable != unreadable {
		t.Fatalf("unreadable = %#v, want %#v", got.PowerUnreadable, unreadable)
	}
	// And it accumulates nothing: an unread power state must never fill the run a
	// destructive premise is corroborated from (ADR 0042).
	if got.PowerRun.Refusals != 0 {
		t.Fatalf("an unreadable reading accumulated %d refusals toward a reclaim", got.PowerRun.Refusals)
	}
	// It also still charges the host, which is the conservative direction: the
	// fleet has not established that this VM gave its vector back either.
	if !got.ConsumesHostResources() {
		t.Fatal("an instance whose power could not be read stopped charging the host")
	}
}
