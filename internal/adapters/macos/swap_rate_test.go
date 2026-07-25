package macos

import (
	"context"
	"testing"
	"time"
)

// swapGuard is the production swap configuration: a 2 GiB ceiling.
func swapGuard() Guardrails {
	return Guardrails{MinFreeDiskGB: 60, MinAvailableMemoryMB: 1024, MaxSwapUsedMB: 2048,
		MaxLoadAverage: 9, MinCPUidlePercent: 5}
}

func swapSnapshot(usedMB int64, rate float64, rateObserved bool) Snapshot {
	return Snapshot{Freshness: Fresh, AvailableMemoryMB: 21_135, FreeDiskGB: 165,
		SwapUsedMB: usedMB, CPUidlePercent: 80, LoadAverage: 2.3,
		SwapOutRatePerSecond: rate, SwapOutRateObserved: rateObserved}
}

// TestSwapGuardIgnoresLatchedResidue is the defect. macOS does not eagerly
// reclaim swap, so "swap used" is closer to a high-water mark than a current
// pressure reading: once a burst has paged out, the level stays high long after
// the pressure ended. Gating admission on the level alone therefore latches the
// whole fleet off a healthy host indefinitely.
//
// Observed on the production Mac mini on 2026-07-25: swap used 2134 MiB against
// a 2048 MiB ceiling, so fleet_host_admission_allowed was 0 and nineteen jobs
// queued behind it -- while the host was 80% idle with 21 GiB available, memory
// pressure reported 86% free, and a 60-second sample measured ZERO swapouts. The
// machine was not paging at all; it was carrying residue from an earlier burst.
func TestSwapGuardIgnoresLatchedResidue(t *testing.T) {
	// Over the ceiling, but measurably not paging.
	residue := swapSnapshot(2134, 0, true)
	if decision := swapGuard().Evaluate(residue, Request{}); !decision.Allowed {
		t.Fatalf("latched swap residue blocked admission on a host that is not paging: %#v", decision)
	}
}

// TestSwapGuardBlocksActivePaging proves the guardrail still does its job when
// the host really is paging out: that is the condition it exists to catch.
func TestSwapGuardBlocksActivePaging(t *testing.T) {
	paging := swapSnapshot(2134, 25, true)
	decision := swapGuard().Evaluate(paging, Request{})
	if decision.Allowed {
		t.Fatal("an actively paging host over its swap ceiling must not admit work")
	}
	if decision.Reason != "swap pressure" {
		t.Fatalf("reason = %q, want swap pressure", decision.Reason)
	}
}

// TestSwapGuardFailsClosedWithoutARateSample proves the repair does not weaken
// the fail-closed contract. A rate needs two samples; on the very first
// observation after a daemon start there is no prior, so the level is the only
// evidence available and must still block. An unmeasured rate must never be
// read as a quiet host.
func TestSwapGuardFailsClosedWithoutARateSample(t *testing.T) {
	unmeasured := swapSnapshot(2134, 0, false)
	if decision := swapGuard().Evaluate(unmeasured, Request{}); decision.Allowed {
		t.Fatalf("an unmeasured swap rate must fail closed on the level: %#v", decision)
	}
}

// TestSwapGuardUnderCeilingNeverBlocks proves the level remains a necessary
// condition: paging while comfortably under the ceiling is normal virtual-memory
// behavior and must not gate admission.
func TestSwapGuardUnderCeilingNeverBlocks(t *testing.T) {
	if decision := swapGuard().Evaluate(swapSnapshot(64, 500, true), Request{}); !decision.Allowed {
		t.Fatalf("paging under the swap ceiling must not block: %#v", decision)
	}
}

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
