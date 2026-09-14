package autoupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A release carries one SHA256SUMS for both node types. Every archive unpacks
// its controller as `fleet`, but the manifest names the Linux controller by
// its loose-asset name, `fleet-linux-amd64`; the bare `fleet` entry is the
// Apple binary. A Linux node must therefore check its `fleet` against the
// suffixed entry — checking it against `fleet` refuses every real release.
func TestLinuxControllerIsVerifiedAgainstItsSuffixedManifestEntry(t *testing.T) {
	linux := Target{OS: "linux", Arch: "amd64"}
	if got := linux.ControllerAsset(); got != "fleet-linux-amd64" {
		t.Fatalf("linux controller asset=%q", got)
	}
	if got := testTarget.ControllerAsset(); got != "fleet" {
		t.Fatalf("darwin controller asset=%q", got)
	}
	dir := t.TempDir()
	controller := []byte("linux control plane")
	apple := []byte("apple control plane")
	unit := []byte("[Service]\nExecStart=fleet\n")
	for name, body := range map[string][]byte{"RELEASE_VERSION": []byte("v1\n"), "fleet": controller, systemdAuthorityUnit: unit} {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var sums strings.Builder
	for name, body := range map[string][]byte{"RELEASE_VERSION": []byte("v1\n"), "fleet": apple, "fleet-linux-amd64": controller, systemdAuthorityUnit: unit} {
		digest := sha256.Sum256(body)
		sums.WriteString(hex.EncodeToString(digest[:]) + "  " + name + "\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(sums.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyChecksums(dir, linux); err != nil {
		t.Fatalf("real release layout refused on linux: %v", err)
	}
	if err := verifyChecksums(dir, testTarget); !errors.Is(err, ErrChecksum) {
		t.Fatalf("apple verifier accepted a linux controller under the apple entry: %v", err)
	}
}

// The host a node constructs verifies for the platform it runs on unless the
// caller names one; the fixtures name theirs so the tests are the same on
// every developer machine.
func TestHostConfigDefaultsTheTargetToTheRunningPlatform(t *testing.T) {
	root := t.TempDir()
	cfg := LocalHostConfig{RootDir: root, StateDir: root, LaunchAgentsDir: root, Domain: systemdUserDomain,
		Repository: "owner/repo", ReadyAttempts: 1}
	host, err := NewSystemdHost(cfg, &fakeCommand{})
	if err != nil {
		t.Fatal(err)
	}
	if host.target != CurrentTarget() {
		t.Fatalf("target=%+v want %+v", host.target, CurrentTarget())
	}
	cfg.Target = Target{OS: "linux", Arch: "arm64"}
	host, err = NewSystemdHost(cfg, &fakeCommand{})
	if err != nil {
		t.Fatal(err)
	}
	if host.target != cfg.Target {
		t.Fatalf("explicit target dropped: %+v", host.target)
	}
}

var linuxTestTarget = Target{OS: "linux", Arch: "amd64"}

// manifestName is the entry a release's shared SHA256SUMS carries for a member
// of the target's archive.
func manifestName(target Target, name string) string {
	if name == "fleet" {
		return target.ControllerAsset()
	}
	return name
}
