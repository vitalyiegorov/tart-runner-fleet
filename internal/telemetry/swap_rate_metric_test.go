package telemetry

import (
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
)

// nodeCPressureMetric is node C's 2026-08-04 observation (issue #154): swap far
// over its ceiling with a measured zero page-out rate, which is why admission
// was correctly allowed.
func nodeCPressureMetric() HostPressureMetric {
	return HostPressureMetric{AvailableMemoryMiB: 11_878, FreeDiskGiB: 99, SwapUsedMiB: 13_593,
		SwapOuts: 3_855_619, SwapOutRatePerSecond: 0, SwapOutRateObserved: true,
		CPUIdlePercent: 79.3, LoadAverage: 3.69, AdmissionAllowed: true, AdmissionReason: "capacity available"}
}

func swapRateMetrics(t *testing.T, metric HostPressureMetric) string {
	t.Helper()
	health, _ := newTestHealth(t)
	if err := health.SetHostPressure(metric); err != nil {
		t.Fatalf("set host pressure: %v", err)
	}
	server, err := NewServer(health, ServerConfig{ControllerVersion: "v0.1.359", ControllerMode: "observe"})
	if err != nil {
		t.Fatal(err)
	}
	response := request(t, server, http.MethodGet, "/metrics")
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read metrics: %v", err)
	}
	return string(body)
}

// TestMetricsPublishTheSwapOutRate proves automation can reproduce the swap
// decision. fleet_host_swapouts_total is cumulative and a single scrape cannot
// difference it, so the rate the guardrail actually compares must be its own
// gauge next to the level.
func TestMetricsPublishTheSwapOutRate(t *testing.T) {
	rendered := swapRateMetrics(t, nodeCPressureMetric())
	for _, line := range []string{
		"fleet_host_swap_used_mib 13593\n",
		"fleet_host_swapouts_total 3855619\n",
		"fleet_host_swapout_rate_pages_per_second 0\n",
		"fleet_host_swapout_rate_observed 1\n",
		"fleet_host_admission_allowed 1\n",
	} {
		if !strings.Contains(rendered, line) {
			t.Fatalf("metrics missing %q:\n%s", line, rendered)
		}
	}
}

// TestMetricsMarkAnUnmeasuredSwapOutRate proves the fail-closed case is not
// published as a quiet host: the rate gauge stays zero, so the observed gauge is
// the only thing separating "measured no paging" from "could not measure".
func TestMetricsMarkAnUnmeasuredSwapOutRate(t *testing.T) {
	metric := nodeCPressureMetric()
	metric.SwapOutRateObserved = false
	metric.AdmissionAllowed = false
	metric.AdmissionReason = "swap pressure"
	rendered := swapRateMetrics(t, metric)
	for _, line := range []string{"fleet_host_swapout_rate_observed 0\n", "fleet_host_admission_allowed 0\n"} {
		if !strings.Contains(rendered, line) {
			t.Fatalf("metrics missing %q:\n%s", line, rendered)
		}
	}
}

// TestMetricsRenderAMeasuredSwapOutRate proves a fractional rate survives the
// gauge formatting rather than truncating to the zero it must never look like.
func TestMetricsRenderAMeasuredSwapOutRate(t *testing.T) {
	metric := nodeCPressureMetric()
	metric.SwapOutRatePerSecond = 0.25
	if rendered := swapRateMetrics(t, metric); !strings.Contains(rendered, "fleet_host_swapout_rate_pages_per_second 0.25\n") {
		t.Fatalf("fractional swap-out rate lost:\n%s", rendered)
	}
}

// TestHostPressureRejectsAnImpossibleSwapOutRate keeps the metric bounded. A
// page-out rate cannot be negative or non-finite, and a rejected metric leaves
// the last coherent snapshot in place rather than publishing nonsense.
func TestHostPressureRejectsAnImpossibleSwapOutRate(t *testing.T) {
	health, _ := newTestHealth(t)
	for _, rate := range []float64{-1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		metric := nodeCPressureMetric()
		metric.SwapOutRatePerSecond = rate
		if err := health.SetHostPressure(metric); err == nil {
			t.Fatalf("swap-out rate %v accepted", rate)
		}
	}
	if health.Snapshot().HostPressure.SwapUsedMiB != 0 {
		t.Fatal("a rejected metric must not be published")
	}
}
