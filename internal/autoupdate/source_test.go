package autoupdate

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type releaseCommand struct {
	metadata    string
	assets      string
	calls       []string
	apiErr      error
	downloadErr error
}

func (c *releaseCommand) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	c.calls = append(c.calls, name+" "+strings.Join(args, " "))
	if name == "gh" && len(args) > 1 && args[0] == "api" {
		return []byte(c.metadata), c.apiErr
	}
	if name == "gh" && len(args) > 1 && args[0] == "release" {
		if c.downloadErr != nil {
			return nil, c.downloadErr
		}
		destination := args[len(args)-1]
		entries, err := os.ReadDir(c.assets)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			body, readErr := os.ReadFile(filepath.Join(c.assets, entry.Name()))
			if readErr != nil {
				return nil, readErr
			}
			if writeErr := os.WriteFile(filepath.Join(destination, entry.Name()), body, 0o600); writeErr != nil {
				return nil, writeErr
			}
		}
		return nil, nil
	}
	return nil, errors.New("unexpected command")
}

func TestLatestProductionReleaseFailsClosedAtSourceBoundaries(t *testing.T) {
	for _, test := range []struct {
		name, root, repository, metadata string
		command                          Command
	}{
		{name: "relative root", root: "relative", repository: "owner/repo", command: &releaseCommand{}},
		{name: "unsafe repository", root: t.TempDir(), repository: "owner/repo/extra", command: &releaseCommand{}},
		{name: "nil command", root: t.TempDir(), repository: "owner/repo"},
		{name: "API", root: t.TempDir(), repository: "owner/repo", command: &releaseCommand{apiErr: errors.New("offline")}},
		{name: "invalid JSON", root: t.TempDir(), repository: "owner/repo", command: &releaseCommand{metadata: `{`}},
		{name: "draft", root: t.TempDir(), repository: "owner/repo", command: &releaseCommand{metadata: `{"tag_name":"v2","draft":true}`}},
		{name: "tag", root: t.TempDir(), repository: "owner/repo", command: &releaseCommand{metadata: `{"tag_name":"../../bad"}`}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := LatestProductionRelease(context.Background(), test.root, test.repository, test.command); err == nil {
				t.Fatal("unsafe source accepted")
			}
		})
	}

	root := t.TempDir()
	invalid := filepath.Join(root, "releases", "v2")
	if err := os.MkdirAll(invalid, 0o700); err != nil {
		t.Fatal(err)
	}
	command := &releaseCommand{metadata: `{"tag_name":"v2"}`}
	if _, err := LatestProductionRelease(context.Background(), root, "owner/repo", command); err == nil {
		t.Fatal("invalid existing generation accepted")
	}

	root = t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "releases"), 0o700); err != nil {
		t.Fatal(err)
	}
	command = &releaseCommand{metadata: `{"tag_name":"v2"}`, downloadErr: errors.New("download")}
	if _, err := LatestProductionRelease(context.Background(), root, "owner/repo", command); err == nil {
		t.Fatal("download failure ignored")
	}

	root = t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "downloads"), []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	command = &releaseCommand{metadata: `{"tag_name":"v2"}`}
	if _, err := LatestProductionRelease(context.Background(), root, "owner/repo", command); err == nil {
		t.Fatal("download directory failure ignored")
	}

	root = t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "releases"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "releases", "v2"), []byte("not a generation"), 0o600); err != nil {
		t.Fatal(err)
	}
	command = &releaseCommand{metadata: `{"tag_name":"v2"}`, assets: makeReleaseAssets(t, root, "v2", false)}
	if _, err := LatestProductionRelease(context.Background(), root, "owner/repo", command); err == nil {
		t.Fatal("release path collision ignored")
	}

	root = t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "releases"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("v2", filepath.Join(root, "releases", "v2")); err != nil {
		t.Fatal(err)
	}
	if _, err := LatestProductionRelease(context.Background(), root, "owner/repo", &releaseCommand{metadata: `{"tag_name":"v2"}`}); err == nil {
		t.Fatal("unreadable release path accepted")
	}

	root = t.TempDir()
	downloads := filepath.Join(root, "downloads")
	if err := os.Mkdir(downloads, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(downloads, 0o700)
	if _, err := LatestProductionRelease(context.Background(), root, "owner/repo", &releaseCommand{metadata: `{"tag_name":"v2"}`}); err == nil {
		t.Fatal("unwritable download directory accepted")
	}
}

