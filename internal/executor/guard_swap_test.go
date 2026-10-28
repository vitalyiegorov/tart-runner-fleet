package executor

import "testing"

// swapGuard is the production swap configuration: a 2 GiB ceiling.
func swapGuard() Guardrails {
	return Guardrails{MinFreeDiskGB: 60, MinAvailableMemoryMB: 1024, MaxSwapUsedMB: 2048,
		MaxLoadAverage: 9, MinCPUidlePercent: 5}
}

func swapSnapshot(usedMB int64, rate float64, rateObserved bool) HostSnapshot {
	return HostSnapshot{Freshness: Fresh, AvailableMemoryMB: 21_135, FreeDiskGB: 165,
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
	if decision := swapGuard().Evaluate(residue, AdmissionRequest{}); !decision.Allowed {
		t.Fatalf("latched swap residue blocked admission on a host that is not paging: %#v", decision)
	}
}

// TestSwapGuardBlocksActivePaging proves the guardrail still does its job when
// the host really is paging out: that is the condition it exists to catch.
func TestSwapGuardBlocksActivePaging(t *testing.T) {
	paging := swapSnapshot(2134, 25, true)
	decision := swapGuard().Evaluate(paging, AdmissionRequest{})
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
	if decision := swapGuard().Evaluate(unmeasured, AdmissionRequest{}); decision.Allowed {
		t.Fatalf("an unmeasured swap rate must fail closed on the level: %#v", decision)
	}
}

// TestSwapGuardUnderCeilingNeverBlocks proves the level remains a necessary
// condition: paging while comfortably under the ceiling is normal virtual-memory
// behavior and must not gate admission.
func TestSwapGuardUnderCeilingNeverBlocks(t *testing.T) {
	if decision := swapGuard().Evaluate(swapSnapshot(64, 500, true), AdmissionRequest{}); !decision.Allowed {
		t.Fatalf("paging under the swap ceiling must not block: %#v", decision)
	}
}

// issue451Snapshot is ubuntu-vitalii on 2026-10-28 16:00-16:10Z (v0.1.796):
// 3907 MiB of swap against a 1024 MiB ceiling, 23189 MiB available, and a
// swap-out rate of 0.046 pages/s, about one page every twenty seconds, while
// `vmstat 5` printed so=0. The gate closed on it all day.
func issue451Snapshot() HostSnapshot {
	return HostSnapshot{Freshness: Fresh, AvailableMemoryMB: 23_189, FreeDiskGB: 400,
		SwapUsedMB: 3907, CPUidlePercent: 70, LoadAverage: 6,
		SwapOutRatePerSecond: 0.046, SwapOutRateObserved: true}
}

func issue451Guard() Guardrails {
	return Guardrails{MinFreeDiskGB: 20, MinAvailableMemoryMB: 1024, MaxSwapUsedMB: 1024,
		MaxLoadAverage: 24, MinCPUidlePercent: 5}
}

// TestSwapGuardIgnoresTheKernelsBackgroundTrickle is issue #451. With swap above
// the ceiling, a rate of one page every twenty seconds is the kernel's noise
// floor, not paging. Reading it as pressure flapped the largest amd64 node shut
// for most of a day while 23 GB stayed available.
func TestSwapGuardIgnoresTheKernelsBackgroundTrickle(t *testing.T) {
	if decision := issue451Guard().Evaluate(issue451Snapshot(), AdmissionRequest{}); !decision.Allowed {
		t.Fatalf("a 0.046 pages/s trickle with 23 GB available refused admission: %#v", decision)
	}
}

// TestSwapGuardStillRefusesAThrashingHost proves the threshold does not blind
// the guard. A host paging out thousands of pages a second with its available
// memory nearly gone is the condition the guard exists for, and so is one at
// the threshold exactly.
func TestSwapGuardStillRefusesAThrashingHost(t *testing.T) {
	thrashing := issue451Snapshot()
	thrashing.SwapOutRatePerSecond, thrashing.AvailableMemoryMB = 2_000, 1_500
	at := issue451Snapshot()
	at.SwapOutRatePerSecond = DefaultSwapOutPagesPerSecond
	for name, snapshot := range map[string]HostSnapshot{"thrashing": thrashing, "at the threshold": at} {
		decision := issue451Guard().Evaluate(snapshot, AdmissionRequest{})
		if decision.Allowed || decision.Reason != "swap pressure" {
			t.Fatalf("%s: decision = %#v, want swap pressure", name, decision)
		}
	}
}

// TestSwapGuardThresholdIsConfigurable proves an operator can tighten the
// threshold, and that an unset one is the named default rather than "any page".
func TestSwapGuardThresholdIsConfigurable(t *testing.T) {
	guard := issue451Guard()
	guard.SwapOutPagesPerSecond = 0.01
	if decision := guard.Evaluate(issue451Snapshot(), AdmissionRequest{}); decision.Allowed {
		t.Fatalf("a 0.01 pages/s threshold admitted a 0.046 pages/s host: %#v", decision)
	}
	below := issue451Snapshot()
	below.SwapOutRatePerSecond = DefaultSwapOutPagesPerSecond - 0.001
	if decision := issue451Guard().Evaluate(below, AdmissionRequest{}); !decision.Allowed {
		t.Fatalf("a rate under the default threshold refused admission: %#v", decision)
	}
}

// TestSwapGuardStillFailsClosedOnAnUnmeasuredRate keeps ADR 0018's contract on
// issue #451's host: without a rate the level blocks on its own, however much
// memory is available.
func TestSwapGuardStillFailsClosedOnAnUnmeasuredRate(t *testing.T) {
	unmeasured := issue451Snapshot()
	unmeasured.SwapOutRatePerSecond, unmeasured.SwapOutRateObserved = 0, false
	if decision := issue451Guard().Evaluate(unmeasured, AdmissionRequest{}); decision.Allowed || decision.Reason != "swap pressure" {
		t.Fatalf("an unmeasured rate must fail closed on the level: %#v", decision)
	}
}
