// Package guestbootstrap starts the preinstalled GitHub Actions runner inside
// an ephemeral guest without exposing its JIT configuration outside the child
// process environment.
package guestbootstrap

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

const (
	JITEnvironment = "ACTIONS_RUNNER_INPUT_JITCONFIG"
	defaultMaxJIT  = 1 << 20
)

// ContainerFlag is how the daemon tells a guest what it is running inside. Its
// absence is a virtual machine, so every guest that predates the flag is invoked
// with exactly the argument vector it always was.
//
// The guest is told rather than left to work it out. A container image built
// from the Linux base carries `/usr/bin/systemd-run` and `/sbin/shutdown`
// whether or not it has an init system — Playwright's dependency closure
// installs the systemd package — so probing for the binaries answers a question
// nobody asked, and `systemd-run --scope` then fails at runtime with "system has
// not been booted with systemd as init system (PID 1)" long after the daemon
// stopped being able to explain it (issue #273). Only the side that created the
// guest knows what it created, and that side is `internal/daemon`.
const ContainerFlag = "--container"

var (
	ErrInput      = errors.New("invalid runner bootstrap input")
	ErrFilesystem = errors.New("runner bootstrap filesystem validation failed")
	ErrStart      = errors.New("runner bootstrap start failed")
	ErrRelease    = errors.New("runner bootstrap process release failed")
)

type Config struct {
	RunnerPath  string
	WorkDir     string
	LogPath     string
	MaxJITBytes int
	// RequiredCapabilities is what the daemon expected of this guest's image,
	// taken from the `requiresCapabilities` of the scale sets routed to this
	// instance's profile. Empty means no check and no manifest read, which is
	// every guest that predates the feature.
	RequiredCapabilities []string
	// CapabilityManifestPath overrides where the guest's own declaration is read
	// from. Empty is CapabilityManifestPath, which is the only value production
	// ever uses; the field exists so the check is testable without writing to an
	// absolute system path.
	CapabilityManifestPath string
	// Container is what kind of guest this is, as stated by the daemon that made
	// it (ContainerFlag). False is a virtual machine: an init system to place the
	// runner under and a power switch to press when it exits, which is ADR 0010.
	// True is a container: neither exists, so the runner is started directly and
	// the supervisor's own exit is the whole of the teardown.
	Container bool
}

func (c Config) capabilityManifestPath() string {
	if c.CapabilityManifestPath == "" {
		return CapabilityManifestPath
	}
	return c.CapabilityManifestPath
}

type ProcessSpec struct {
	Path string
	Dir  string
	Args []string
	Env  []string
	Log  *os.File
	// Container carries Config.Container to the launcher, which is the only thing
	// in this package that acts on the difference.
	Container bool
}

type Process interface{ Release() error }

type Launcher interface {
	Start(context.Context, ProcessSpec) (Process, error)
}

type Bootstrap struct{ Launcher Launcher }

func (b Bootstrap) Run(ctx context.Context, stdin io.Reader, config Config) error {
	if b.Launcher == nil || stdin == nil {
		return ErrInput
	}
	validated, err := validateConfig(config)
	if err != nil {
		return err
	}
	// Before a byte of standard input is read, so a capability failure provably
	// cannot carry the JIT configuration and is therefore the one failure this
	// helper is allowed to describe out loud.
	if err := checkCapabilities(validated.capabilityManifestPath(), validated.RequiredCapabilities); err != nil {
		return err
	}
	jit, err := readJIT(stdin, validated.MaxJITBytes)
	if err != nil {
		return err
	}
	log, err := secureLog(validated.WorkDir, validated.LogPath)
	if err != nil {
		return ErrFilesystem
	}
	defer func() { _ = log.Close() }()

	process, err := b.Launcher.Start(ctx, ProcessSpec{
		Path:      validated.RunnerPath,
		Dir:       validated.WorkDir,
		Env:       childEnvironment(jit),
		Log:       log,
		Container: validated.Container,
	})
	if err != nil || process == nil {
		return ErrStart
	}
	if err := process.Release(); err != nil {
		return ErrRelease
	}
	return nil
}

