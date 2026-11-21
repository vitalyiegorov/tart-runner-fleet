package linux

import (
	"context"
	"testing"
	"testing/fstest"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/executor"
)

// memoryPressureBody is ubuntu-vitalii's `/proc/pressure/memory` at
// 2026-11-19 09:03Z, with swap full of cold pages (issue #480).
const memoryPressureBody = `some avg10=0.54 avg60=0.61 avg300=0.48 total=912837465
full avg10=0.21 avg60=0.25 avg300=0.19 total=401928374
`

// TestMemoryStallIsReadFromTheSomeLine proves the probe publishes the share of
// the last ten seconds in which at least one task waited on memory: the `some`
// line's avg10, not `full` and not a longer window.
func TestMemoryStallIsReadFromTheSomeLine(t *testing.T) {
	files := fixture()
	files[memoryPressurePath] = &fstest.MapFile{Data: []byte(memoryPressureBody)}
	snapshot := newProbe(files, &diskStub{body: dfBody}, nil).Snapshot(context.Background())
	if snapshot.Freshness != executor.Fresh {
		t.Fatalf("freshness = %q", snapshot.Freshness)
	}
	if !snapshot.MemoryStallObserved || snapshot.MemoryStallPercent != 0.54 {
		t.Fatalf("stall = %v observed=%v, want 0.54 observed", snapshot.MemoryStallPercent, snapshot.MemoryStallObserved)
	}
}

// TestAnUnreadableMemoryStallIsUnobservedNeverDegrading is the advisory half:
// a kernel without PSI (CONFIG_PSI off, or `psi=0`), a missing file, or content
// the probe cannot read reports the stall as unobserved. The observation stays
// fresh, because the swap guard then falls back to ADR 0089's rule exactly,
// and it never carries a stale stall forward as if it were measured now.
func TestAnUnreadableMemoryStallIsUnobservedNeverDegrading(t *testing.T) {
	for name, body := range map[string]*string{
		"missing file":      nil,
		"empty":             ptr(""),
		"no some line":      ptr("full avg10=0.21 avg60=0.25 avg300=0.19 total=4\n"),
		"no avg10":          ptr("some avg60=0.61 avg300=0.48 total=9\n"),
		"non-numeric avg10": ptr("some avg10=lots avg60=0.61 avg300=0.48 total=9\n"),
		"negative avg10":    ptr("some avg10=-1.00 avg60=0.61 avg300=0.48 total=9\n"),
		"avg10 over 100":    ptr("some avg10=100.01 avg60=0.61 avg300=0.48 total=9\n"),
		"NaN avg10":         ptr("some avg10=NaN avg60=0.61 avg300=0.48 total=9\n"),
	} {
		t.Run(name, func(t *testing.T) {
			files := fixture()
			probe := newProbe(files, &diskStub{body: dfBody}, nil)
			files[memoryPressurePath] = &fstest.MapFile{Data: []byte(memoryPressureBody)}
			if measured := probe.Snapshot(context.Background()); !measured.MemoryStallObserved {
				t.Fatal("the good reading was not observed")
			}
			delete(files, memoryPressurePath)
			if body != nil {
				files[memoryPressurePath] = &fstest.MapFile{Data: []byte(*body)}
			}
			snapshot := probe.Snapshot(context.Background())
			if snapshot.Freshness != executor.Fresh || snapshot.Cause != nil {
				t.Fatalf("an unreadable stall degraded the observation: %q %v", snapshot.Freshness, snapshot.Cause)
			}
			if snapshot.MemoryStallObserved || snapshot.MemoryStallPercent != 0 {
				t.Fatalf("stall = %v observed=%v, want unobserved", snapshot.MemoryStallPercent, snapshot.MemoryStallObserved)
			}
		})
	}
}

func ptr(value string) *string { return &value }
