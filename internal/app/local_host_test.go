package app

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
)

func TestValidateLocalHostShare(t *testing.T) {
	for _, tc := range []struct {
		name, goos, path string
		disabled         bool
		mode             fs.FileMode
		lookupErr        error
		want             string
		calls            int
	}{
		{name: "absent wrong user", goos: "darwin", path: "/Users/other/ci-shared", lookupErr: fs.ErrNotExist, want: "does not exist", calls: 1},
		{name: "file", goos: "darwin", path: "/share", want: "not a directory", calls: 1},
		{name: "unreadable lookup", goos: "darwin", path: "/share", lookupErr: fs.ErrPermission, want: "permission denied", calls: 1},
		{name: "unavailable lookup", goos: "darwin", path: "/share", lookupErr: errors.New("private backend detail"), want: "cannot inspect directory", calls: 1},
		{name: "valid", goos: "darwin", path: "/share", mode: fs.ModeDir, calls: 1},
		{name: "disabled", goos: "darwin", path: "/share", disabled: true},
		{name: "linux remote path", goos: "linux", path: "/Users/other/ci-shared"},
		{name: "other platform", goos: "freebsd", path: "/share"},
		{name: "no share", goos: "darwin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.MacOS.Enabled = !tc.disabled
			cfg.MacOS.SharedDirectoryPath = tc.path
			calls := 0
			err := ValidateLocalHost(cfg, tc.goos, func(path string) (fs.FileInfo, error) {
				calls++
				if path != tc.path {
					t.Fatalf("lookup %q", path)
				}
				info, statErr := (fstest.MapFS{"share": &fstest.MapFile{Mode: tc.mode}}).Stat("share")
				if statErr != nil {
					t.Fatal(statErr)
				}
				return info, tc.lookupErr
			}, validatedHost)
			if calls != tc.calls {
				t.Fatalf("lookups = %d, want %d", calls, tc.calls)
			}
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), tc.path) || !strings.Contains(err.Error(), "macosBurst.sharedDirectoryPath") {
				t.Fatalf("error = %v", err)
			}
			if strings.Contains(err.Error(), "private backend detail") {
				t.Fatal("leaked raw lookup error")
			}
		})
	}
}

func TestLocalHostLookupDiagnosticIsBounded(t *testing.T) {
	cfg := config.Default()
	cfg.MacOS.SharedDirectoryPath = "/" + strings.Repeat("x", 4096)
	err := ValidateLocalHost(cfg, "darwin", func(string) (fs.FileInfo, error) { return nil, fs.ErrNotExist }, validatedHost)
	if err == nil || len(err.Error()) > 512 || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("unbounded or missing diagnostic: %v", err)
	}
}

func validatedHost() (string, error) { return "26.5", nil }

// Replay #363: on the macOS 27.0.1 MacBook, file-backed pages in a macOS guest's
// page cache came back zero-filled mid-job, so the runner's bundled
// externals/node24/bin/node failed every later exec with ENOEXEC. A node whose
// host is on an unvalidated macOS major must not start with macOS guests
// enabled; its Linux guests are unaffected.
func TestValidateLocalHostRefusesMacOSGuestsOnUnvalidatedHostOS(t *testing.T) {
	noShare := func(string) (fs.FileInfo, error) {
		t.Fatal("looked up a share that is not configured")
		return nil, nil
	}
	for _, tc := range []struct {
		name, goos, version string
		readErr             error
		disabled            bool
		want                string
		reads               int
	}{
		{name: "macbook incident host", goos: "darwin", version: "27.0.1", want: `host macOS "27.0.1"`, reads: 1},
		{name: "later unvalidated major", goos: "darwin", version: "28.0", want: `host macOS "28.0"`, reads: 1},
		{name: "major only", goos: "darwin", version: "27", want: `host macOS "27"`, reads: 1},
		{name: "mac mini", goos: "darwin", version: "26.5", reads: 1},
		{name: "mac studio", goos: "darwin", version: "15.7.4", reads: 1},
		{name: "unreadable version fails closed", goos: "darwin", readErr: errors.New("private sysctl detail"), want: "cannot read this host's macOS version", reads: 1},
		{name: "unparsable version fails closed", goos: "darwin", version: "twenty-seven", want: "is not a macOS version", reads: 1},
		{name: "empty version fails closed", goos: "darwin", version: "", want: "is not a macOS version", reads: 1},
		{name: "macos guests disabled", goos: "darwin", version: "27.0.1", disabled: true},
		{name: "linux node", goos: "linux", version: "27.0.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.MacOS.Enabled = !tc.disabled
			reads := 0
			err := ValidateLocalHost(cfg, tc.goos, noShare, func() (string, error) {
				reads++
				return tc.version, tc.readErr
			})
			if reads != tc.reads {
				t.Fatalf("version reads = %d, want %d", reads, tc.reads)
			}
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "macosBurst.enabled") {
				t.Fatalf("error = %v", err)
			}
			if strings.Contains(err.Error(), "private sysctl detail") {
				t.Fatal("leaked raw version read error")
			}
		})
	}
}

func TestHostOSDiagnosticIsBounded(t *testing.T) {
	cfg := config.Default()
	cfg.MacOS.Enabled = true
	err := ValidateLocalHost(cfg, "darwin", nil, func() (string, error) { return strings.Repeat("9", 4096), nil })
	if err == nil || len(err.Error()) > 512 {
		t.Fatalf("unbounded or missing diagnostic: %v", err)
	}
}

// A share failure is reported before the host version is read, so an operator
// repairing both sees the cheaper, purely local repair first and the version
// port is never asked a question whose answer cannot change the outcome.
func TestShareFailureIsReportedBeforeHostOS(t *testing.T) {
	cfg := config.Default()
	cfg.MacOS.Enabled = true
	cfg.MacOS.SharedDirectoryPath = "/absent"
	err := ValidateLocalHost(cfg, "darwin", func(string) (fs.FileInfo, error) { return nil, fs.ErrNotExist }, func() (string, error) {
		t.Fatal("read the host version after the share already failed")
		return "", nil
	})
	if err == nil || !strings.Contains(err.Error(), "sharedDirectoryPath") {
		t.Fatalf("error = %v", err)
	}
}
