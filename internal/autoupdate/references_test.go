package autoupdate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type missingService int

func (e missingService) Error() string { return "service missing" }
func (e missingService) ExitCode() int { return int(e) }

func TestReleaseReferenceParsingIsConservativeAndDeterministic(t *testing.T) {
	got := referencedVersions([]string{"/other/releases/v9/fleetd /root/releases/v1+main.abc/fleet /root/releases/v9/helper", "&#47;releases&#47;v2&#47;fleet", "/releases/.generation-temporary/fleet"})
	if !reflect.DeepEqual(got, []string{"v1+main.abc", "v2", "v9"}) {
		t.Fatalf("references=%v", got)
	}
	if got := referencedVersions(nil); len(got) != 0 {
		t.Fatalf("references=%v", got)
	}
}

// The replay's loaded program is much older than the committed generation and
// lies outside rollback headroom. Disk and manager observations are independent.
func TestLaunchdReferencesKeepBothCachedAndOnDiskGenerations(t *testing.T) {
	host, command, current, _, canonical := hostFixture(t)
	command.launchPrint = "program = /old root/releases/v0.1.575+main.old/fleet\n"
	if err := os.WriteFile(canonical, []byte("<string>"+current.ReleaseDir+"/fleet</string>"), 0o600); err != nil {
		t.Fatal(err)
	}
	command.fail = map[string]error{".shadow": missingService(113)}
	refs, err := host.ReferencedReleases(context.Background())
	if err != nil || !reflect.DeepEqual(refs, []string{"v0.1.575+main.old", "v1"}) {
		t.Fatalf("refs=%v err=%v", refs, err)
	}
}

func TestLaunchdReferenceFailuresNeverBecomeEmptyEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		err        error
	}{
		{name: "permission", err: errors.New("permission")},
		{name: "other exit", err: missingService(5)},
		{name: "empty"},
		{name: "partial", body: "arguments = { /releases/v1/fleet }"},
		{name: "empty program", body: "program = "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, command, _, _, _ := hostFixture(t)
			command.launchPrint = tc.body
			if tc.err != nil {
				command.fail = map[string]error{"launchctl print": tc.err}
			}
			refs, err := host.ReferencedReleases(context.Background())
			if !errors.Is(err, ErrServiceReferences) || refs != nil {
				t.Fatalf("refs=%v err=%v", refs, err)
			}
		})
	}
}

func TestUnobservableBootFilesDeferCollection(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      []byte
		directory bool
	}{
		{name: "binary", body: []byte("bplist00")},
		{name: "non UTF8", body: []byte{0xff}},
		{name: "NUL", body: []byte{'x', 0}},
		{name: "directory", directory: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, _, _, _, canonical := hostFixture(t)
			if tc.directory {
				if err := os.Remove(canonical); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(canonical, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(canonical, tc.body, 0o600); err != nil {
				t.Fatal(err)
			}
			if refs, err := host.ReferencedReleases(context.Background()); !errors.Is(err, ErrServiceReferences) || refs != nil {
				t.Fatalf("refs=%v err=%v", refs, err)
			}
		})
	}
	host, _, _, _, _ := hostFixture(t)
	host.launchAgentsDir = filepath.Join(t.TempDir(), "absent")
	if _, err := host.ReferencedReleases(context.Background()); !errors.Is(err, ErrServiceReferences) {
		t.Fatal(err)
	}
}

func TestSystemdReferencesKeepLoadedAndDiskBootPaths(t *testing.T) {
	host, command, _, _, _ := systemdFixture(t)
	command.systemdShow = "LoadState=loaded\nExecStart={ path=/old/releases/v0.1.575/fleet ; argv[]=/old/releases/v0.1.575/fleet run ; }\n"
	refs, err := host.ReferencedReleases(context.Background())
	if err != nil || !reflect.DeepEqual(refs, []string{"v0.1.575", "v1"}) {
		t.Fatalf("refs=%v err=%v", refs, err)
	}
}

