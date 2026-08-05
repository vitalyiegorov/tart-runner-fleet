package adminapi

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestHostPressurePublishesThePageOutRate is the API half of issue #154. Since
// ADR 0018 the swap guardrail refuses admission only when the level exceeds the
// ceiling AND the host is measurably paging out, so the page-out rate is the
// deciding fact. `fleet.v1` publishes the level and the cumulative counter but
// not the rate, which leaves automation unable to reproduce the decision it is
// reading -- and a cumulative counter cannot be differenced from a single
// document.
func TestHostPressurePublishesThePageOutRate(t *testing.T) {
	encoded, err := json.Marshal(HostPressure{AvailableMemoryMiB: 11_878, FreeDiskGiB: 99,
		SwapUsedMiB: 13_593, SwapOuts: 3_855_619, CPUIdlePercent: 79.3, LoadAverage: 3.69,
		AdmissionAllowed: true, AdmissionReason: "capacity available"})
	if err != nil {
		t.Fatalf("marshal host pressure: %v", err)
	}
	document := string(encoded)
	for _, field := range []string{`"swapUsedMiB"`, `"swapOuts"`, `"swapOutRatePerSecond"`, `"swapOutRateObserved"`} {
		if !strings.Contains(document, field) {
			t.Fatalf("fleet.v1 host pressure omits %s: %s", field, document)
		}
	}
}
