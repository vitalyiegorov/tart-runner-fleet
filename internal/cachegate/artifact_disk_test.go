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

// Subvolumes have distinct device IDs but share the filesystem free-space pool.
func TestArtifactBudgetAcceptsSubvolumesSharingFilesystem(t *testing.T) {
	old := artifactStatfs
	t.Cleanup(func() { artifactStatfs = old })
	artifactStatfs = func(path string, stat *unix.Statfs_t) error {
		stat.Fsid.Val[0] = 123
		if path == "other-disk" {
			stat.Fsid.Val[0] = 456
		}
		return nil
	}
	if !sameArtifactFilesystem("budget-subvolume", "store-subvolume") {
		t.Fatal("shared pool rejected")
	}
	if sameArtifactFilesystem("budget-subvolume", "other-disk") {
		t.Fatal("different pool accepted")
	}
}
