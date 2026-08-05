package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
)

// nodeCPressure is what node C (Mac Studio, 14 cores, 36 GiB) reported on
// 2026-08-04 in observe mode, filed as issue #154: swap 6.6x the configured
// 2048 MiB ceiling and admission allowed. That is the correct decision --
// ADR 0018 makes the swap guard a level AND rate gate, and two consecutive
// samples measured zero page-outs -- but nothing on this line says so.
func nodeCPressure() adminapi.HostPressure {
	return adminapi.HostPressure{AvailableMemoryMiB: 11_878, FreeDiskGiB: 99, SwapUsedMiB: 13_593,
		SwapOuts: 3_855_619, SwapOutRatePerSecond: 0, SwapOutRateObserved: true,
		CPUIdlePercent: 79.3, LoadAverage: 3.69,
		AdmissionAllowed: true, AdmissionReason: "capacity available"}
}

func renderNodeC(pressure adminapi.HostPressure) string {
	status := healthyStatus()
	status.Data.HostPressure = pressure
	buffer := &bytes.Buffer{}
	renderStatus(buffer, status)
	return buffer.String()
}

// TestStatusShowsWhySwapResidueStillAdmits is the defect. Printing the swap
// LEVEL beside a verdict the level contradicts, with no sign of the page-out
// rate the verdict was actually made on, tells an operator that a 2048 MiB
// ceiling is being ignored at 13593 MiB. It is not: the host is not paging.
// The status line must carry that evidence, because it is the only thing that
// reconciles the two numbers printed next to each other.
func TestStatusShowsWhySwapResidueStillAdmits(t *testing.T) {
	rendered := renderNodeC(nodeCPressure())
	if !strings.Contains(rendered, "swap 13593 MiB") {
		t.Fatalf("swap level missing from status:\n%s", rendered)
	}
	if !strings.Contains(rendered, "swap 13593 MiB (paging 0/s)") {
		t.Fatalf("status does not show the page-out rate the swap guard decides on:\n%s", rendered)
	}
}

// TestStatusMarksAnUnmeasuredPagingRate proves the other half is legible too. A
// rate needs two samples; until there is a second one the guard fails closed on
// the level alone, and an operator must be able to tell that state from a
// measured quiet host rather than reading a zero that was never measured.
func TestStatusMarksAnUnmeasuredPagingRate(t *testing.T) {
	pressure := nodeCPressure()
	pressure.SwapOutRateObserved = false
	pressure.AdmissionAllowed = false
	pressure.AdmissionReason = "swap pressure"
	rendered := renderNodeC(pressure)
	if !strings.Contains(rendered, "swap 13593 MiB (paging unmeasured)") {
		t.Fatalf("an unmeasured page-out rate must not render as a quiet host:\n%s", rendered)
	}
	if !strings.Contains(rendered, "admission deferred (swap pressure)") {
		t.Fatalf("deferred admission missing from status:\n%s", rendered)
	}
}

// TestStatusRendersAMeasuredPagingRate proves a host that really is paging shows
// the figure, so the same line explains a refusal as well as an admission.
func TestStatusRendersAMeasuredPagingRate(t *testing.T) {
	pressure := nodeCPressure()
	pressure.SwapOutRatePerSecond = 25.5
	pressure.AdmissionAllowed = false
	pressure.AdmissionReason = "swap pressure"
	if rendered := renderNodeC(pressure); !strings.Contains(rendered, "swap 13593 MiB (paging 25.5/s)") {
		t.Fatalf("measured page-out rate missing from status:\n%s", rendered)
	}
}
