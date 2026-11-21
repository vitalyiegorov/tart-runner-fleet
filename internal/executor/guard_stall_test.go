package executor

import "testing"

// issue480Guard is ubuntu-vitalii's deployed swap configuration on 2026-11-19:
// a 1024 MiB ceiling and the default paging threshold.
func issue480Guard() Guardrails {
	return Guardrails{MinFreeDiskGB: 20, MinAvailableMemoryMB: 1024, MaxSwapUsedMB: 1024,
		MaxLoadAverage: 24, MinCPUidlePercent: 5}
}

// issue480Snapshot is ubuntu-vitalii at 2026-11-19 08:56Z (issue #480): swap
// full at 8191 of 8191 MiB, the kernel paging out about 50 pages a second,
// 38402 MiB available, load 13, and `/proc/pressure/memory` reporting
// `some avg10=0.54`. Tasks were stalled on memory 0.54% of the last ten
// seconds: the swap was cold residue, not pressure, and the gate closed on it.
func issue480Snapshot() HostSnapshot {
	return HostSnapshot{Freshness: Fresh, AvailableMemoryMB: 38_402, FreeDiskGB: 400,
		SwapUsedMB: 8191, CPUidlePercent: 45, LoadAverage: 13,
		SwapOutRatePerSecond: 50, SwapOutRateObserved: true,
		MemoryStallPercent: 0.54, MemoryStallObserved: true}
}

func assertSwapDecision(t *testing.T, name string, guard Guardrails, snapshot HostSnapshot, allowed bool) {
	t.Helper()
	decision := guard.Evaluate(snapshot, AdmissionRequest{})
	if decision.Allowed != allowed {
		t.Fatalf("%s: decision = %#v, want allowed=%v", name, decision, allowed)
	}
	if !allowed && decision.Reason != "swap pressure" {
		t.Fatalf("%s: reason = %q, want swap pressure", name, decision.Reason)
	}
}

// TestSwapGuardAdmitsColdResidueTheKernelIsNotStalledOn is issue #480. Swap
// over the ceiling and a swap-out rate over the threshold are both true of a
// host whose swap is full of pages nobody touches: the kernel keeps rotating
// them. Memory stall is what says whether any task is waiting on that, and at
// 0.54% none materially is.
func TestSwapGuardAdmitsColdResidueTheKernelIsNotStalledOn(t *testing.T) {
	assertSwapDecision(t, "2026-11-19 sample", issue480Guard(), issue480Snapshot(), true)
}

// TestSwapGuardAdmitsColdResidueBeforeTheRateIsMeasured is the first tick after
// a daemon restart on the same host: the rate needs two samples and has none,
// but the stall is a single reading and is already low. A positively measured
// low stall is evidence the level alone is residue (ADR 0102 narrows ADR
// 0018's fail-closed contract to exactly this case).
func TestSwapGuardAdmitsColdResidueBeforeTheRateIsMeasured(t *testing.T) {
	first := issue480Snapshot()
	first.SwapOutRatePerSecond, first.SwapOutRateObserved = 0, false
	assertSwapDecision(t, "unmeasured rate, low stall", issue480Guard(), first, true)
}

// TestSwapGuardStillRefusesWhatStallCorroborates walks the cases that must stay
// closed: real thrash with a high stall, a stall at the threshold exactly, a
// high stall before the rate is measured, and every host whose stall is not
// observed, which keeps ADR 0089's rule byte for byte (macOS, and Linux without
// PSI).
func TestSwapGuardStillRefusesWhatStallCorroborates(t *testing.T) {
	omarchyThrash := issue480Snapshot()
	omarchyThrash.SwapOutRatePerSecond, omarchyThrash.MemoryStallPercent = 15_000, 40
	atThreshold := issue480Snapshot()
	atThreshold.MemoryStallPercent = DefaultMemoryStallPercent
	unmeasuredStalled := issue480Snapshot()
	unmeasuredStalled.SwapOutRateObserved, unmeasuredStalled.MemoryStallPercent = false, 40
	macBook := issue480Snapshot()
	macBook.SwapOutRatePerSecond, macBook.MemoryStallPercent, macBook.MemoryStallObserved = 5_700, 0, false
	noPSI := issue480Snapshot()
	noPSI.MemoryStallPercent, noPSI.MemoryStallObserved = 0, false
	noPSIUnmeasured := noPSI
	noPSIUnmeasured.SwapOutRateObserved = false
	for name, snapshot := range map[string]HostSnapshot{
		"Omarchy thrash, 15000 pages/s at stall 40": omarchyThrash,
		"stall at the threshold":                    atThreshold,
		"unmeasured rate, high stall":               unmeasuredStalled,
		"MacBook thrash, 5700 pages/s, no PSI":      macBook,
		"Linux without PSI, 50 pages/s":             noPSI,
		"Linux without PSI, unmeasured rate":        noPSIUnmeasured,
	} {
		assertSwapDecision(t, name, issue480Guard(), snapshot, false)
	}
}

