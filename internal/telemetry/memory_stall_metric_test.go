package telemetry

import (
	"math"
	"strings"
	"testing"
)

// issue480PressureMetric is ubuntu-vitalii at 2026-11-19 08:56Z once the stall
// corroborates the swap guard: swap full, 50 pages/s, stall 0.54%, admitted.
func issue480PressureMetric() HostPressureMetric {
	return HostPressureMetric{AvailableMemoryMiB: 38_402, FreeDiskGiB: 400, SwapUsedMiB: 8191,
		SwapOuts: 9_000_000, SwapOutRatePerSecond: 50, SwapOutRateObserved: true,
		MemoryStallPercent: 0.54, MemoryStallObserved: true,
		CPUIdlePercent: 45, LoadAverage: 13, AdmissionAllowed: true, AdmissionReason: "capacity available"}
}

// TestMetricsPublishTheMemoryStall proves automation can reproduce a swap
// decision that the level and rate alone no longer decide (ADR 0102), and that
// the status document carries the same evidence.
func TestMetricsPublishTheMemoryStall(t *testing.T) {
	rendered := swapRateMetrics(t, issue480PressureMetric())
	for _, line := range []string{
		"fleet_host_swapout_rate_pages_per_second 50\n",
		"fleet_host_memory_stall_percent 0.54\n",
		"fleet_host_memory_stall_observed 1\n",
		"fleet_host_admission_allowed 1\n",
	} {
		if !strings.Contains(rendered, line) {
			t.Fatalf("metrics missing %q:\n%s", line, rendered)
		}
	}
	health, _ := newTestHealth(t)
	if err := health.SetHostPressure(issue480PressureMetric()); err != nil {
		t.Fatal(err)
	}
	ok := HealthResult{OK: true}
	status := statusEnvelope(health.Snapshot(), "v", "authority", ok, ok, ok, ok, ok, ok, ok, ok, ok).Data.HostPressure
	if status.MemoryStallPercent != 0.54 || !status.MemoryStallObserved {
		t.Fatalf("status host pressure lost the stall: %+v", status)
	}
}

// TestMetricsMarkAnUnobservedMemoryStall: a host that reports no stall
// publishes 0 beside an observed gauge of 0, never a measured quiet host.
func TestMetricsMarkAnUnobservedMemoryStall(t *testing.T) {
	metric := issue480PressureMetric()
	metric.MemoryStallPercent, metric.MemoryStallObserved = 0, false
	metric.AdmissionAllowed, metric.AdmissionReason = false, "swap pressure"
	rendered := swapRateMetrics(t, metric)
	for _, line := range []string{"fleet_host_memory_stall_percent 0\n", "fleet_host_memory_stall_observed 0\n"} {
		if !strings.Contains(rendered, line) {
			t.Fatalf("metrics missing %q:\n%s", line, rendered)
		}
	}
}

// TestHostPressureRejectsAnImpossibleMemoryStall keeps the metric bounded: a
// stall is a share of time, so it is a finite percentage.
func TestHostPressureRejectsAnImpossibleMemoryStall(t *testing.T) {
	health, _ := newTestHealth(t)
	for _, stall := range []float64{-0.01, 100.01, math.NaN(), math.Inf(1)} {
		metric := issue480PressureMetric()
		metric.MemoryStallPercent = stall
		if err := health.SetHostPressure(metric); err == nil {
			t.Fatalf("memory stall %v accepted", stall)
		}
	}
	if health.Snapshot().HostPressure.SwapUsedMiB != 0 {
		t.Fatal("a rejected metric must not be published")
	}
}
