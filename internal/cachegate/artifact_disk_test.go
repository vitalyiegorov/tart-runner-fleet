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
