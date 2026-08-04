package autoupdate

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

var safeRepository = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

type Release struct {
	Version string
	Dir     string
}

// Target is the platform a generation is for. A release publishes one archive
// per node type — ADR 0034 gives the fleet a darwin/arm64 node and a
// linux/amd64 one — so the asset an updater downloads, and the service
// definition its generation must carry, are both properties of the machine
// asking rather than constants.
//
// It is a parameter rather than a read of runtime.GOOS inside the download,
// because the test suite runs on whichever node CI happens to own and a
// platform-dependent fixture would pass on one machine and fail on the other.
type Target struct {
	OS, Arch string
}

// CurrentTarget is the platform this binary was built for.
func CurrentTarget() Target { return Target{OS: runtime.GOOS, Arch: runtime.GOARCH} }

// ArchiveName is the release asset that carries this target's generation.
func (t Target) ArchiveName(version string) string {
	return "tart-runner-fleet-" + version + "-" + t.OS + "-" + t.Arch + ".tar.gz"
}

// ServiceDefinition is the boot definition a generation must carry a verified
// copy of: the authority LaunchAgent on macOS, the authority `systemd --user`
// unit everywhere else. Verifying it with the executable is what makes a
// generation a complete, self-contained thing to boot from.
func (t Target) ServiceDefinition() string {
	if t.OS == "darwin" {
		return authorityServiceDefinition
	}
	return "tart-runner-fleet-authority.service"
}

// LatestProductionRelease downloads and verifies GitHub's latest normal
// release into an immutable version directory. Drafts and prereleases are
// rejected even if an API proxy incorrectly returns one as latest.
func LatestProductionRelease(ctx context.Context, root, repository string, command Command, target Target) (Release, error) {
	if command == nil || !filepath.IsAbs(root) || !safeRepository.MatchString(repository) ||
		target.OS == "" || target.Arch == "" {
		return Release{}, ErrInvalidGeneration
	}
	body, err := command.Run(ctx, "gh", "api", "repos/"+repository+"/releases/latest")
	if err != nil {
		return Release{}, fmt.Errorf("query latest release: %w", err)
	}
	var metadata struct {
		TagName    string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if json.Unmarshal(body, &metadata) != nil || metadata.Draft || metadata.Prerelease || !safeVersion.MatchString(metadata.TagName) {
		return Release{}, ErrInvalidGeneration
	}
	destination := filepath.Join(root, "releases", metadata.TagName)
	if info, statErr := os.Stat(destination); statErr == nil && info.IsDir() {
		if err := verifyReleaseIdentity(destination, metadata.TagName); err != nil {
			return Release{}, err
		}
		return Release{Version: metadata.TagName, Dir: destination}, nil
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return Release{}, statErr
	}
	downloadRoot := filepath.Join(root, "downloads")
	if err := os.MkdirAll(downloadRoot, 0o700); err != nil {
		return Release{}, err
	}
	if err := os.MkdirAll(filepath.Join(root, "releases"), 0o700); err != nil {
		return Release{}, err
	}
	download, err := os.MkdirTemp(downloadRoot, ".release-*")
	if err != nil {
		return Release{}, err
	}
	defer func() { _ = os.RemoveAll(download) }()
	archiveName := target.ArchiveName(metadata.TagName)
	if _, err := command.Run(ctx, "gh", "release", "download", metadata.TagName, "--repo", repository,
		"--pattern", archiveName, "--pattern", "SHA256SUMS", "--dir", download); err != nil {
		return Release{}, fmt.Errorf("download release: %w", err)
	}
	if err := verifyNamedChecksum(filepath.Join(download, "SHA256SUMS"), download, archiveName); err != nil {
		return Release{}, err
	}
	staging, err := os.MkdirTemp(filepath.Join(root, "releases"), ".generation-*")
	if err != nil {
		return Release{}, err
	}
	removeStaging := true
	defer func() {
		if removeStaging {
			_ = os.RemoveAll(staging)
		}
	}()
	if err := extractRelease(filepath.Join(download, archiveName), staging); err != nil {
		return Release{}, err
	}
	if err := stageChecksumManifest(filepath.Join(download, "SHA256SUMS"), filepath.Join(staging, "SHA256SUMS")); err != nil {
		return Release{}, err
	}
	if err := verifyReleaseIdentity(staging, metadata.TagName); err != nil {
		return Release{}, err
	}
	if err := verifyChecksums(staging, target.ServiceDefinition()); err != nil {
		return Release{}, err
	}
	if err := os.Rename(staging, destination); err != nil {
		return Release{}, err
	}
	removeStaging = false
	return Release{Version: metadata.TagName, Dir: destination}, nil
}

// stageChecksumManifest persists the separately published checksum manifest
// inside the immutable generation. The archive cannot contain a digest of
// itself, so production publishes this manifest as an external release asset.
// A manifest embedded in the archive is rejected instead of overwritten.
func stageChecksumManifest(source, destination string) error {
	if _, err := os.Lstat(destination); err == nil {
		return ErrInvalidGeneration
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	body, err := os.ReadFile(source) // #nosec G304 -- exact downloaded checksum asset.
	if err != nil {
		return err
	}
	return atomicWrite(destination, body, 0o600)
}

func verifyReleaseIdentity(dir, version string) error {
	body, err := os.ReadFile(filepath.Join(dir, "RELEASE_VERSION")) // #nosec G304 -- immutable release directory.
	if err != nil || strings.TrimSpace(string(body)) != version {
		return ErrInvalidGeneration
	}
	return nil
}

func verifyNamedChecksum(sumPath, directory, name string) error {
	file, err := os.Open(sumPath) // #nosec G304 -- caller-owned download directory.
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(io.LimitReader(file, 1<<20))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 || fields[1] != name {
			continue
		}
		body, readErr := os.ReadFile(filepath.Join(directory, name)) // #nosec G304 -- exact trusted asset name.
		if readErr != nil {
			return readErr
		}
		digestBytes := sha256.Sum256(body)
		digest := hex.EncodeToString(digestBytes[:])
		if !strings.EqualFold(fields[0], digest) {
			return ErrChecksum
		}
		return nil
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return ErrChecksum
}

func extractRelease(archivePath, destination string) error {
	file, err := os.Open(archivePath) // #nosec G304 -- exact downloaded archive.
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer func() { _ = gzipReader.Close() }()
	tarReader := tar.NewReader(io.LimitReader(gzipReader, 512<<20))
	count := 0
	for {
		header, nextErr := tarReader.Next()
		if errors.Is(nextErr, io.EOF) {
			return nil
		}
		if nextErr != nil {
			return nextErr
		}
		count++
		if count > 128 || header.Typeflag != tar.TypeReg || filepath.Base(header.Name) != header.Name || header.Size < 0 || header.Size > 256<<20 || header.Mode < 0 || header.Mode > 0o777 {
			return ErrInvalidGeneration
		}
		path := filepath.Join(destination, header.Name)                                 // #nosec G305 -- header name is constrained to one basename above.
		mode := os.FileMode(header.Mode) & 0o700                                        // #nosec G115 -- mode is range-checked to 0..0777 above.
		output, createErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode) // #nosec G304 -- basename constrained above.
		if createErr != nil {
			return createErr
		}
		_, copyErr := io.CopyN(output, tarReader, header.Size)
		closeErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
}
