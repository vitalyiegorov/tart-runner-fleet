package replay_test

import (
	"context"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/macos"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/executor"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// Replay of the 2026-08-04 node C (Mac Studio, Mac16,9: 14 cores, 36 GiB)
// swap-residue report, filed as issue #154.
//
// Observed in OBSERVE mode on fleet v0.1.359, with guards
// minFreeDiskGb 60 / minAvailableMemoryMb 1024 / maxSwapUsedMb 2048 /
// maxLoadAverage 9 / minCpuIdlePercent 5:
//
//	disk 99 GiB  memory 11878 MiB  swap 13593 MiB  cpu idle 79.3%  load 3.69
//	admission allowed (capacity available)
//
// Swap used was 6.6x the configured ceiling and admission was still allowed,
// which reads as a guardrail that never fires. It is not. ADR 0018 made the
// swap guard a level AND rate gate: macOS does not eagerly reclaim swap, so the
// level is a high-water mark and only a measured page-out rate proves the host
// is under pressure now. The host had 3855619 cumulative swapouts and was not
// adding to them, so the level was residue and admission was correct.
//
// The defect this replay pins is that an operator cannot reach that conclusion:
// the rate the guard decides on never leaves the guard. This drives the real
// probe, so the reproduction includes the two-sample rate derivation rather
// than assuming it.
const nodeCPageSize = 16384

// nodeCCommands replays node C's host command output. The vm_stat page counts
// are the ones that yield the reported 11878 MiB, and the Swapouts counter is
// the cumulative figure read from the host over five days of uptime.
type nodeCCommands struct{ swapouts string }

func (c nodeCCommands) Run(_ context.Context, binary string, args ...string) ([]byte, error) {
	switch binary {
	case "vm_stat":
		return []byte("Mach Virtual Memory Statistics: (page size of 16384 bytes)\n" +
			"Pages free: 350000.\nPages active: 392161.\nPages inactive: 400000.\n" +
			"Pages speculative: 10192.\nPages wired down: 208663.\nSwapouts: " + c.swapouts + ".\n"), nil
	case "df":
		// 99 GiB available.
		return []byte("Filesystem 1024-blocks Used Available Capacity Mounted on\n" +
			"/dev/disk3s5 1942700360 934821244 103809024 91% /\n"), nil
	case "top":
		return []byte("CPU usage: 12.51% user, 8.19% sys, 79.30% idle\n"), nil
	case "sysctl":
		switch args[len(args)-1] {
		case "vm.swapusage":
			return []byte("total = 14336.00M  used = 13593.00M  free = 743.00M  (encrypted)\n"), nil
		case "vm.loadavg":
			return []byte("{ 3.69 3.51 3.44 }\n"), nil
		case "hw.ncpu":
			return []byte("14\n"), nil
		case "hw.memsize":
			return []byte("38654705664\n"), nil
		}
	}
	return nil, nil
}

type nodeCStore struct{}

func (nodeCStore) LiveInstances(context.Context) ([]operations.Instance, error) {
	return nil, nil
}

type nodeCExecutor struct{}

func (nodeCExecutor) List(context.Context) ([]executor.Instance, error) { return nil, nil }

// nodeCGuards is node C's deployed guardrail configuration.
func nodeCGuards() executor.Guardrails {
	return executor.Guardrails{MinFreeDiskGB: 60, MinAvailableMemoryMB: 1024, MaxSwapUsedMB: 2048,
		MaxLoadAverage: 9, MinCPUidlePercent: 5}
}

// nodeCObservation drives the real macOS probe for two consecutive ticks -- the
// second is the one that has a rate -- and returns the host observation the
// scheduler and the operator API both read.
func nodeCObservation(t *testing.T) domain.Observation[domain.Host] {
	t.Helper()
	now := time.Date(2026, 8, 4, 17, 25, 0, 0, time.UTC)
	// The counter does not advance between the two samples: the host is carrying
	// residue from an earlier burst, not paging.
	probe := &macos.Probe{Runner: nodeCCommands{swapouts: "3855619"}, Timeout: time.Second,
		Now: func() time.Time { return now }}
	inventory := app.ProductionInventory{Store: nodeCStore{}, Executor: nodeCExecutor{}, Host: probe,
		Capacity: domain.Resources{CPU: 8, MemoryMB: 10_240, Slots: 4}, Guards: nodeCGuards(),
		HostBudget: domain.Resources{CPU: 8, MemoryMB: 10_240}}
	if _, first := inventory.Observe(context.Background()); !first.Usable() {
		t.Fatalf("first observation unusable: %#v", first)
	}
	now = now.Add(30 * time.Second)
	_, host := inventory.Observe(context.Background())
	return host
}

// TestNodeCSwapResidueAdmitsBecauseTheHostIsNotPaging proves the reported
// admission decision is correct and not a guard that failed to run: the level
// clause did compare 13593 against the 2048 ceiling, and the rate clause
// measured zero page-outs across two samples.
func TestNodeCSwapResidueAdmitsBecauseTheHostIsNotPaging(t *testing.T) {
	host := nodeCObservation(t)
	if !host.Usable() {
		t.Fatalf("host observation unusable: %#v", host)
	}
	pressure := host.Value.Pressure
	if pressure.SwapUsedMB != 13_593 || pressure.FreeDiskGB != 99 || pressure.AvailableMemoryMB != 11_878 ||
		pressure.CPUIdlePercent != 79.3 || pressure.LoadAverage != 3.69 {
		t.Fatalf("node C pressure not reproduced: %#v", pressure)
	}
	if pressure.SwapUsedMB <= nodeCGuards().MaxSwapUsedMB {
		t.Fatal("the reproduction must exceed the configured swap ceiling")
	}
	if !pressure.AdmissionAllowed || pressure.AdmissionReason != "capacity available" {
		t.Fatalf("residue must not latch the fleet off a host that is not paging: %#v", pressure)
	}
}

// TestNodeCUnmeasuredRateStillFailsClosedOnTheLevel proves the reporting change
// is not a policy change: the first observation after a daemon start has no
// prior sample, so the level is the only evidence and node C's residue defers
// admission exactly as before.
func TestNodeCUnmeasuredRateStillFailsClosedOnTheLevel(t *testing.T) {
	now := time.Date(2026, 8, 4, 17, 25, 0, 0, time.UTC)
	probe := &macos.Probe{Runner: nodeCCommands{swapouts: "3855619"}, Timeout: time.Second,
		Now: func() time.Time { return now }}
	inventory := app.ProductionInventory{Store: nodeCStore{}, Executor: nodeCExecutor{}, Host: probe,
		Capacity: domain.Resources{CPU: 8, MemoryMB: 10_240, Slots: 4}, Guards: nodeCGuards()}
	_, host := inventory.Observe(context.Background())
	pressure := host.Value.Pressure
	if pressure.AdmissionAllowed || pressure.AdmissionReason != "swap pressure" {
		t.Fatalf("an unmeasured rate must block on the level alone: %#v", pressure)
	}
}

// TestNodeCPageSizeIsTheOneTheHostReports guards the reproduction itself: the
// 11878 MiB figure is only reproducible against a 16 KiB page, which is what
// this Apple silicon host reports.
func TestNodeCPageSizeIsTheOneTheHostReports(t *testing.T) {
	if (350_000+400_000+10_192)*nodeCPageSize/1_048_576 != 11_878 {
		t.Fatal("the replayed vm_stat page counts no longer yield the reported 11878 MiB")
	}
}