func validateConfig(config Config) (Config, error) {
	if config.MaxJITBytes == 0 {
		config.MaxJITBytes = defaultMaxJIT
	}
	if config.MaxJITBytes < 1 || !cleanAbsolute(config.RunnerPath) || !cleanAbsolute(config.WorkDir) || !cleanAbsolute(config.LogPath) {
		return Config{}, ErrFilesystem
	}
	workInfo, err := os.Lstat(config.WorkDir)
	if err != nil || !workInfo.IsDir() || workInfo.Mode()&os.ModeSymlink != 0 {
		return Config{}, ErrFilesystem
	}
	runnerInfo, err := os.Lstat(config.RunnerPath)
	if err != nil || !runnerInfo.Mode().IsRegular() || runnerInfo.Mode()&os.ModeSymlink != 0 || runnerInfo.Mode().Perm()&0o111 == 0 {
		return Config{}, ErrFilesystem
	}
	if !inside(config.WorkDir, config.RunnerPath) || !inside(config.WorkDir, config.LogPath) || config.LogPath == config.WorkDir {
		return Config{}, ErrFilesystem
	}
	return config, nil
}

func cleanAbsolute(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path
}

func inside(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func readJIT(reader io.Reader, maximum int) (string, error) {
	data, err := io.ReadAll(io.LimitReader(reader, int64(maximum)+1))
	if err != nil || len(data) == 0 || len(data) > maximum {
		zero(data)
		return "", ErrInput
	}
	defer zero(data)
	data = bytes.TrimSuffix(data, []byte{'\n'})
	data = bytes.TrimSuffix(data, []byte{'\r'})
	if len(data) == 0 || bytes.ContainsAny(data, "\r\n\x00") {
		return "", ErrInput
	}
	return string(data), nil
}

func zero(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

func childEnvironment(jit string) []string {
	return childEnvironmentForOS(jit, runtime.GOOS)
}

func childEnvironmentForOS(jit, goos string) []string {
	jitPrefix := JITEnvironment + "="
	pathPrefix := "PATH="
	langPrefix := "LANG="
	lcAllPrefix := "LC_ALL="
	androidHomePrefix := "ANDROID_HOME="
	androidSDKRootPrefix := "ANDROID_SDK_ROOT="
	parent := os.Environ()
	environment := make([]string, 0, len(parent)+6)
	for _, value := range parent {
		if strings.HasPrefix(value, jitPrefix) || strings.HasPrefix(value, pathPrefix) ||
			strings.HasPrefix(value, langPrefix) || strings.HasPrefix(value, lcAllPrefix) {
			continue
		}
		// The two Android variables split by platform. On macOS the parent is
		// an uncontrolled VM login shell, so any inherited value is stale by
		// definition: strip here, and re-add the fixed SDK path below. On
		// Linux the parent IS the sealed runner image — its ENV is the image's
		// declaration of where the baked SDK lives — so erasing it here would
		// leave an Android job no way to find an SDK the image audit promised.
		if (strings.HasPrefix(value, androidHomePrefix) || strings.HasPrefix(value, androidSDKRootPrefix)) &&
			goos != "linux" {
			continue
		}
		environment = append(environment, value)
	}
	locale := runnerLocale(goos)
	environment = append(environment,
		jitPrefix+jit,
		pathPrefix+runnerToolchainPath(goos),
		langPrefix+locale,
		lcAllPrefix+locale,
	)
	if goos == "darwin" {
		environment = append(environment,
			androidHomePrefix+darwinAndroidSDKPath(),
			androidSDKRootPrefix+darwinAndroidSDKPath(),
		)
	}
	return environment
}

func darwinAndroidSDKPath() string {
	return "/Users/admin/android-sdk"
}

func runnerToolchainPath(goos string) string {
	portable := "/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
	if goos == "darwin" {
		return "/Users/admin/.rbenv/shims:/opt/homebrew/bin:" + portable
	}
	return portable
}

func runnerLocale(goos string) string {
	if goos == "darwin" {
		return "en_US.UTF-8"
	}
	return "C.UTF-8"
}

func secureLog(workDir, path string) (*os.File, error) {
	if err := secureDirectory(workDir, filepath.Dir(path)); err != nil {
		return nil, err
	}
	file, err := openLogNoFollow(path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		_ = file.Close()
		return nil, operations.ErrInvalid
	}
	return file, nil
}

func secureDirectory(root, target string) error {
	if !inside(root, target) {
		return operations.ErrInvalid
	}
	relative, _ := filepath.Rel(root, target) // inside already proved this relation
	current := root
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if errors.Is(statErr, os.ErrNotExist) {
			if err := os.Mkdir(current, 0o700); err != nil {
				return err
			}
			continue
		}
		if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
			return operations.ErrInvalid
		}
	}
	return nil
}
