package cachegate

import (
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

var artifactBtrfsID = btrfsFilesystemID

func sameArtifactSubvolume(a, b string, x, y unix.Statfs_t) bool {
	if x.Type != unix.BTRFS_SUPER_MAGIC || y.Type != unix.BTRFS_SUPER_MAGIC {
		return false
	}
	first, err := artifactBtrfsID(a)
	if err != nil {
		return false
	}
	second, err := artifactBtrfsID(b)
	return err == nil && first == second
}

func btrfsFilesystemID(path string) ([16]byte, error) {
	var id [16]byte
	f, err := os.Open(path) // #nosec G304 -- explicit operator-selected storage/budget path, opened read-only.
	if err != nil {
		return id, err
	}
	defer func() { _ = f.Close() }()
	// Linux btrfs.h: BTRFS_IOC_FS_INFO = _IOR(0x94, 31, 1024).
	// The kernel writes a 1 KiB fs_info_args; its UUID occupies bytes 16..31.
	var info [1024]byte
	// #nosec G103 -- audited ioctl ABI: kernel writes 1024 bytes into a live 1024-byte buffer; pointer never escapes the syscall.
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), 0x8400941f, uintptr(unsafe.Pointer(&info[0])))
	if errno != 0 {
		return id, errno
	}
	copy(id[:], info[16:32])
	return id, nil
}
