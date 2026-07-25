package macos

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestProbeReportsPhysicalCapacity covers the facts second-pilot admission needs
// that the probe never reported: how many cores and how much RAM the machine
// actually has. Without them the scheduler's CPU bound can only echo
// configuration, so host CPU consumption can never shrink the envelope and the
// fleet can never expand into an idle host.
func TestProbeReportsPhysicalCapacity(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	probe := &Probe{Runner: &fakeCommands{outputs: validOutputs()}, Timeout: time.Second,
		Now: func() time.Time { return now }}

	snapshot := probe.Snapshot(context.Background())
	if snapshot.Freshness != Fresh {
		t.Fatalf("freshness = %s, want fresh", snapshot.Freshness)
	}
	if snapshot.PhysicalCPU != 10 {
		t.Fatalf("PhysicalCPU = %d, want 10 from hw.ncpu", snapshot.PhysicalCPU)
	}
	if snapshot.PhysicalMemoryMB != 24_576 {
		t.Fatalf("PhysicalMemoryMB = %d, want 24576 from hw.memsize", snapshot.PhysicalMemoryMB)
	}
}

// TestProbePhysicalCapacityFailsSafe proves an unreadable physical fact never
// fails the whole host observation and never reports a fake zero-core machine as
// a measurement. Zero means "not observed" and consumers must fall back to
// configuration rather than close admission.
func TestProbePhysicalCapacityFailsSafe(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	runner := &fakeCommands{outputs: validOutputs(),
		errors: map[string]error{"sysctl:hw.ncpu": errors.New("unavailable")}}
	probe := &Probe{Runner: runner, Timeout: time.Second, Now: func() time.Time { return now }}

	snapshot := probe.Snapshot(context.Background())
	if snapshot.Freshness != Fresh {
		t.Fatalf("freshness = %s: an unreadable physical core count must not degrade the observation", snapshot.Freshness)
	}
	if snapshot.PhysicalCPU != 0 {
		t.Fatalf("PhysicalCPU = %d, want 0 to signal not-observed", snapshot.PhysicalCPU)
	}
	// The hard memory and disk signals are still intact.
	if snapshot.AvailableMemoryMB == 0 || snapshot.FreeDiskGB == 0 {
		t.Fatalf("hard signals lost alongside the advisory one: %#v", snapshot)
	}
}

// TestProbeRejectsNonsensePhysicalValues proves malformed sysctl output is
// treated as not-observed rather than parsed into an absurd machine.
func TestProbeRejectsNonsensePhysicalValues(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	outputs := validOutputs()
	outputs["hw.ncpu"] = []byte("not-a-number\n")
	probe := &Probe{Runner: &fakeCommands{outputs: outputs}, Timeout: time.Second,
		Now: func() time.Time { return now }}

	if snapshot := probe.Snapshot(context.Background()); snapshot.PhysicalCPU != 0 {
		t.Fatalf("PhysicalCPU = %d, want 0 for unparseable output", snapshot.PhysicalCPU)
	}
}
