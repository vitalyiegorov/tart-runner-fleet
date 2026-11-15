//go:build linux || darwin

package cachegate

import (
	"math"
	"math/bits"

	"golang.org/x/sys/unix"
)

func artifactFreeSpace(path string) (int64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return artifactDiskBytes(stat)
}

func artifactDiskBytes(stat unix.Statfs_t) (int64, error) {
	if stat.Files > 0 && float64(stat.Ffree)*100 < 5*float64(stat.Files) || stat.Bsize <= 0 {
		return 0, errArtifactSpace
	}
	high, available := bits.Mul64(stat.Bavail, uint64(stat.Bsize))
	if high != 0 || available > math.MaxInt64 {
		return 0, errArtifactSpace
	}
	return int64(available), nil
}

// artifactStatfs is the filesystem observation seam used by storage admission.
var artifactStatfs = unix.Statfs

// artifactStat is the device observation seam used by storage admission.
var artifactStat = unix.Stat

// A shared device proves one free-space pool. btrfs subvolumes get distinct
// devices on one pool, so only their filesystem UUID can prove it (#513);
// fsid alone cannot, because FUSE mounts all report zero.
func sameArtifactFilesystem(a, b string) bool {
	var x, y unix.Stat_t
	if artifactStat(a, &x) != nil || artifactStat(b, &y) != nil {
		return false
	}
	if x.Dev == y.Dev {
		return true
	}
	var p, q unix.Statfs_t
	return artifactStatfs(a, &p) == nil && artifactStatfs(b, &q) == nil && sameArtifactSubvolume(a, b, p, q)
}
