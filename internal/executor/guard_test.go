package executor

import "testing"

func TestGuardrailReasons(t *testing.T) {
	base := HostSnapshot{Freshness: Fresh, AvailableMemoryMB: 8000, FreeDiskGB: 100, SwapUsedMB: 10, CPUidlePercent: 50, LoadAverage: 2}
	guard := Guardrails{MinFreeDiskGB: 60, MinAvailableMemoryMB: 2000, MaxSwapUsedMB: 100, MaxLoadAverage: 8, MinCPUidlePercent: 15}
	tests := []struct {
		name     string
		snapshot HostSnapshot
		request  AdmissionRequest
		reason   string
	}{
		{"invalid memory", base, AdmissionRequest{MemoryMB: -1}, "invalid requested memory"},
		{"disk", func() HostSnapshot { s := base; s.FreeDiskGB = 10; return s }(), AdmissionRequest{}, "disk reserve"},
		{"memory", base, AdmissionRequest{MemoryMB: 7000}, "memory reserve"},
		{"swap", func() HostSnapshot { s := base; s.SwapUsedMB = 101; return s }(), AdmissionRequest{}, "swap pressure"},
		{"cpu", func() HostSnapshot { s := base; s.LoadAverage = 9; s.CPUidlePercent = 10; return s }(), AdmissionRequest{}, "cpu pressure"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if decision := guard.Evaluate(test.snapshot, test.request); decision.Allowed || decision.Reason != test.reason {
				t.Fatalf("unexpected decision: %#v", decision)
			}
		})
	}
}
