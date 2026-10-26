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

// TestInodeReserveRefusesAFilesystemOutOfInodes is issue #423: free bytes say
// nothing about whether a file can be created. A filesystem with 29 GiB free
// and no inode left fails every guest with ENOSPC, so it must be refused as
// surely as one out of bytes.
func TestInodeReserveRefusesAFilesystemOutOfInodes(t *testing.T) {
	guard := Guardrails{MinFreeDiskGB: 20, MinAvailableMemoryMB: 1024, MinFreeInodePercent: 5}
	healthy := HostSnapshot{Freshness: Fresh, AvailableMemoryMB: 8000, FreeDiskGB: 29, CPUidlePercent: 50,
		FreeInodes: 30_000_000, TotalInodes: 60_850_176}
	tests := []struct {
		name    string
		free    int64
		total   int64
		allowed bool
	}{
		{"exhausted", 0, 60_850_176, false},
		{"just under the floor", 3_042_508, 60_850_176, false},
		{"exactly at the floor", 3_042_509, 60_850_176, true},
		{"ample", 30_000_000, 60_850_176, true},
		// btrfs and other dynamically allocating filesystems report no inode
		// totals: there is no fixed limit to run out of, so the floor does not
		// apply, and treating it as zero free would brick the node.
		{"no fixed inode limit", 0, 0, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := healthy
			snapshot.FreeInodes, snapshot.TotalInodes = test.free, test.total
			decision := guard.Evaluate(snapshot, AdmissionRequest{})
			if decision.Allowed != test.allowed {
				t.Fatalf("allowed=%v reason=%q, want allowed=%v", decision.Allowed, decision.Reason, test.allowed)
			}
			if !test.allowed && decision.Reason != "inode reserve" {
				t.Fatalf("reason = %q, want inode reserve", decision.Reason)
			}
		})
	}
	// The byte floor keeps precedence: a node out of both is named by the guard
	// that has always named it, so existing runbooks still read it.
	both := healthy
	both.FreeDiskGB, both.FreeInodes = 1, 0
	if decision := guard.Evaluate(both, AdmissionRequest{}); decision.Reason != "disk reserve" {
		t.Fatalf("reason = %q, want disk reserve", decision.Reason)
	}
	// A floor the operator never set refuses nothing.
	unset := guard
	unset.MinFreeInodePercent = 0
	exhausted := healthy
	exhausted.FreeInodes = 0
	if decision := unset.Evaluate(exhausted, AdmissionRequest{}); !decision.Allowed {
		t.Fatalf("an unset inode floor refused admission: %#v", decision)
	}
}
