// Package hostpaths resolves one node's install layout for the operating system
// it runs on.
//
// The layout is a single fact with several readers — the daemon's admin socket,
// the operator interface's `--root`/`--state-dir`/`--endpoint` defaults, and the
// updater's service-definition directory — and before ADR 0034's second node
// each reader spelled it out again. Two of those spellings disagreed off macOS:
// the operator defaults were the literal `~/Library/Application Support`, while
// the socket followed `os.UserConfigDir`, which is `~/.config` on Linux. A Linux
// daemon therefore listened on a socket its own CLI would not look for.
//
// One resolver removes that class of bug. macOS keeps `~/Library/Application
// Support/tart-runner-fleet` and `~/Library/LaunchAgents` byte for byte; every
// other platform gets the XDG mirror of the same shape — `$XDG_DATA_HOME`
// (default `~/.local/share`) for the immutable root and its state, and
// `$XDG_CONFIG_HOME/systemd/user` (default `~/.config/systemd/user`) for the
// `systemd --user` units that stand in for the LaunchAgents.
package hostpaths

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
)

// application is the one directory name every platform's layout is built around.
const application = "tart-runner-fleet"

// Layout is where one node keeps its releases, its state, and the service
// definitions that boot it. Every path is absolute.
type Layout struct {
	// Root is the immutable installation root: `releases/<version>`, the
	// `current` link, and `state` live beneath it.
	Root string
	// StateDir holds the database, the socket, the configuration, and the
	// updater's transaction journal.
	StateDir string
	// ConfigPath is the persisted fleet configuration inside StateDir.
	ConfigPath string
	// SocketPath is the private admin API socket inside StateDir.
	SocketPath string
	// UnitsDir is the per-user service definition directory the supervisor
	// reads: `~/Library/LaunchAgents` on macOS, the `systemd --user` unit
	// directory elsewhere.
	UnitsDir string
	// ServiceDomain names the supervisor's per-user domain: launchd's
	// `gui/<uid>`, or systemd's user manager.
	ServiceDomain string
}

// Endpoint is SocketPath as the URL the operator interface and the readiness
// probe accept.
func (l Layout) Endpoint() string { return "unix://" + l.SocketPath }

// Default resolves the layout of the machine this process is running on.
func Default() Layout {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	return For(runtime.GOOS, home, os.TempDir(), os.Getenv, os.Getuid())
}

// For is Default's pure core: the layout goos implies for a user whose home
// directory is home, with temp as the fallback when there is no home directory
// at all. A homeless process keeps the pre-existing contract — everything under
// a private temporary directory — because a daemon that cannot find its user
// must not silently adopt some other user's state.
func For(goos, home, temp string, env func(string) string, uid int) Layout {
	data, units := temp, temp
	if home != "" {
		data, units = platformDirectories(goos, home, env)
	}
	root := filepath.Join(data, application)
	state := filepath.Join(root, "state")
	return Layout{
		Root:          root,
		StateDir:      state,
		ConfigPath:    filepath.Join(state, "fleet.json"),
		SocketPath:    filepath.Join(state, "fleetd.sock"),
		UnitsDir:      units,
		ServiceDomain: serviceDomain(goos, uid),
	}
}

// platformDirectories reports the data root and the service definition
// directory. macOS is the Apple layout the live node already uses; every other
// platform is the XDG layout, honouring an absolute override and falling back to
// the specified defaults. A relative override is ignored rather than joined,
// because the specification says a relative XDG value is invalid.
func platformDirectories(goos, home string, env func(string) string) (string, string) {
	if goos == "darwin" {
		return filepath.Join(home, "Library", "Application Support"), filepath.Join(home, "Library", "LaunchAgents")
	}
	data := xdgDirectory(env, "XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	config := xdgDirectory(env, "XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	return data, filepath.Join(config, "systemd", "user")
}

func xdgDirectory(env func(string) string, name, fallback string) string {
	if value := env(name); filepath.IsAbs(value) {
		return filepath.Clean(value)
	}
	return fallback
}

// serviceDomain names the per-user supervisor domain. launchd addresses jobs by
// `gui/<uid>`; a `systemd --user` manager has exactly one user scope and names
// it `user`, so there is nothing to interpolate.
func serviceDomain(goos string, uid int) string {
	if goos == "darwin" {
		return "gui/" + strconv.Itoa(uid)
	}
	return "user"
}
