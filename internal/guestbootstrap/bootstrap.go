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
}

type ProcessSpec struct {
	Path string
	Dir  string
	Args []string
	Env  []string
	Log  *os.File
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
		Path: validated.RunnerPath,
		Dir:  validated.WorkDir,
		Env:  childEnvironment(jit),
		Log:  log,
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
	jitPrefix := JITEnvironment + "="
	pathPrefix := "PATH="
	langPrefix := "LANG="
	lcAllPrefix := "LC_ALL="
	parent := os.Environ()
	environment := make([]string, 0, len(parent)+4)
	for _, value := range parent {
		if !strings.HasPrefix(value, jitPrefix) && !strings.HasPrefix(value, pathPrefix) &&
			!strings.HasPrefix(value, langPrefix) && !strings.HasPrefix(value, lcAllPrefix) {
			environment = append(environment, value)
		}
	}
	locale := runnerLocale(runtime.GOOS)
	return append(environment,
		jitPrefix+jit,
		pathPrefix+runnerToolchainPath(runtime.GOOS),
		langPrefix+locale,
		lcAllPrefix+locale,
	)
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
