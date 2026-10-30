package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// DefaultHostLauncher is where deploy/macos-host-runner installs the launcher, root-owned,
// and where the sudoers drop-in names it. A launcher the daemon user could rewrite
// would make the sudoers rule a grant of arbitrary code as the runner user, so the
// path is outside every directory the daemon owns, including its release tree.
const DefaultHostLauncher = "/usr/local/libexec/tart-runner-fleet-host-launcher"

// runnerUserGrammar is a macOS short account name the launcher's sudoers rule can
// name: lower-case, no whitespace, and never readable as a command-line option.
var runnerUserGrammar = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// RunsHostProcesses reports whether this node runs its jobs as host processes
// under a dedicated runner user rather than in a guest (ADR 0090).
func (c Config) RunsHostProcesses() bool { return c.Executor.Backend == ExecutorHostProcess }

// HostLauncherPath is the launcher a host-process node invokes through sudo.
func (c Config) HostLauncherPath() string {
	if c.Executor.Binary == "" {
		return DefaultHostLauncher
	}
	return c.Executor.Binary
}

// validateHostProcess checks a host-process node as far as a file can be
// checked. Whether the runner user exists, whether the sudoers rule admits the
// daemon, and whether the launcher is root-owned are host facts, and the daemon's
// preflight proves them before any mode above observe.
//
// The node runs macOS jobs and nothing else. A Linux profile there would be a
// Linux job executed as a macOS host process, which is a label that lies (ADR
// 0032), so the two are refused together rather than one being ignored.
func (c Config) validateHostProcess() error {
	executor := c.Executor
	if !runnerUserGrammar.MatchString(executor.RunnerUser) || executor.RunnerUser == "root" {
		return errors.New("executor runnerUser must name a dedicated non-root macOS short account name")
	}
	if !cleanAbsolutePath(executor.Image) {
		return errors.New("executor image of a hostprocess node must be the clean absolute path of the runner template directory")
	}
	if executor.Binary != "" && !cleanAbsolutePath(executor.Binary) {
		return errors.New("executor binary of a hostprocess node must be the clean absolute path of the root-installed launcher")
	}
	if len(executor.KVMProfiles) > 0 || len(executor.HoldCommand) > 0 || executor.EmulatorSlotsDir != "" {
		return errors.New("kvmProfiles, holdCommand and emulatorSlotsDir are container settings; a hostprocess node has no container")
	}
	if !c.MacOS.Enabled {
		return errors.New("a hostprocess node runs macOS jobs; enable macosBurst and expose its profiles")
	}
	if c.ExecutesLinux() {
		return fmt.Errorf("a hostprocess node runs macOS jobs only; remove linuxProfiles (%d declared)", len(c.Linux.Profiles))
	}
	return nil
}

func cleanAbsolutePath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && path != "/" &&
		!strings.ContainsAny(path, "\r\n\x00")
}
