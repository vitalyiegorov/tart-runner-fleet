package adminapi

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
)

const maxSocketPathBytes = 100

var ErrInvalidSocket = errors.New("adminapi: invalid socket path")

func DefaultSocketPath() string {
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "tart-runner-fleet", "state", "fleetd.sock")
}

func DefaultEndpoint() string { return "unix://" + DefaultSocketPath() }

// Listen creates a private Unix socket. A stale socket owned by this process'
// user is recoverable; every other filesystem object fails closed.
func Listen(path string) (net.Listener, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || len([]byte(path)) > maxSocketPathBytes {
		return nil, ErrInvalidSocket
	}
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, fmt.Errorf("create socket directory: %w", err)
	}
	parentInfo, err := os.Lstat(parent)
	if err != nil || !parentInfo.IsDir() || !ownedByCurrentUser(parentInfo) || parentInfo.Mode().Perm()&0o077 != 0 {
		return nil, ErrInvalidSocket
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 || !ownedByCurrentUser(info) {
			return nil, ErrInvalidSocket
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect socket: %w", err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("listen unix: %w", err)
	}
	listener.SetUnlinkOnClose(true)
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("secure socket: %w", err)
	}
	return listener, nil
}

func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int64(stat.Uid) == int64(os.Getuid())
}