func TestSystemdMissingMaskedAndUnavailableReferences(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		err        error
		wantErr    bool
	}{
		{name: "not found", body: "LoadState=not-found\n"},
		{name: "not found exit", body: "LoadState=not-found\n", err: missingService(1)},
		{name: "not found transport", body: "LoadState=not-found\n", err: errors.New("transport"), wantErr: true},
		{name: "not found other exit", body: "LoadState=not-found\n", err: missingService(5), wantErr: true},
		{name: "masked", body: "LoadState=masked\nExecStart=\n"},
		{name: "empty", wantErr: true},
		{name: "missing ExecStart", body: "LoadState=loaded\n", wantErr: true},
		{name: "empty loaded ExecStart", body: "LoadState=loaded\nExecStart=\n", wantErr: true},
		{name: "loading", body: "LoadState=loading\nExecStart=x", wantErr: true},
		{name: "error", err: errors.New("unavailable"), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, command, _, _, _ := systemdFixture(t)
			command.systemdShow = tc.body
			if tc.err != nil {
				host.command = referenceCommand(func(context.Context, string, ...string) ([]byte, error) { return []byte(tc.body), tc.err })
			}
			refs, err := host.ReferencedReleases(context.Background())
			if (err != nil) != tc.wantErr {
				t.Fatalf("refs=%v err=%v", refs, err)
			}
			if tc.wantErr && refs != nil {
				t.Fatalf("unavailable became %v", refs)
			}
		})
	}
	host, _, _, _, _ := systemdFixture(t)
	host.unitsDir = filepath.Join(t.TempDir(), "absent")
	if _, err := host.ReferencedReleases(context.Background()); !errors.Is(err, ErrServiceReferences) {
		t.Fatal(err)
	}
}

type referenceCommand func(context.Context, string, ...string) ([]byte, error)

func (f referenceCommand) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return f(ctx, name, args...)
}

func TestReferenceReadsAreBoundedAndCancelledEvidenceIsUnavailable(t *testing.T) {
	host, _, _, _, _ := systemdFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	host.command = referenceCommand(func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("reference read lacks a deadline")
		}
		cancel()
		return []byte("LoadState=not-found"), nil
	})
	if refs, err := host.ReferencedReleases(ctx); !errors.Is(err, ErrServiceReferences) || refs != nil {
		t.Fatalf("refs=%v err=%v", refs, err)
	}
}

func TestCachedShadowWithoutABootFileStillProtectsItsRelease(t *testing.T) {
	host, _, _, _, _ := systemdFixture(t)
	host.command = referenceCommand(func(_ context.Context, _ string, args ...string) ([]byte, error) {
		for _, arg := range args {
			if arg == "tart-runner-fleet-shadow.service" {
				return []byte("LoadState=loaded\nExecStart={ path=/root/releases/v0.1.575/fleet ; }"), nil
			}
		}
		return []byte("LoadState=not-found"), nil
	})
	refs, err := host.ReferencedReleases(context.Background())
	if err != nil || !reflect.DeepEqual(refs, []string{"v0.1.575", "v1"}) {
		t.Fatalf("refs=%v err=%v", refs, err)
	}
}

func TestCancelledLaunchdReadCannotClaimServiceAbsence(t *testing.T) {
	host, _, _, _, _ := hostFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	host.command = referenceCommand(func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("read lacks deadline")
		}
		cancel()
		return nil, missingService(113)
	})
	if refs, err := host.ReferencedReleases(ctx); !errors.Is(err, ErrServiceReferences) || refs != nil {
		t.Fatalf("refs=%v err=%v", refs, err)
	}
}

func TestBootDefinitionBackupsDoNotPinReleasesForever(t *testing.T) {
	host, command, _, _, canonical := hostFixture(t)
	command.launchPrint = "program = /root/releases/v1/fleet"
	if err := os.WriteFile(canonical+".bak", []byte("/root/releases/v0.1.575/fleet"), 0o600); err != nil {
		t.Fatal(err)
	}
	refs, err := host.ReferencedReleases(context.Background())
	if err != nil || !reflect.DeepEqual(refs, []string{"v1"}) {
		t.Fatalf("refs=%v err=%v", refs, err)
	}
}
