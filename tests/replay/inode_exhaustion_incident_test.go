package replay_test

import (
	"context"
	"slices"
	"testing"
	"testing/fstest"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/linux"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/executor"
)

// Replay of the 2026-10-25 ubuntu-vitalii inode exhaustion, filed as issue #423.
//
// Between about 15:20Z and 15:40Z the node's root filesystem had 60,850,176 of
// 60,850,176 inodes in use, consumed by roughly 16.6M small files in the host
// owner's own scratch directory, while it still had 29 GiB of free bytes --
// above its 20 GiB `minFreeDiskGb`. The fleet kept reporting READY and
// "capacity available" and kept starting guests, and every one of them failed
// with `No space left on device`: a clone failed eleven times, three deregister
// stops dead-lettered, CI failed in four repositories, and the actions-cache
// podman layer store became unreadable.
//
// This drives the real Linux probe against `df` output in the shape GNU df
// prints, so the reproduction includes how free space and free inodes are read
// rather than assuming either.
const (
	ubuntuDiskBody = "Filesystem     1024-blocks      Used Available Capacity Mounted on\n" +
		"/dev/nvme0n1p2   958123456 898123456  30408704      97% /\n"
	ubuntuInodeBody = "Filesystem       Inodes    IUsed IFree IUse% Mounted on\n" +
		"/dev/nvme0n1p2 60850176 60850176     0  100% /\n"
	// Omarchy's root is btrfs, which allocates inodes dynamically and reports
	// no inode totals at all.
	btrfsInodeBody = "Filesystem     Inodes IUsed IFree IUse% Mounted on\n" +
		"/dev/nvme0n1p2      0     0     0     - /\n"
)

// inodeDisk answers `df -P -k` and `df -P -i` the way GNU df does.
type inodeDisk struct{ inodes string }

func (d inodeDisk) Run(_ context.Context, args ...string) ([]byte, error) {
	if slices.Contains(args, "-i") {
		return []byte(d.inodes), nil
	}
	return []byte(ubuntuDiskBody), nil
}

func ubuntuProc() fstest.MapFS {
	return fstest.MapFS{
		"proc/meminfo": {Data: []byte("MemTotal: 65536000 kB\nMemAvailable: 50331648 kB\n" +
			"SwapTotal: 8388608 kB\nSwapFree: 8388608 kB\n")},
		"proc/vmstat":  {Data: []byte("pswpout 0\n")},
		"proc/stat":    {Data: []byte("cpu  100 0 100 800 0 0 0 0 0 0\ncpu0 100 0 100 800 0 0 0 0 0 0\n")},
		"proc/loadavg": {Data: []byte("1.00 1.00 1.00 1/100 1\n")},
	}
}

// ubuntuGuards is ubuntu-vitalii's deployed guardrail configuration, with the
// inode floor every node now carries by default.
func ubuntuGuards() executor.Guardrails {
	return executor.Guardrails{MinFreeDiskGB: 20, MinAvailableMemoryMB: 1024, MaxSwapUsedMB: 2048,
		MaxLoadAverage: 9, MinCPUidlePercent: 5, MinFreeInodePercent: 5}
}

func inodeObservation(t *testing.T, inodes string) domain.Observation[domain.Host] {
	t.Helper()
	now := time.Date(2026, 10, 25, 15, 30, 0, 0, time.UTC)
	probe := &linux.Probe{FS: ubuntuProc(), Disk: inodeDisk{inodes: inodes}, Now: func() time.Time { return now }}
	inventory := app.ProductionInventory{Store: nodeCStore{}, Executor: nodeCExecutor{}, Host: probe,
		Capacity: domain.Resources{CPU: 8, MemoryMB: 16_384, Slots: 4}, Guards: ubuntuGuards()}
	_, host := inventory.Observe(context.Background())
	return host
}

// TestUbuntuInodeExhaustionRefusesAdmission is the regression: ample bytes and
// zero free inodes must refuse new work and name inodes as the reason, because
// every guest started on that filesystem fails the moment it creates a file.
func TestUbuntuInodeExhaustionRefusesAdmission(t *testing.T) {
	host := inodeObservation(t, ubuntuInodeBody)
	pressure := host.Value.Pressure
	if pressure.FreeDiskGB != 29 {
		t.Fatalf("free disk = %d GiB, want the 29 GiB the node reported", pressure.FreeDiskGB)
	}
	if pressure.AdmissionAllowed || pressure.AdmissionReason != "inode reserve" {
		t.Fatalf("a node with 0 free inodes must refuse admission as an inode reserve: %#v", pressure)
	}
	if pressure.FreeInodes != 0 || pressure.TotalInodes != 60_850_176 {
		t.Fatalf("inodes free=%d total=%d, want 0 of 60850176", pressure.FreeInodes, pressure.TotalInodes)
	}
}

// TestBtrfsWithoutAnInodeLimitIsStillAdmitted pins the other half: a
// filesystem that reports no fixed inode count has no inode floor to fall
// below, and must not be bricked by a guard that does not apply to it.
func TestBtrfsWithoutAnInodeLimitIsStillAdmitted(t *testing.T) {
	pressure := inodeObservation(t, btrfsInodeBody).Value.Pressure
	if !pressure.AdmissionAllowed || pressure.AdmissionReason != "capacity available" {
		t.Fatalf("a filesystem with no inode limit must stay admissible: %#v", pressure)
	}
	if pressure.TotalInodes != 0 {
		t.Fatalf("total inodes = %d, want 0 (not applicable)", pressure.TotalInodes)
	}
}
