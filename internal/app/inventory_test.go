package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/macos"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/tart"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
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
	values []tart.VM
	err    error
}

func (f fakeTart) List(context.Context) ([]tart.VM, error) { return f.values, f.err }

type fakeHost struct{ value macos.Snapshot }

func (f fakeHost) Snapshot(context.Context) macos.Snapshot { return f.value }

func inventoryInstance(state operations.State) operations.Instance {
	return operations.Instance{ID: "trf-small-1", Repo: "o/r", Platform: domain.PlatformLinux, Profile: "small", Route: "tiered",
		Resources: domain.Resources{CPU: 2, MemoryMB: 4096, Slots: 1}, Demand: domain.DemandKey{Repo: "o/r", RunID: 1, Attempt: 1, JobID: 1},
		State: state, Ownership: operations.Ownership{ControllerID: "c", ResourceID: "r", OperationID: "o"}}
}

func healthySnapshot(now time.Time) macos.Snapshot {
	return macos.Snapshot{Freshness: macos.Fresh, ObservedAt: now, AvailableMemoryMB: 12000, FreeDiskGB: 100, CPUidlePercent: 80, LoadAverage: 1}
}

func TestProductionInventoryFreshSnapshot(t *testing.T) {
	now := time.Now().UTC()
	inv := ProductionInventory{Store: fakeInstances{values: []operations.Instance{inventoryInstance(operations.StateRunning)}},
		Tart: fakeTart{values: []tart.VM{{Name: "trf-small-1", Running: true}}}, Host: fakeHost{healthySnapshot(now)},
		Capacity: domain.Resources{CPU: 8, MemoryMB: 16384, Slots: 4}, Guards: macos.Guardrails{MinFreeDiskGB: 60, MinAvailableMemoryMB: 2048}}
	instances, host := inv.Observe(context.Background())
	if !instances.Usable() || len(instances.Value) != 1 || instances.Value[0].State != domain.InstanceRunning {
		t.Fatalf("instances = %#v", instances)
	}
	if !host.Usable() || host.Value.Available.CPU != 8 || host.Value.Available.MemoryMB != 9952 || host.Value.Available.Slots != 4 {
		t.Fatalf("host = %#v", host)
	}
}

func TestProductionInventoryKnownPressureReturnsFreshZeroCapacity(t *testing.T) {
	now := time.Now().UTC()
	snapshot := healthySnapshot(now)
	snapshot.FreeDiskGB = 1
	inv := ProductionInventory{Store: fakeInstances{}, Tart: fakeTart{}, Host: fakeHost{snapshot}, Capacity: domain.Resources{CPU: 8, MemoryMB: 16384, Slots: 4}, Guards: macos.Guardrails{MinFreeDiskGB: 60}}
	_, host := inv.Observe(context.Background())
	if !host.Usable() || host.Value.Available != (domain.Resources{}) || host.Reason != "disk reserve" {
		t.Fatalf("host = %#v", host)
	}
}

func TestHostObservationCapsMemoryAndRejectsInvalidFreshness(t *testing.T) {
	now := time.Now().UTC()
	snapshot := healthySnapshot(now)
	snapshot.AvailableMemoryMB = 99_999
	got := hostObservation(snapshot, domain.Resources{CPU: 8, MemoryMB: 16_000, Slots: 4}, macos.Guardrails{})
	if got.Value.Available.MemoryMB != 16_000 {
		t.Fatalf("available memory = %d", got.Value.Available.MemoryMB)
	}
	snapshot.Freshness = "bad"
	if got := hostObservation(snapshot, domain.Resources{}, macos.Guardrails{}); got.State != domain.ObservationUnavailable {
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
		{name: "store", inv: ProductionInventory{Store: fakeInstances{err: want}, Tart: fakeTart{}, Host: fakeHost{healthySnapshot(now)}}, wantState: domain.ObservationUnavailable},
		{name: "tart", inv: ProductionInventory{Store: fakeInstances{}, Tart: fakeTart{err: want}, Host: fakeHost{healthySnapshot(now)}}, wantState: domain.ObservationUnavailable},
		{name: "stale host", inv: ProductionInventory{Store: fakeInstances{}, Tart: fakeTart{}, Host: fakeHost{macos.Snapshot{Freshness: macos.Stale, ObservedAt: now}}}, wantState: domain.ObservationStale},
		{name: "unavailable host", inv: ProductionInventory{Store: fakeInstances{}, Tart: fakeTart{}, Host: fakeHost{macos.Snapshot{Freshness: macos.Unavailable}}}, wantState: domain.ObservationUnavailable},
		{name: "missing vm", inv: ProductionInventory{Store: fakeInstances{values: []operations.Instance{inventoryInstance(operations.StateRunning)}}, Tart: fakeTart{}, Host: fakeHost{healthySnapshot(now)}}, wantState: domain.ObservationUnavailable},
		{name: "orphan vm", inv: ProductionInventory{Store: fakeInstances{}, Tart: fakeTart{values: []tart.VM{{Name: "trf-orphan", Running: true}}}, Host: fakeHost{healthySnapshot(now)}}, wantState: domain.ObservationUnavailable},
		{name: "bad metadata", inv: ProductionInventory{Store: fakeInstances{values: []operations.Instance{{ID: "trf-bad", State: operations.StatePlanned}}}, Tart: fakeTart{}, Host: fakeHost{healthySnapshot(now)}}, wantState: domain.ObservationUnavailable},
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
	inv := ProductionInventory{Store: fakeInstances{values: []operations.Instance{planned}}, Tart: fakeTart{}, Host: fakeHost{healthySnapshot(now)}, Capacity: domain.Resources{CPU: 8, MemoryMB: 16000, Slots: 4}}
	instances, _ := inv.Observe(context.Background())
	if !instances.Usable() || len(instances.Value) != 1 {
		t.Fatalf("instances=%#v", instances)
	}
	if got, _ := (ProductionInventory{}).Observe(context.Background()); got.State != domain.ObservationUnavailable {
		t.Fatalf("nil adapters=%#v", got)
	}
}