func TestChecksumAndArchiveReadersRejectMalformedInputs(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "asset"), []byte("asset"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, sum := range []string{"", "bad line\n", strings.Repeat("x", 2<<20)} {
		path := filepath.Join(dir, "sums")
		if err := os.WriteFile(path, []byte(sum), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := verifyNamedChecksum(path, dir, "asset"); err == nil {
			t.Fatalf("malformed checksum accepted: %.20q", sum)
		}
	}
	if err := verifyNamedChecksum(filepath.Join(dir, "missing"), dir, "asset"); err == nil {
		t.Fatal("missing checksum accepted")
	}
	missingAssetDir := t.TempDir()
	digest := sha256.Sum256([]byte("missing"))
	if err := os.WriteFile(filepath.Join(missingAssetDir, "sums"), []byte(hex.EncodeToString(digest[:])+"  asset\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyNamedChecksum(filepath.Join(missingAssetDir, "sums"), missingAssetDir, "asset"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing asset error=%v", err)
	}
	badArchive := filepath.Join(dir, "bad.tar.gz")
	if err := os.WriteFile(badArchive, []byte("bad gzip"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := extractRelease(badArchive, t.TempDir()); err == nil {
		t.Fatal("bad gzip accepted")
	}
	if err := extractRelease(filepath.Join(dir, "missing.tar.gz"), t.TempDir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing archive error=%v", err)
	}

	for _, header := range []*tar.Header{
		{Name: "directory", Typeflag: tar.TypeDir},
		{Name: "../escape", Typeflag: tar.TypeReg},
		{Name: "huge", Typeflag: tar.TypeReg, Size: (256 << 20) + 1},
	} {
		archive := filepath.Join(t.TempDir(), "case.tar.gz")
		file, _ := os.Create(archive)
		gzipWriter := gzip.NewWriter(file)
		tarWriter := tar.NewWriter(gzipWriter)
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		_ = tarWriter.Close()
		_ = gzipWriter.Close()
		_ = file.Close()
		if err := extractRelease(archive, t.TempDir()); err == nil {
			t.Fatalf("unsafe header accepted: %+v", header)
		}
	}
}

func TestArchiveExtractorRejectsDuplicateAndExcessiveMembers(t *testing.T) {
	makeArchive := func(t *testing.T, names []string) string {
		t.Helper()
		archive := filepath.Join(t.TempDir(), "case.tar.gz")
		file, err := os.Create(archive)
		if err != nil {
			t.Fatal(err)
		}
		gzipWriter := gzip.NewWriter(file)
		tarWriter := tar.NewWriter(gzipWriter)
		for _, name := range names {
			if err := tarWriter.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o600, Size: 1}); err != nil {
				t.Fatal(err)
			}
			if _, err := tarWriter.Write([]byte("x")); err != nil {
				t.Fatal(err)
			}
		}
		if err := tarWriter.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gzipWriter.Close(); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		return archive
	}
	if err := extractRelease(makeArchive(t, []string{"same", "same"}), t.TempDir()); err == nil {
		t.Fatal("duplicate archive member accepted")
	}
	names := make([]string, 129)
	for index := range names {
		names[index] = fmt.Sprintf("file-%03d", index)
	}
	if err := extractRelease(makeArchive(t, names), t.TempDir()); !errors.Is(err, ErrInvalidGeneration) {
		t.Fatalf("excessive archive error=%v", err)
	}
}

func makeReleaseAssets(t *testing.T, root, version string, malicious bool) string {
	t.Helper()
	releaseRoot := filepath.Join(t.TempDir(), "fixture")
	dir := makeRelease(t, releaseRoot, version)
	assets := filepath.Join(t.TempDir(), "assets")
	if err := os.MkdirAll(assets, 0o700); err != nil {
		t.Fatal(err)
	}
	archiveName := "tart-runner-fleet-" + version + "-darwin-arm64.tar.gz"
	archivePath := filepath.Join(assets, archiveName)
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		body, readErr := os.ReadFile(filepath.Join(dir, entry.Name()))
		if readErr != nil {
			t.Fatal(readErr)
		}
		name := entry.Name()
		if malicious && name == "fleetd" {
			name = "../fleetd"
		}
		if err := tarWriter.WriteHeader(&tar.Header{Name: name, Mode: 0o700, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(tarWriter, strings.NewReader(string(body))); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(archivePath)
	digest := sha256.Sum256(body)
	if err := os.WriteFile(filepath.Join(assets, "SHA256SUMS"), []byte(hex.EncodeToString(digest[:])+"  "+archiveName+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return assets
}

func TestLatestProductionReleaseDownloadsVerifiesAndExtractsOnce(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "releases"), 0o700); err != nil {
		t.Fatal(err)
	}
	command := &releaseCommand{metadata: `{"tag_name":"v2","draft":false,"prerelease":false}`, assets: makeReleaseAssets(t, root, "v2", false)}
	release, err := LatestProductionRelease(context.Background(), root, "owner/repo", command)
	if err != nil || release.Version != "v2" || release.Dir != filepath.Join(root, "releases", "v2") {
		t.Fatalf("release=%+v err=%v", release, err)
	}
	if _, err := os.Stat(filepath.Join(release.Dir, "fleetd")); err != nil {
		t.Fatal(err)
	}
	if _, err := LatestProductionRelease(context.Background(), root, "owner/repo", command); err != nil {
		t.Fatal(err)
	}
	downloads := 0
	for _, call := range command.calls {
		if strings.Contains(call, "release download") {
			downloads++
		}
	}
	if downloads != 1 {
		t.Fatalf("downloads=%d calls=%v", downloads, command.calls)
	}
}

func TestLatestProductionReleaseRejectsPrereleaseTamperingAndTraversal(t *testing.T) {
	for _, test := range []struct {
		name, metadata string
		malicious      bool
		tamper         bool
	}{
		{name: "prerelease", metadata: `{"tag_name":"v2","prerelease":true}`},
		{name: "archive traversal", metadata: `{"tag_name":"v2"}`, malicious: true},
		{name: "checksum", metadata: `{"tag_name":"v2"}`, tamper: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "releases"), 0o700); err != nil {
				t.Fatal(err)
			}
			assets := makeReleaseAssets(t, root, "v2", test.malicious)
			if test.tamper {
				entries, _ := os.ReadDir(assets)
				for _, entry := range entries {
					if strings.HasSuffix(entry.Name(), ".tar.gz") {
						if err := os.WriteFile(filepath.Join(assets, entry.Name()), []byte("tampered"), 0o600); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			_, err := LatestProductionRelease(context.Background(), root, "owner/repo", &releaseCommand{metadata: test.metadata, assets: assets})
			if err == nil {
				t.Fatal("unsafe release accepted")
			}
		})
	}
}
