package macos

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/executor"
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
	if binary == "sysctl" && len(args) > 0 {
		switch args[len(args)-1] {
		case "vm.loadavg":
			return []byte("{ 2.0 1.5 1.0 }"), nil
		case "kern.memorystatus_level":
			if out, ok := f.outputs["kern.memorystatus_level"]; ok {
				return out, nil
			}
			return []byte("78\n"), nil
		case "hw.memsize":
			if out, ok := f.outputs["hw.memsize"]; ok {
				return out, nil
			}
			return []byte("25769803776\n"), nil
		case "hw.ncpu":
			if out, ok := f.outputs["hw.ncpu"]; ok {
				return out, nil
			}
			return []byte("10\n"), nil
		}
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
	if snapshot.Freshness != executor.Fresh || snapshot.AvailableMemoryMB != 4843 || snapshot.FreeDiskGB != 80 || snapshot.SwapUsedMB != 512 || snapshot.SwapOuts != 7 || snapshot.CPUidlePercent != 90 {
		t.Fatalf("unexpected snapshot: %#v", snapshot)
	}
	guard := executor.Guardrails{MinFreeDiskGB: 60, MinAvailableMemoryMB: 2000, MaxSwapUsedMB: 1024, MaxLoadAverage: 8, MinCPUidlePercent: 15}
	if decision := guard.Evaluate(snapshot, executor.AdmissionRequest{MemoryMB: 2000}); !decision.Allowed {
		t.Fatalf("healthy host rejected: %#v", decision)
	}
	runner.err = errors.New("host unavailable")
	stale := probe.Snapshot(context.Background())
	if stale.Freshness != executor.Stale || stale.Cause == nil {
		t.Fatalf("expected stale cache: %#v", stale)
	}
	if guard.Evaluate(stale, executor.AdmissionRequest{}).Allowed {
		t.Fatal("stale observation allowed")
	}
	empty := (&Probe{Runner: runner, Now: func() time.Time { return now }}).Snapshot(context.Background())
	if empty.Freshness != executor.Unavailable {
		t.Fatalf("expected unavailable: %#v", empty)
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

func TestCriticalProbeFailuresDegradeAdvisoryFailuresDoNot(t *testing.T) {
	now := time.Unix(200, 0).UTC()

	// Memory (vm_stat) and disk (df) are hard capacity signals: a command or
	// parse failure must degrade the whole observation so admission fails
	// closed rather than admitting VMs blind to capacity.
	critical := []struct {
		name   string
		cmdKey string
		badKey string
	}{
		{"vm_stat command", "vm_stat", ""},
		{"df command", "df:/", ""},
		{"vm_stat parse", "", "vm_stat"},
		{"df parse", "", "df"},
	}
	for _, test := range critical {
		t.Run("critical/"+test.name, func(t *testing.T) {
			outputs := validOutputs()
			errs := map[string]error{}
			if test.cmdKey != "" {
				errs[test.cmdKey] = errors.New("failed")
			}
			if test.badKey != "" {
				outputs[test.badKey] = []byte("bad")
			}
			snapshot := (&Probe{Runner: &fakeCommands{outputs: outputs, errors: errs}, Now: func() time.Time { return now }}).Snapshot(context.Background())
			if snapshot.Freshness != executor.Unavailable || snapshot.Cause == nil {
				t.Fatalf("critical failure not degraded: %#v", snapshot)
			}
		})
	}

	// Swap, CPU, and load are advisory throttles: a failure with no prior
	// reading must still produce a executor.Fresh snapshot carrying the real memory and
	// disk figures with permissive advisory defaults, never executor.Unavailable. A
	// flaky `top` bricking the whole scheduler was an 18h fleet-wide outage.
	advisory := []struct {
		name   string
		cmdKey string
		badKey string
	}{
		{"swap command", "sysctl:vm.swapusage", ""},
		{"cpu command", "top:0", ""},
		{"load command", "sysctl:vm.loadavg", ""},
		{"swap parse", "", "sysctl"},
		{"cpu parse", "", "top"},
	}
	for _, test := range advisory {
		t.Run("advisory/"+test.name, func(t *testing.T) {
			outputs := validOutputs()
			errs := map[string]error{}
			if test.cmdKey != "" {
				errs[test.cmdKey] = errors.New("failed")
			}
			if test.badKey != "" {
				outputs[test.badKey] = []byte("bad")
			}
			snapshot := (&Probe{Runner: &fakeCommands{outputs: outputs, errors: errs}, Now: func() time.Time { return now }}).Snapshot(context.Background())
			if snapshot.Freshness != executor.Fresh {
				t.Fatalf("advisory failure bricked the observation: %#v", snapshot)
			}
			if snapshot.AvailableMemoryMB != 4843 || snapshot.FreeDiskGB != 80 {
				t.Fatalf("advisory failure dropped hard-capacity metrics: %#v", snapshot)
			}
		})
	}
}

func TestAdvisoryFailureStaysAdmissibleAndRecoversLastKnown(t *testing.T) {
	now := time.Unix(300, 0).UTC()
	guard := executor.Guardrails{MinFreeDiskGB: 60, MinAvailableMemoryMB: 2000, MaxSwapUsedMB: 1024, MaxLoadAverage: 8, MinCPUidlePercent: 15}

	// Regression for the 18h wedge: a daemon that restarts while `top` is flaky
	// has no prior snapshot, so a CPU-probe failure must still yield a executor.Fresh,
	// admissible observation on permissive defaults instead of bricking.
	coldStart := &fakeCommands{outputs: validOutputs(), errors: map[string]error{"top:0": errors.New("top hung")}}
	fresh := (&Probe{Runner: coldStart, Now: func() time.Time { return now }}).Snapshot(context.Background())
	if fresh.Freshness != executor.Fresh || fresh.CPUidlePercent != permissiveCPUidlePercent {
		t.Fatalf("cold-start CPU probe failure did not degrade permissively: %#v", fresh)
	}
	if !guard.Evaluate(fresh, executor.AdmissionRequest{MemoryMB: 2000}).Allowed {
		t.Fatal("healthy host with an unreadable CPU probe was denied admission")
	}

	// Once a good reading exists, a later advisory failure carries it forward
	// rather than snapping to the permissive default.
	probe := &Probe{Runner: &fakeCommands{outputs: validOutputs()}, Now: func() time.Time { return now }}
	if good := probe.Snapshot(context.Background()); good.CPUidlePercent != 90 {
		t.Fatalf("expected 90%% idle from valid output, got %v", good.CPUidlePercent)
	}
	probe.Runner = &fakeCommands{outputs: validOutputs(), errors: map[string]error{"top:0": errors.New("top hung")}}
	carried := probe.Snapshot(context.Background())
	if carried.Freshness != executor.Fresh || carried.CPUidlePercent != 90 {
		t.Fatalf("advisory failure did not carry the last-known reading: %#v", carried)
	}
}

func TestPressureAccountingPrimaryAndFallback(t *testing.T) {
	now := time.Unix(400, 0).UTC()

	// Flag on with both sysctls valid: availability is memsize x level%
	// (25769803776 B = 24576 MiB, level 78 => 19169 MiB), replacing the legacy
	// page figure (4843 MiB) that understates reality by gigabytes.
	on := &Probe{Runner: &fakeCommands{outputs: validOutputs()}, Now: func() time.Time { return now }, PressureAccounting: true}
	if snapshot := on.Snapshot(context.Background()); snapshot.Freshness != executor.Fresh || snapshot.AvailableMemoryMB != 19169 {
		t.Fatalf("pressure path did not compute memsize x level: %#v", snapshot)
	}

	// Flag off: the pressure sysctls are ignored and the legacy page figure
	// stands byte-for-byte, even though the runner would answer them.
	off := &Probe{Runner: &fakeCommands{outputs: validOutputs()}, Now: func() time.Time { return now }}
	if snapshot := off.Snapshot(context.Background()); snapshot.AvailableMemoryMB != 4843 {
		t.Fatalf("flag off changed legacy availability: %#v", snapshot)
	}

	// A level above 100 (kernel anomaly) is capped at physical memory, never more.
	capped := validOutputs()
	capped["kern.memorystatus_level"] = []byte("150\n")
	high := &Probe{Runner: &fakeCommands{outputs: capped}, Now: func() time.Time { return now }, PressureAccounting: true}
	if snapshot := high.Snapshot(context.Background()); snapshot.AvailableMemoryMB != 24576 {
		t.Fatalf("pressure availability exceeded physical memory: %#v", snapshot)
	}

	// Every pressure read/parse failure falls back to the vm_stat page figure
	// without degrading the observation (executor.Fresh, 4843 MiB).
	fallbacks := []struct {
		name    string
		errKey  string
		badKey  string
		badData string
	}{
		{"level command", "sysctl:kern.memorystatus_level", "", ""},
		{"memsize command", "sysctl:hw.memsize", "", ""},
		{"level parse", "", "kern.memorystatus_level", "bad"},
		{"level negative", "", "kern.memorystatus_level", "-1"},
		{"memsize parse", "", "hw.memsize", "bad"},
		{"memsize zero", "", "hw.memsize", "0"},
	}
	for _, test := range fallbacks {
		t.Run("fallback/"+test.name, func(t *testing.T) {
			outputs := validOutputs()
			errs := map[string]error{}
			if test.errKey != "" {
				errs[test.errKey] = errors.New("failed")
			}
			if test.badKey != "" {
				outputs[test.badKey] = []byte(test.badData)
			}
			probe := &Probe{Runner: &fakeCommands{outputs: outputs, errors: errs}, Now: func() time.Time { return now }, PressureAccounting: true}
			snapshot := probe.Snapshot(context.Background())
			if snapshot.Freshness != executor.Fresh || snapshot.AvailableMemoryMB != 4843 {
				t.Fatalf("pressure failure did not fall back to the page figure: %#v", snapshot)
			}
		})
	}

	// vm_stat remains the fail-closed base: with the flag on, a vm_stat failure
	// still degrades the whole observation to executor.Unavailable.
	degraded := &Probe{Runner: &fakeCommands{outputs: validOutputs(), errors: map[string]error{"vm_stat": errors.New("failed")}}, Now: func() time.Time { return now }, PressureAccounting: true}
	if snapshot := degraded.Snapshot(context.Background()); snapshot.Freshness != executor.Unavailable || snapshot.Cause == nil {
		t.Fatalf("pressure flag masked a critical vm_stat failure: %#v", snapshot)
	}
}

func TestPressureParsingFailuresAndUnits(t *testing.T) {
	if level, err := parseMemorystatusLevel("78\n"); err != nil || level != 78 {
		t.Fatalf("level parse: %d %v", level, err)
	}
	if _, err := parseMemorystatusLevel("bad"); err == nil {
		t.Fatal("nonnumeric level accepted")
	}
	if _, err := parseMemorystatusLevel("-1"); err == nil {
		t.Fatal("negative level accepted")
	}
	if memsize, err := parseMemsizeBytes("25769803776\n"); err != nil || memsize != 25769803776 {
		t.Fatalf("memsize parse: %d %v", memsize, err)
	}
	if _, err := parseMemsizeBytes("bad"); err == nil {
		t.Fatal("nonnumeric memsize accepted")
	}
	if _, err := parseMemsizeBytes("0"); err == nil {
		t.Fatal("nonpositive memsize accepted")
	}
}

type blockingCommands struct{}

func (blockingCommands) Run(ctx context.Context, _ string, _ ...string) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestProbeTimeoutDefaultsAndExecRunner(t *testing.T) {
	probe := &Probe{Runner: blockingCommands{}, Timeout: time.Millisecond}
	if snapshot := probe.Snapshot(context.Background()); snapshot.Freshness != executor.Unavailable || !errors.Is(snapshot.Cause, context.DeadlineExceeded) {
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
