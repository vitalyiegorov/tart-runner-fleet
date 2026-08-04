package hostpaths

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

func noEnv(string) string { return "" }

// TestDarwinLayoutIsUnchanged pins the live node's paths. ADR 0034's second node
// must not move the first node's installation by one byte.
func TestDarwinLayoutIsUnchanged(t *testing.T) {
	layout := For("darwin", "/Users/fleet", "/tmp", noEnv, 501)
	want := Layout{
		Root:          "/Users/fleet/Library/Application Support/tart-runner-fleet",
		StateDir:      "/Users/fleet/Library/Application Support/tart-runner-fleet/state",
		ConfigPath:    "/Users/fleet/Library/Application Support/tart-runner-fleet/state/fleet.json",
		SocketPath:    "/Users/fleet/Library/Application Support/tart-runner-fleet/state/fleetd.sock",
		UnitsDir:      "/Users/fleet/Library/LaunchAgents",
		ServiceDomain: "gui/501",
	}
	if layout != want {
		t.Fatalf("darwin layout = %#v, want %#v", layout, want)
	}
	if endpoint := layout.Endpoint(); endpoint != "unix://"+want.SocketPath {
		t.Fatalf("endpoint = %q", endpoint)
	}
}

// TestLinuxLayoutMirrorsMacosUnderXDG proves the Linux node keeps the same
// shape — one immutable root, one state directory beneath it, the units beside
// them — expressed in the directories Linux reserves for it.
func TestLinuxLayoutMirrorsMacosUnderXDG(t *testing.T) {
	layout := For("linux", "/home/fleet", "/tmp", noEnv, 1000)
	want := Layout{
		Root:          "/home/fleet/.local/share/tart-runner-fleet",
		StateDir:      "/home/fleet/.local/share/tart-runner-fleet/state",
		ConfigPath:    "/home/fleet/.local/share/tart-runner-fleet/state/fleet.json",
		SocketPath:    "/home/fleet/.local/share/tart-runner-fleet/state/fleetd.sock",
		UnitsDir:      "/home/fleet/.config/systemd/user",
		ServiceDomain: "user",
	}
	if layout != want {
		t.Fatalf("linux layout = %#v, want %#v", layout, want)
	}
}

// TestAbsoluteXDGOverridesAreHonoured covers an operator who moved either XDG
// base directory, which is the whole reason the specification exists.
func TestAbsoluteXDGOverridesAreHonoured(t *testing.T) {
	env := map[string]string{"XDG_DATA_HOME": "/srv/data/", "XDG_CONFIG_HOME": "/srv/config"}
	layout := For("linux", "/home/fleet", "/tmp", func(name string) string { return env[name] }, 1000)
	if layout.Root != "/srv/data/tart-runner-fleet" {
		t.Errorf("root = %q", layout.Root)
	}
	if layout.UnitsDir != "/srv/config/systemd/user" {
		t.Errorf("units = %q", layout.UnitsDir)
	}
}

// TestRelativeXDGOverrideIsIgnored keeps an invalid override from being joined
// onto the home directory, which would silently install the node somewhere the
// operator never named. The specification calls a relative value invalid.
func TestRelativeXDGOverrideIsIgnored(t *testing.T) {
	env := map[string]string{"XDG_DATA_HOME": "share", "XDG_CONFIG_HOME": "cfg"}
	layout := For("linux", "/home/fleet", "/tmp", func(name string) string { return env[name] }, 1000)
	if layout.Root != "/home/fleet/.local/share/tart-runner-fleet" {
		t.Errorf("root = %q", layout.Root)
	}
	if layout.UnitsDir != "/home/fleet/.config/systemd/user" {
		t.Errorf("units = %q", layout.UnitsDir)
	}
}

// TestHomelessProcessFallsBackToATemporaryDirectory preserves the contract the
// admin socket already had: no home directory means a private temporary path,
// never another user's state.
func TestHomelessProcessFallsBackToATemporaryDirectory(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		layout := For(goos, "", "/private/tmp", noEnv, 7)
		if layout.Root != "/private/tmp/tart-runner-fleet" || layout.UnitsDir != "/private/tmp" {
			t.Errorf("%s homeless layout = %#v", goos, layout)
		}
	}
}

// TestDefaultResolvesTheRunningMachine proves the impure wrapper agrees with its
// pure core on whatever platform the suite is executing.
func TestDefaultResolvesTheRunningMachine(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory on this machine")
	}
	if Default() != For(runtime.GOOS, home, os.TempDir(), os.Getenv, os.Getuid()) {
		t.Fatalf("Default() = %#v", Default())
	}
}

// TestDefaultSurvivesAnUnresolvableHome exercises the error arm of
// os.UserHomeDir, which a daemon started from an environment without HOME hits.
func TestDefaultSurvivesAnUnresolvableHome(t *testing.T) {
	t.Setenv("HOME", "")
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skipf("home resolution is not environment-driven on %s", runtime.GOOS)
	}
	layout := Default()
	want := filepath.Join(os.TempDir(), application)
	if layout.Root != want {
		t.Fatalf("root = %q, want %q", layout.Root, want)
	}
	if layout.ServiceDomain != serviceDomain(runtime.GOOS, os.Getuid()) {
		t.Fatalf("domain = %q", layout.ServiceDomain)
	}
	if runtime.GOOS == "darwin" && layout.ServiceDomain != "gui/"+strconv.Itoa(os.Getuid()) {
		t.Fatalf("darwin domain = %q", layout.ServiceDomain)
	}
}
