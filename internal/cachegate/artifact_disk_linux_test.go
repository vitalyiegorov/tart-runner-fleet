package cachegate

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestArtifactBtrfsSubvolumesUseFilesystemUUID(t *testing.T) {
	oldDev, oldStat, oldID := artifactStat, artifactStatfs, artifactBtrfsID
	t.Cleanup(func() { artifactStat, artifactStatfs, artifactBtrfsID = oldDev, oldStat, oldID })
	// Subvolumes report distinct devices (Omarchy: 57 and 88).
	artifactStat = func(path string, stat *unix.Stat_t) error {
		stat.Dev = uint64(len(path))
		return nil
	}
	artifactStatfs = func(path string, stat *unix.Statfs_t) error {
		stat.Type = unix.BTRFS_SUPER_MAGIC
		stat.Fsid.Val[0] = int32(len(path))
		return nil
	}
	artifactBtrfsID = func(path string) ([16]byte, error) {
		if path == "missing" {
			return [16]byte{}, os.ErrNotExist
		}
		if path == "other" {
			return [16]byte{2}, nil
		}
		return [16]byte{1}, nil
	}
	if !sameArtifactFilesystem("budget", "store-subvolume") {
		t.Fatal("same btrfs UUID rejected")
	}
	for _, paths := range [][2]string{{"budget", "other"}, {"missing", "budget"}, {"budget", "missing"}} {
		if sameArtifactFilesystem(paths[0], paths[1]) {
			t.Fatal("different or unknown UUID accepted")
		}
	}
}

func TestArtifactBtrfsUUIDObservation(t *testing.T) {
	if _, err := btrfsFilesystemID(t.TempDir() + "/missing"); err == nil {
		t.Fatal("missing path accepted")
	}
	dir := t.TempDir()
	var stat unix.Statfs_t
	if err := unix.Statfs(dir, &stat); err != nil {
		t.Fatal(err)
	}
	id, err := btrfsFilesystemID(dir)
	if stat.Type == unix.BTRFS_SUPER_MAGIC {
		if err != nil || id == ([16]byte{}) {
			t.Fatalf("btrfs UUID unavailable: %v", err)
		}
	} else if err == nil {
		t.Fatal("non-btrfs accepted")
	}
	if _, err := btrfsFilesystemID("/dev/null"); err == nil {
		t.Fatal("unsupported ioctl accepted")
	}
}
