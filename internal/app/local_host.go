package app

import (
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
)

// FirstUnvalidatedGuestHostMacOSMajor is the first host macOS major on which
// this fleet has not shown that a macOS guest keeps its own memory. On 27.0.1
// it does not: file-backed pages in the guest's page cache come back
// zero-filled mid-job, and every later exec of the runner's bundled Node fails
// with ENOEXEC until the guest cache is purged (issue #363, ADR 0072). Lifting
// the bar is a code change backed by the issue's reproduction passing on the
// newer host, not a configuration knob.
const FirstUnvalidatedGuestHostMacOSMajor = 27

// ValidateLocalHost checks executable configuration against this host. The stat
// and productVersion ports supply filesystem and operating-system evidence;
// Config.Validate and cross-node validation remain independent of the machine
// reading the configuration.
func ValidateLocalHost(cfg config.Config, goos string, stat func(string) (fs.FileInfo, error),
	productVersion func() (string, error)) error {
	if goos != "darwin" || !cfg.MacOS.Enabled {
		return nil
	}
	if err := validateLocalShare(cfg.MacOS.SharedDirectoryPath, stat); err != nil {
		return err
	}
	return validateGuestHostOS(productVersion)
}

func validateLocalShare(path string, stat func(string) (fs.FileInfo, error)) error {
	if path == "" {
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

// validateGuestHostOS refuses macOS guests on a host whose macOS major is not
// validated, and fails closed when the version cannot be read or parsed: an
// unknown host is not a validated one.
func validateGuestHostOS(productVersion func() (string, error)) error {
	version, err := productVersion()
	if err != nil {
		return errors.New("macosBurst.enabled: cannot read this host's macOS version; macOS guests stay off until it can be read")
	}
	major, err := strconv.Atoi(strings.SplitN(strings.TrimSpace(version), ".", 2)[0])
	if err != nil || major <= 0 {
		return fmt.Errorf("macosBurst.enabled: host version %.32q is not a macOS version; macOS guests stay off until it can be read", version)
	}
	if major >= FirstUnvalidatedGuestHostMacOSMajor {
		return fmt.Errorf("macosBurst.enabled: host macOS %.32q is not validated to keep macOS guest memory intact "+
			"(issue #363: guest page cache zero-filled mid-job, runner Node fails with Exec format error); "+
			"set macosBurst.enabled to false on this node", version)
	}
	return nil
}
