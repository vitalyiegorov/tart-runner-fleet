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
			})
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
	err := ValidateLocalHost(cfg, "darwin", func(string) (fs.FileInfo, error) { return nil, fs.ErrNotExist })
	if err == nil || len(err.Error()) > 512 || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("unbounded or missing diagnostic: %v", err)
	}
}
