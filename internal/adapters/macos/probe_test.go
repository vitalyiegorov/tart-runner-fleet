package macos

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeCommands struct {
	mu      sync.Mutex
	outputs map[string][]byte
	err     error
	errors  map[string]error
}

func (f *fakeCommands) Run(_ context.Context, binary string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	key := binary
	if len(args) > 0 {
		key += ":" + args[len(args)-1]
	}
	if err := f.errors[key]; err != nil {
		return nil, err
	}
	if binary == "sysctl" && len(args) > 0 && args[len(args)-1] == "vm.loadavg" {
		return []byte("{ 2.0 1.5 1.0 }"), nil
	}
	return f.outputs[binary], nil
}

func validOutputs() map[string][]byte {
	return map[string][]byte{
		"vm_stat": []byte("Mach Virtual Memory Statistics: (page size of 16384 bytes)\nPages free: 100000.\nPages speculative: 10000.\nPages inactive: 200000.\nSwapouts: 7.\n"),
		"df":      []byte("Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/disk 100000000 1000 83886080 1% /\n"),
		"sysctl":  []byte("total = 4096.00M  used = 512.00M  free = 3584.00M\n"),
		"top":     []byte("CPU usage: 5.0% user, 5.0% sys, 90.0% idle\n"),
	}
}

func TestProbeFreshStaleUnavailableAndGuardrails(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	outputs := validOutputs()
	runner := &fakeCommands{outputs: outputs}
	probe := &Probe{Runner: runner, Timeout: time.Second, Now: func() time.Time { return now }}
	snapshot := probe.Snapshot(context.Background())
	if snapshot.Freshness != Fresh || snapshot.AvailableMemoryMB != 4843 || snapshot.FreeDiskGB != 80 || snapshot.SwapUsedMB != 512 || snapshot.SwapOuts != 7 || snapshot.CPUidlePercent != 90 {
		t.Fatalf("unexpected snapshot: %#v", snapshot)
	}
	guard := Guardrails{MinFreeDiskGB: 60, MinAvailableMemoryMB: 2000, MaxSwapUsedMB: 1024, MaxLoadAverage: 8, MinCPUidlePercent: 15}
	if decision := guard.Evaluate(snapshot, Request{MemoryMB: 2000}); !decision.Allowed {
		t.Fatalf("healthy host rejected: %#v", decision)
	}
	runner.err = errors.New("host unavailable")
	stale := probe.Snapshot(context.Background())
	if stale.Freshness != Stale || stale.Cause == nil {
		t.Fatalf("expected stale cache: %#v", stale)
	}
	if guard.Evaluate(stale, Request{}).Allowed {
		t.Fatal("stale observation allowed")
	}
	empty := (&Probe{Runner: runner, Now: func() time.Time { return now }}).Snapshot(context.Background())
	if empty.Freshness != Unavailable {
		t.Fatalf("expected unavailable: %#v", empty)
	}
}

func TestGuardrailReasons(t *testing.T) {
	base := Snapshot{Freshness: Fresh, AvailableMemoryMB: 8000, FreeDiskGB: 100, SwapUsedMB: 10, CPUidlePercent: 50, LoadAverage: 2}
	guard := Guardrails{MinFreeDiskGB: 60, MinAvailableMemoryMB: 2000, MaxSwapUsedMB: 100, MaxLoadAverage: 8, MinCPUidlePercent: 15}
	tests := []struct {
		name     string
		snapshot Snapshot
		request  Request
		reason   string
	}{
		{"invalid memory", base, Request{MemoryMB: -1}, "invalid requested memory"},
		{"disk", func() Snapshot { s := base; s.FreeDiskGB = 10; return s }(), Request{}, "disk reserve"},
		{"memory", base, Request{MemoryMB: 7000}, "memory reserve"},
		{"swap", func() Snapshot { s := base; s.SwapUsedMB = 101; return s }(), Request{}, "swap pressure"},
		{"cpu", func() Snapshot { s := base; s.LoadAverage = 9; s.CPUidlePercent = 10; return s }(), Request{}, "cpu pressure"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if decision := guard.Evaluate(test.snapshot, test.request); decision.Allowed || decision.Reason != test.reason {
				t.Fatalf("unexpected decision: %#v", decision)
			}
		})
	}
}

