package app

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
)

// ValidateLocalHost checks executable configuration against this host. The stat
// port supplies filesystem evidence; Config.Validate and cross-node validation
// remain independent of the machine reading the configuration.
func ValidateLocalHost(cfg config.Config, goos string, stat func(string) (fs.FileInfo, error)) error {
	path := cfg.MacOS.SharedDirectoryPath
	if goos != "darwin" || !cfg.MacOS.Enabled || path == "" {
		return nil
	}
	info, err := stat(path)
	reason := ""
	switch {
	case errors.Is(err, fs.ErrNotExist):
		reason = "does not exist; create the directory for the daemon user or correct the path"
	case errors.Is(err, fs.ErrPermission):
		reason = "permission denied; allow the daemon user to look up the directory"
	case err != nil:
		reason = "cannot inspect directory; check the path as the daemon user"
	case !info.IsDir():
		reason = "not a directory; select a host directory"
	default:
		return nil
	}
	// Bound operator-controlled text and omit raw filesystem errors, which may
	// contain unrelated paths or implementation details.
	return fmt.Errorf("macosBurst.sharedDirectoryPath %.256q: %s", path, reason)
}