// TestSwapGuardStallNeverOpensWhatTheRateAlreadyOpens and never closes what the
// level does not: the stall only corroborates swap pressure, it is not a gate of
// its own (ADR 0102 defers PSI-alone gating).
func TestSwapGuardStallNeverOpensWhatTheRateAlreadyOpens(t *testing.T) {
	quiet := issue480Snapshot()
	quiet.SwapUsedMB, quiet.SwapOutRatePerSecond, quiet.MemoryStallPercent = 46*1024, 0, 0
	assertSwapDecision(t, "46 GiB of swap at rate 0", issue480Guard(), quiet, true)
	underCeiling := issue480Snapshot()
	underCeiling.SwapUsedMB, underCeiling.MemoryStallPercent = 512, 80
	assertSwapDecision(t, "under the ceiling at stall 80", issue480Guard(), underCeiling, true)
}

// TestSwapGuardStallThresholdIsConfigurable proves an operator can tighten the
// threshold below the 2026-11-19 reading, and that an unset one is the default.
func TestSwapGuardStallThresholdIsConfigurable(t *testing.T) {
	guard := issue480Guard()
	guard.MemoryStallPercent = 0.5
	assertSwapDecision(t, "threshold 0.5 against stall 0.54", guard, issue480Snapshot(), false)
	below := issue480Snapshot()
	below.MemoryStallPercent = DefaultMemoryStallPercent - 0.01
	assertSwapDecision(t, "stall just under the default", issue480Guard(), below, true)
}

// omarchyTrainingSnapshot is Omarchy at 2026-11-19 10:26Z: a 7.8 GB process
// training a model, swap 6321 MiB against a 2048 MiB ceiling, 9563 pages/s
// paged out, 12077 MiB available, load 8.8, and PSI memory `some avg10=1.67`
// (`full avg10=0.98`). Heavy, real swap-out beside a low stall: the stall
// alone would have admitted it.
func omarchyTrainingSnapshot() HostSnapshot {
	return HostSnapshot{Freshness: Fresh, AvailableMemoryMB: 12_077, FreeDiskGB: 400,
		SwapUsedMB: 6321, CPUidlePercent: 60, LoadAverage: 8.8,
		SwapOutRatePerSecond: 9563, SwapOutRateObserved: true,
		MemoryStallPercent: 1.67, MemoryStallObserved: true}
}

func omarchyGuard() Guardrails {
	return Guardrails{MinFreeDiskGB: 20, MinAvailableMemoryMB: 1024, MaxSwapUsedMB: 2048,
		MaxLoadAverage: 24, MinCPUidlePercent: 5}
}

// TestSwapGuardALowStallNeverOverridesAHardRate is the correction to ADR 0102's
// first draft. A stall may only exempt the trickle band between
// swapOutPagesPerSecond and swapOutHardPagesPerSecond; at or above the hard
// rate the host is paging out in earnest whatever the stall reads.
func TestSwapGuardALowStallNeverOverridesAHardRate(t *testing.T) {
	assertSwapDecision(t, "Omarchy training, 9563 pages/s at stall 1.67", omarchyGuard(), omarchyTrainingSnapshot(), false)
	atHard := issue480Snapshot()
	atHard.SwapOutRatePerSecond = DefaultSwapOutHardPagesPerSecond
	assertSwapDecision(t, "at the hard rate exactly, stall 0.54", issue480Guard(), atHard, false)
	unobserved := issue480Snapshot()
	unobserved.SwapOutRatePerSecond, unobserved.MemoryStallPercent, unobserved.MemoryStallObserved = 1500, 0, false
	assertSwapDecision(t, "1500 pages/s, stall unobserved", issue480Guard(), unobserved, false)
}

// TestSwapGuardStallDecidesTheTrickleBand: between the paging threshold and the
// hard rate, the stall decides.
func TestSwapGuardStallDecidesTheTrickleBand(t *testing.T) {
	band := issue480Snapshot()
	band.SwapOutRatePerSecond = 500
	assertSwapDecision(t, "500 pages/s at stall 0.54", issue480Guard(), band, true)
	band.MemoryStallPercent = 15
	assertSwapDecision(t, "500 pages/s at stall 15", issue480Guard(), band, false)
}

// TestSwapGuardHardRateIsConfigurable proves an operator can raise the hard
// rate so a low stall exempts more of the band, and that zero is the default.
func TestSwapGuardHardRateIsConfigurable(t *testing.T) {
	guard := omarchyGuard()
	guard.SwapOutHardPagesPerSecond = 10_000
	assertSwapDecision(t, "9563 pages/s under a 10000 hard rate, stall 1.67", guard, omarchyTrainingSnapshot(), true)
}
