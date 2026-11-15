package cachegate

import "golang.org/x/sys/unix"

func sameArtifactSubvolume(_, _ string, _, _ unix.Statfs_t) bool { return false }
