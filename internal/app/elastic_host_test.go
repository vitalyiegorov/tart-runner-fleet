package app

import (
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/executor"
)

// productionSnapshot is the host reading fleetd actually reported during the
// 2026-07-25 incident on the 10-core / 24 GiB Mac mini.
func productionSnapshot() executor.HostSnapshot {
	return executor.HostSnapshot{
		Freshness:         executor.Fresh,
		ObservedAt:        time.Unix(1000, 0).UTC(),
		AvailableMemoryMB: 12_288,
		FreeDiskGB:        152,
		SwapUsedMB:        574,
		CPUidlePercent:    62.4,
		LoadAverage:       4.19,
		PhysicalCPU:       10,
		PhysicalMemoryMB:  24_576,
	}
}

func productionGuards() executor.Guardrails {
	return executor.Guardrails{MinFreeDiskGB: 60, MinAvailableMemoryMB: 1024, MaxSwapUsedMB: 2048,
		MaxLoadAverage: 9, MinCPUidlePercent: 5}
}

// configuredCapacity is the static 8-vCPU / 16-GiB envelope from fleet.json.
func configuredCapacity() domain.Resources {
	return domain.Resources{CPU: 8, MemoryMB: 16_384, Slots: 4}
}

// TestHostObservationReportsPhysicalCapacityWhenElastic proves the observation
// carries the real machine so the scheduler can bound aggregate reservations by
// it, and derives available CPU from measured idle instead of echoing config.
func TestHostObservationReportsPhysicalCapacityWhenElastic(t *testing.T) {
	observation := hostObservation(productionSnapshot(), configuredCapacity(), productionGuards(), true, domain.Resources{})
	if !observation.Usable() {
		t.Fatalf("observation unusable: %#v", observation)
	}
	host := observation.Value
	if host.Capacity.CPU != 10 {
		t.Fatalf("Capacity.CPU = %d, want the 10 physical cores", host.Capacity.CPU)
	}
	// The physical memory total is reduced by the host's reserve, never more.
	if want := 24_576 - 1024; host.Capacity.MemoryMB != want {
		t.Fatalf("Capacity.MemoryMB = %d, want %d (physical minus reserve)", host.Capacity.MemoryMB, want)
	}
	// floor(10 cores x 62.4% idle) = 6 measured free cores.
	if host.Available.CPU != 6 {
		t.Fatalf("Available.CPU = %d, want 6 from floor(10 x 62.4%%)", host.Available.CPU)
	}
}

// TestHostObservationYieldsCPUAsHostGetsBusy is the second-pilot signal: as the
// host's own tenant consumes cores the advertised availability shrinks toward
// zero, so the fleet waits instead of competing.
func TestHostObservationYieldsCPUAsHostGetsBusy(t *testing.T) {
	previous := -1
	for _, idle := range []float64{2, 15, 40, 80, 100} {
		snapshot := productionSnapshot()
		snapshot.CPUidlePercent = idle
		available := hostObservation(snapshot, configuredCapacity(), productionGuards(), true, domain.Resources{}).Value.Available.CPU
		if available < previous {
			t.Fatalf("idle %.0f%% advertised %d cores, fewer than %d on a busier host", idle, available, previous)
		}
		if available > 10 {
			t.Fatalf("idle %.0f%% advertised %d cores, more than the 10 physical", idle, available)
		}
		previous = available
	}
	// A saturated host must advertise no free cores at all.
	saturated := productionSnapshot()
	saturated.CPUidlePercent = 2
	if got := hostObservation(saturated, configuredCapacity(), productionGuards(), true, domain.Resources{}).Value.Available.CPU; got != 0 {
		t.Fatalf("a 2%%-idle host advertised %d free cores, want 0", got)
	}
}

// TestHostObservationClampsNonsenseIdle proves an out-of-range idle reading is
// clamped rather than trusted into advertising more cores than exist, or a
// negative count.
func TestHostObservationClampsNonsenseIdle(t *testing.T) {
	for _, test := range []struct {
		idle float64
		want int
	}{
		{idle: 140, want: 10}, // never more than the physical cores
		{idle: 0, want: 0},
		{idle: -5, want: 0},
	} {
		snapshot := productionSnapshot()
		snapshot.CPUidlePercent = test.idle
		got := hostObservation(snapshot, configuredCapacity(), productionGuards(), true, domain.Resources{}).Value.Available.CPU
		if got != test.want {
			t.Fatalf("idle %.0f%% advertised %d cores, want %d", test.idle, got, test.want)
		}
	}
}

// TestHostObservationLegacyModeUnchanged proves the static model is preserved
// byte-for-byte when the elastic envelope is off: CPU echoes configuration and
// no physical capacity is advertised.
func TestHostObservationLegacyModeUnchanged(t *testing.T) {
	host := hostObservation(productionSnapshot(), configuredCapacity(), productionGuards(), false, domain.Resources{}).Value
	if host.Available.CPU != configuredCapacity().CPU {
		t.Fatalf("Available.CPU = %d, want the configured %d in legacy mode", host.Available.CPU, configuredCapacity().CPU)
	}
	if host.Capacity != (domain.Resources{}) {
		t.Fatalf("Capacity = %+v, want unobserved in legacy mode", host.Capacity)
	}
}

// TestHostObservationUnobservedPhysicalFactsFallBack proves a probe that could
// not read the machine totals degrades to the configured envelope rather than
// advertising a zero-resource host and closing admission.
func TestHostObservationUnobservedPhysicalFactsFallBack(t *testing.T) {
	snapshot := productionSnapshot()
	snapshot.PhysicalCPU = 0
	snapshot.PhysicalMemoryMB = 0

	host := hostObservation(snapshot, configuredCapacity(), productionGuards(), true, domain.Resources{}).Value
	if host.Capacity.CPU != 0 || host.Capacity.MemoryMB != 0 {
		t.Fatalf("Capacity = %+v, want unobserved dimensions left at zero", host.Capacity)
	}
	if host.Available.CPU != configuredCapacity().CPU {
		t.Fatalf("Available.CPU = %d, want the configured fallback %d", host.Available.CPU, configuredCapacity().CPU)
	}
}

// TestHostObservationBlockedPressureStillFailsClosed proves the elastic model
// does not weaken the pressure guardrails: a host over its limits admits nothing
// regardless of how many cores are idle.
func TestHostObservationBlockedPressureStillFailsClosed(t *testing.T) {
	snapshot := productionSnapshot()
	snapshot.FreeDiskGB = 10 // below the 60 GiB reserve
	observation := hostObservation(snapshot, configuredCapacity(), productionGuards(), true, domain.Resources{})
	if observation.Value.Available != (domain.Resources{}) {
		t.Fatalf("Available = %+v, want empty while pressure blocks admission", observation.Value.Available)
	}
	if observation.Value.Pressure.AdmissionAllowed {
		t.Fatal("admission allowed despite the disk reserve being breached")
	}
	if observation.Reason != "disk reserve" {
		t.Fatalf("reason = %q, want the guardrail reason", observation.Reason)
	}
}
