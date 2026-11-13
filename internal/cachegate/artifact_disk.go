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

func sameArtifactFilesystem(a, b string) bool {
	var x, y unix.Stat_t
	return unix.Stat(a, &x) == nil && unix.Stat(b, &y) == nil && x.Dev == y.Dev
}
