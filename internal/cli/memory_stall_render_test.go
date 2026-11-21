package cli

import (
	"strings"
	"testing"
)

// TestStatusShowsTheMemoryStallBesidePaging is issue #480's operator half. A
// host over its swap ceiling and paging out at 50/s is admitted because no task
// is waiting on memory; without the stall on the line, that verdict reads as a
// guard that did not fire.
func TestStatusShowsTheMemoryStallBesidePaging(t *testing.T) {
	pressure := nodeCPressure()
	pressure.SwapUsedMiB, pressure.SwapOutRatePerSecond = 8191, 50
	pressure.MemoryStallPercent, pressure.MemoryStallObserved = 0.54, true
	if rendered := renderNodeC(pressure); !strings.Contains(rendered, "swap 8191 MiB (paging 50/s, stall 0.54%)") {
		t.Fatalf("status does not show the stall the swap guard corroborates with:\n%s", rendered)
	}
	pressure.SwapOutRateObserved = false
	if rendered := renderNodeC(pressure); !strings.Contains(rendered, "swap 8191 MiB (paging unmeasured, stall 0.54%)") {
		t.Fatalf("an unmeasured rate beside an observed stall lost either fact:\n%s", rendered)
	}
}
