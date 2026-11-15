//go:build linux || darwin

package cachegate

import (
	"math"
	"testing"

	"golang.org/x/sys/unix"
)

func TestArtifactPhysicalReserveObservations(t *testing.T) {
	for _, stat := range []unix.Statfs_t{{Bsize: 4096, Bavail: 10, Files: 100, Ffree: 4}, {Bsize: 0, Bavail: 10}, {Bsize: 4096, Bavail: math.MaxUint64}, {Bsize: 1, Bavail: math.MaxUint64}} {
		if _, err := artifactDiskBytes(stat); err == nil {
			t.Fatal("unknown, exhausted inode or overflowing capacity allowed")
		}
	}
	if free, err := artifactDiskBytes(unix.Statfs_t{Bsize: 4096, Bavail: 10, Files: 100, Ffree: 5}); err != nil || free != 40960 {
		t.Fatal("boundary capacity refused")
	}
}

// Distinct mounts that both report fsid 0 (FUSE) do not share a free-space
// pool: only a matching device, or one btrfs filesystem UUID, proves that.
func TestArtifactBudgetRejectsDistinctMountsSharingZeroFsid(t *testing.T) {
	oldStat, oldStatfs := artifactStat, artifactStatfs
	t.Cleanup(func() { artifactStat, artifactStatfs = oldStat, oldStatfs })
	artifactStat = func(path string, stat *unix.Stat_t) error {
		stat.Dev = 1 // Untyped: Dev is uint64 on Linux, int32 on darwin.
		if path == "fuse-bb" {
			stat.Dev = 2
		}
		return nil
	}
	artifactStatfs = func(string, *unix.Statfs_t) error { return nil }
	if sameArtifactFilesystem("fuse-a", "fuse-bb") {
		t.Fatal("distinct zero-fsid mounts accepted")
	}
	if !sameArtifactFilesystem("same-1", "same-2") {
		t.Fatal("same device rejected")
	}
	artifactStat = func(string, *unix.Stat_t) error { return unix.ENOENT }
	if sameArtifactFilesystem("same-1", "same-2") {
		t.Fatal("unobservable path accepted")
	}
}
