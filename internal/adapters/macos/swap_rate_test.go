package macos

import (
	"context"
	"testing"
	"time"
)

// TestProbeMeasuresSwapOutRate proves the probe derives the rate from
// consecutive observations and reports the first one as unmeasured.
func TestProbeMeasuresSwapOutRate(t *testing.T) {
	now := time.Unix(1000, 0).UTC()
	outputs := validOutputs()
	runner := &fakeCommands{outputs: outputs}
	probe := &Probe{Runner: runner, Timeout: time.Second, Now: func() time.Time { return now }}

	first := probe.Snapshot(context.Background())
	if first.SwapOutRateObserved {
		t.Fatal("the first observation has no prior sample and must report the rate as unmeasured")
	}
	if first.SwapOuts != 7 {
		t.Fatalf("SwapOuts = %d, want 7", first.SwapOuts)
	}

	// 30 seconds later, 607 total swapouts: 600 pages over 30s = 20/s.
	now = now.Add(30 * time.Second)
	outputs["vm_stat"] = []byte("Mach Virtual Memory Statistics: (page size of 16384 bytes)\n" +
		"Pages free: 100000.\nPages speculative: 10000.\nPages inactive: 200000.\nSwapouts: 607.\n")
	second := probe.Snapshot(context.Background())
	if !second.SwapOutRateObserved {
		t.Fatal("a second observation must measure the rate")
	}
	if second.SwapOutRatePerSecond != 20 {
		t.Fatalf("SwapOutRatePerSecond = %v, want 20", second.SwapOutRatePerSecond)
	}
}

// TestProbeSwapOutRateSurvivesCounterReset proves a reboot, which resets the
// cumulative counter, is reported as unmeasured rather than as a negative rate
// that would read as a quiet host.
func TestProbeSwapOutRateSurvivesCounterReset(t *testing.T) {
	now := time.Unix(1000, 0).UTC()
	outputs := validOutputs()
	probe := &Probe{Runner: &fakeCommands{outputs: outputs}, Timeout: time.Second,
		Now: func() time.Time { return now }}
	probe.Snapshot(context.Background())

	now = now.Add(30 * time.Second)
	outputs["vm_stat"] = []byte("Mach Virtual Memory Statistics: (page size of 16384 bytes)\n" +
		"Pages free: 100000.\nPages speculative: 10000.\nPages inactive: 200000.\nSwapouts: 0.\n")
	after := probe.Snapshot(context.Background())
	if after.SwapOutRateObserved {
		t.Fatalf("a counter reset must be reported as unmeasured, got rate %v", after.SwapOutRatePerSecond)
	}
}
