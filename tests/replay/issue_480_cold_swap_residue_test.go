package replay_test

import (
	"context"
	"strconv"
	"testing"
	"testing/fstest"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/linux"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/executor"
)

// Replay of ubuntu-vitalii at 2026-11-19 08:56Z, issue #480.
//
// Swap was full, 8191 of 8191 MiB, against a 1024 MiB ceiling. The kernel paged
// out about 50 pages a second, five times ADR 0089's threshold, while
// 38402 MiB stayed available at load 13. The gate stayed closed on `swap
// pressure` and the node ran no guest. `/proc/pressure/memory` read
// `some avg10=0.54`: tasks waited on memory for 0.54% of the last ten seconds.
// The swap was cold residue the kernel kept rotating, not pressure.
//
// This drives the real Linux probe over five 20-second ticks, so the replay
// includes how the stall is read and how the rate is derived, and the first
// tick after a restart, which has no rate yet.

// issue480Guards is ubuntu-vitalii's deployed guardrail configuration.
func issue480Guards() executor.Guardrails {
	return executor.Guardrails{MinFreeDiskGB: 20, MinAvailableMemoryMB: 1024, MaxSwapUsedMB: 1024,
		MaxLoadAverage: 22, MinCPUidlePercent: 15}
}

// issue480Ticks replays the sample for five ticks and returns each host
// observation. pressure is the `/proc/pressure/memory` body, or "" for a kernel
// without PSI.
func issue480Ticks(t *testing.T, pressure string) []domain.Observation[domain.Host] {
	t.Helper()
	files := fstest.MapFS{
		"proc/meminfo": {Data: []byte("MemTotal: 63676416 kB\nMemAvailable: 39323648 kB\n" +
			"SwapTotal: 8387584 kB\nSwapFree: 0 kB\n")},
		"proc/stat":    {Data: []byte("cpu  100 0 100 800 0 0 0 0 0 0\ncpu0 100 0 100 800 0 0 0 0 0 0\n")},
		"proc/loadavg": {Data: []byte("13.02 12.88 12.40 3/2048 88123\n")},
	}
	if pressure != "" {
		files["proc/pressure/memory"] = &fstest.MapFile{Data: []byte(pressure)}
	}
	now := time.Date(2026, 11, 19, 8, 56, 0, 0, time.UTC)
	probe := &linux.Probe{FS: files, Disk: inodeDisk{inodes: btrfsInodeBody}, Now: func() time.Time { return now }}
	inventory := app.ProductionInventory{Store: nodeCStore{}, Executor: nodeCExecutor{}, Host: probe,
		Capacity: domain.Resources{CPU: 12, MemoryMB: 28_672, Slots: 6}, Guards: issue480Guards()}
	var observations []domain.Observation[domain.Host]
	for tick := range 5 {
		// 50 pages a second, 1000 a tick.
		files["proc/vmstat"] = &fstest.MapFile{Data: []byte("pswpout " + strconv.Itoa(9_000_000+1_000*tick) + "\n")}
		_, host := inventory.Observe(context.Background())
		observations = append(observations, host)
		now = now.Add(20 * time.Second)
	}
	return observations
}

// TestIssue480ColdSwapResidueKeepsTheGateOpen: every tick admits, including the
// first after a restart, whose rate is not yet measured. No refusal episode
// begins, so the node keeps its scale-set offer (ADR 0096) and its sessions
// (ADR 0047).
func TestIssue480ColdSwapResidueKeepsTheGateOpen(t *testing.T) {
	for tick, host := range issue480Ticks(t, "some avg10=0.54 avg60=0.61 avg300=0.48 total=912837465\n"+
		"full avg10=0.21 avg60=0.25 avg300=0.19 total=401928374\n") {
		pressure := host.Value.Pressure
		if !host.Usable() || pressure.SwapUsedMB != 8191 || pressure.AvailableMemoryMB != 38_402 {
			t.Fatalf("tick %d: the 2026-11-19 sample was not reproduced: %#v", tick, host)
		}
		if !pressure.MemoryStallObserved || pressure.MemoryStallPercent != 0.54 {
			t.Fatalf("tick %d: stall = %v observed=%v", tick, pressure.MemoryStallPercent, pressure.MemoryStallObserved)
		}
		if tick > 0 && (!pressure.SwapOutRateObserved || pressure.SwapOutRatePerSecond != 50) {
			t.Fatalf("tick %d: rate = %v observed=%v, want 50 measured", tick,
				pressure.SwapOutRatePerSecond, pressure.SwapOutRateObserved)
		}
		if !pressure.AdmissionAllowed {
			t.Fatalf("tick %d: cold swap residue closed the gate: %q", tick, pressure.AdmissionReason)
		}
	}
}

// TestIssue480WithoutPSITheRuleIsUnchanged: the same host on a kernel without
// PSI keeps ADR 0089's rule exactly, so every tick refuses on swap pressure.
func TestIssue480WithoutPSITheRuleIsUnchanged(t *testing.T) {
	for tick, host := range issue480Ticks(t, "") {
		pressure := host.Value.Pressure
		if pressure.MemoryStallObserved || pressure.AdmissionAllowed || pressure.AdmissionReason != "swap pressure" {
			t.Fatalf("tick %d: a host without PSI must keep ADR 0089's rule: %#v", tick, pressure)
		}
	}
}