func TestParsingFailuresAndUnits(t *testing.T) {
	if _, _, err := parseVMStat("bad"); err == nil {
		t.Fatal("bad vm_stat accepted")
	}
	if _, err := parseDisk("bad"); err == nil {
		t.Fatal("bad df accepted")
	}
	if _, err := parseDisk("header\nonly two"); err == nil {
		t.Fatal("short df row accepted")
	}
	if _, err := parseDisk("header\na b c nope"); err == nil {
		t.Fatal("nonnumeric df accepted")
	}
	for input, want := range map[string]int64{"used = 1024.00K": 1, "used = 2.00G": 2048, "used = 3.00M": 3} {
		got, err := parseSwap(input)
		if err != nil || got != want {
			t.Fatalf("swap %q: %d %v", input, got, err)
		}
	}
	if _, err := parseSwap("bad"); err == nil {
		t.Fatal("bad swap accepted")
	}
	if _, err := parseSwap("used = 1..2M"); err == nil {
		t.Fatal("nonnumeric swap accepted")
	}
	if _, err := parseCPU("bad"); err == nil {
		t.Fatal("bad cpu accepted")
	}
	if _, err := parseCPU("1..2% idle"); err == nil {
		t.Fatal("nonnumeric CPU accepted")
	}
	if _, err := parseLoad("{}"); err == nil {
		t.Fatal("bad load accepted")
	}
	if _, err := parseLoad("nope"); err == nil {
		t.Fatal("nonnumeric load accepted")
	}
}

func TestSnapshotDegradesAtEveryProbeAndParseStage(t *testing.T) {
	now := time.Unix(200, 0).UTC()
	stages := []string{"df:/", "sysctl:vm.swapusage", "top:0", "sysctl:vm.loadavg"}
	for _, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			runner := &fakeCommands{outputs: validOutputs(), errors: map[string]error{stage: errors.New("failed")}}
			snapshot := (&Probe{Runner: runner, Now: func() time.Time { return now }}).Snapshot(context.Background())
			if snapshot.Freshness != Unavailable || snapshot.Cause == nil {
				t.Fatalf("stage failure not degraded: %#v", snapshot)
			}
		})
	}
	badOutputs := []struct {
		name string
		key  string
	}{
		{"vm", "vm_stat"},
		{"disk", "df"},
		{"swap", "sysctl"},
		{"cpu", "top"},
	}
	for _, test := range badOutputs {
		t.Run("parse-"+test.name, func(t *testing.T) {
			outputs := validOutputs()
			outputs[test.key] = []byte("bad")
			snapshot := (&Probe{Runner: &fakeCommands{outputs: outputs}, Now: func() time.Time { return now }}).Snapshot(context.Background())
			if snapshot.Freshness != Unavailable || snapshot.Cause == nil {
				t.Fatalf("parse failure not degraded: %#v", snapshot)
			}
		})
	}
	runner := &fakeCommands{outputs: validOutputs()}
	runner.outputs["sysctl"] = []byte("used = 1M")
	// Load parsing is reached through the special vm.loadavg response, so test
	// the aggregate parser directly for that final error branch.
	if _, err := parseSnapshot(validOutputs()["vm_stat"], validOutputs()["df"], validOutputs()["sysctl"], validOutputs()["top"], []byte("bad")); err == nil {
		t.Fatal("bad load accepted by aggregate parser")
	}
}

type blockingCommands struct{}

func (blockingCommands) Run(ctx context.Context, _ string, _ ...string) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestProbeTimeoutDefaultsAndExecRunner(t *testing.T) {
	probe := &Probe{Runner: blockingCommands{}, Timeout: time.Millisecond}
	if snapshot := probe.Snapshot(context.Background()); snapshot.Freshness != Unavailable || !errors.Is(snapshot.Cause, context.DeadlineExceeded) {
		t.Fatalf("timeout not classified: %#v", snapshot)
	}
	defaultProbe := &Probe{DiskPath: "/tmp"}
	if defaultProbe.diskPath() != "/tmp" || defaultProbe.runner() == nil || defaultProbe.now()().Location() != time.UTC {
		t.Fatal("probe defaults mismatch")
	}
	output, err := (ExecRunner{}).Run(context.Background(), "/usr/bin/printf", "ok")
	if err != nil || string(output) != "ok" {
		t.Fatalf("exec runner: %q %v", output, err)
	}
}
