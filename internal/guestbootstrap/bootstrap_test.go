package guestbootstrap

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

type fakeProcess struct {
	released bool
	err      error
}

func (p *fakeProcess) Release() error {
	p.released = true
	return p.err
}

type fakeLauncher struct {
	spec    ProcessSpec
	process *fakeProcess
	err     error
}

func (l *fakeLauncher) Start(_ context.Context, spec ProcessSpec) (Process, error) {
	l.spec = spec
	if l.process == nil {
		return nil, l.err
	}
	return l.process, l.err
}

func bootstrapConfig(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	runner := filepath.Join(root, "run.sh")
	if err := os.WriteFile(runner, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return Config{RunnerPath: runner, WorkDir: root, LogPath: filepath.Join(root, "_diag", "runner.log"), MaxJITBytes: 1024}
}

func TestBootstrapPassesJITOnlyInChildEnvironmentAndReleases(t *testing.T) {
	config := bootstrapConfig(t)
	process := &fakeProcess{}
	launcher := &fakeLauncher{process: process}
	bootstrap := Bootstrap{Launcher: launcher}
	if err := bootstrap.Run(context.Background(), strings.NewReader("encoded-jit\n"), config); err != nil {
		t.Fatal(err)
	}
	if !process.released {
		t.Fatal("child process was not released")
	}
	joinedArgs := strings.Join(launcher.spec.Args, " ")
	if strings.Contains(joinedArgs, "encoded-jit") {
		t.Fatal("JIT leaked to argv")
	}
	if got := environmentValue(launcher.spec.Env, JITEnvironment); got != "encoded-jit" {
		t.Fatal("child JIT environment did not match input")
	}
	if countEnvironment(launcher.spec.Env, JITEnvironment) != 1 {
		t.Fatal("duplicate JIT environment")
	}
	if launcher.spec.Path != config.RunnerPath || launcher.spec.Dir != config.WorkDir {
		t.Fatalf("process path=%q dir=%q", launcher.spec.Path, launcher.spec.Dir)
	}
	info, err := os.Stat(config.LogPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("log mode=%v err=%v", info.Mode().Perm(), err)
	}
	dir, err := os.Stat(filepath.Dir(config.LogPath))
	if err != nil || dir.Mode().Perm() != 0o700 {
		t.Fatalf("log dir mode=%v err=%v", dir.Mode().Perm(), err)
	}
}

func TestChildEnvironmentProvidesDeterministicRunnerToolchainPath(t *testing.T) {
	t.Setenv("PATH", "/guest-agent-only")
	t.Setenv("LANG", "ASCII-8BIT")
	t.Setenv("LC_ALL", "C")
	environment := childEnvironment("jit")
	want := "/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
	wantLocale := "C.UTF-8"
	if runtime.GOOS == "darwin" {
		want = "/Users/admin/.rbenv/shims:/opt/homebrew/bin:" + want
		wantLocale = "en_US.UTF-8"
	}
	if got := environmentValue(environment, "PATH"); got != want {
		t.Fatalf("runner PATH=%q want=%q", got, want)
	}
	if countEnvironment(environment, "PATH") != 1 {
		t.Fatal("duplicate PATH environment")
	}
	for _, name := range []string{"LANG", "LC_ALL"} {
		if got := environmentValue(environment, name); got != wantLocale {
			t.Fatalf("runner %s=%q want=%q", name, got, wantLocale)
		}
		if countEnvironment(environment, name) != 1 {
			t.Fatalf("duplicate %s environment", name)
		}
	}
	if got := runnerToolchainPath("linux"); got != "/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin" {
		t.Fatalf("linux runner PATH=%q", got)
	}
	if got := runnerToolchainPath("darwin"); got != "/Users/admin/.rbenv/shims:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin" {
		t.Fatalf("darwin runner PATH=%q", got)
	}
}

func TestDarwinRunnerEnvironmentProvidesAndroidSDK(t *testing.T) {
	t.Setenv("ANDROID_HOME", "/stale/android-home")
	t.Setenv("ANDROID_SDK_ROOT", "/stale/android-sdk-root")
	environment := childEnvironmentForOS("jit", "darwin")
	want := "/Users/admin/android-sdk"
	for _, name := range []string{"ANDROID_HOME", "ANDROID_SDK_ROOT"} {
		if got := environmentValue(environment, name); got != want {
			t.Fatalf("runner %s=%q want=%q", name, got, want)
		}
		if countEnvironment(environment, name) != 1 {
			t.Fatalf("duplicate %s environment", name)
		}
	}

	linux := childEnvironmentForOS("jit", "linux")
	for _, name := range []string{"ANDROID_HOME", "ANDROID_SDK_ROOT"} {
		if countEnvironment(linux, name) != 0 {
			t.Fatalf("linux runner unexpectedly defines %s", name)
		}
	}
}

func TestBootstrapBoundsAndValidatesInputWithoutEchoingIt(t *testing.T) {
	config := bootstrapConfig(t)
	secret := "do-not-echo"
	tests := []struct {
		name  string
		input io.Reader
	}{
		{"empty", strings.NewReader("")},
		{"only newline", strings.NewReader("\n")},
		{"too large", strings.NewReader(strings.Repeat("x", config.MaxJITBytes+1))},
		{"embedded newline", strings.NewReader("one\ntwo")},
		{"nul", strings.NewReader("one\x00two")},
		{"read failure", errorReader{err: errors.New(secret)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := (Bootstrap{Launcher: &fakeLauncher{process: &fakeProcess{}}}).Run(context.Background(), test.input, config)
			if err == nil {
				t.Fatal("invalid input was accepted")
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatal("input secret leaked through error")
			}
		})
	}
}

func TestBootstrapFailsClosedForPathsSymlinksAndProcessErrors(t *testing.T) {
	secret := "jit-never-log"
	config := bootstrapConfig(t)
	bad := config
	bad.RunnerPath = "relative/run.sh"
	if err := (Bootstrap{Launcher: &fakeLauncher{process: &fakeProcess{}}}).Run(context.Background(), strings.NewReader(secret), bad); err == nil {
		t.Fatal("relative runner accepted")
	}
	bad = config
	bad.LogPath = filepath.Join(t.TempDir(), "outside.log")
	if err := (Bootstrap{Launcher: &fakeLauncher{process: &fakeProcess{}}}).Run(context.Background(), strings.NewReader(secret), bad); err == nil {
		t.Fatal("outside log accepted")
	}
	symlink := filepath.Join(config.WorkDir, "runner-link")
	if err := os.Symlink(config.RunnerPath, symlink); err != nil {
		t.Fatal(err)
	}
	bad = config
	bad.RunnerPath = symlink
	if err := (Bootstrap{Launcher: &fakeLauncher{process: &fakeProcess{}}}).Run(context.Background(), strings.NewReader(secret), bad); err == nil {
		t.Fatal("symlink runner accepted")
	}

	for name, launcher := range map[string]*fakeLauncher{
		"start":   {process: &fakeProcess{}, err: errors.New(secret)},
		"release": {process: &fakeProcess{err: errors.New(secret)}},
	} {
		t.Run(name, func(t *testing.T) {
			err := (Bootstrap{Launcher: launcher}).Run(context.Background(), strings.NewReader(secret), config)
			if err == nil {
				t.Fatal("process failure was ignored")
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatal("JIT secret leaked through process error")
			}
		})
	}
}

func TestBootstrapConfigurationAndFilesystemFailureMatrix(t *testing.T) {
	base := bootstrapConfig(t)
	zeroMaximum := base
	zeroMaximum.MaxJITBytes = 0
	if validated, err := validateConfig(zeroMaximum); err != nil || validated.MaxJITBytes != defaultMaxJIT {
		t.Fatalf("default maximum=%d err=%v", validated.MaxJITBytes, err)
	}

	workFile := filepath.Join(t.TempDir(), "work-file")
	if err := os.WriteFile(workFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	nonExecutable := filepath.Join(base.WorkDir, "not-executable")
	if err := os.WriteFile(nonExecutable, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	runnerDirectory := filepath.Join(base.WorkDir, "runner-directory")
	if err := os.Mkdir(runnerDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	outsideRunner := filepath.Join(t.TempDir(), "run.sh")
	if err := os.WriteFile(outsideRunner, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	workLink := filepath.Join(t.TempDir(), "work-link")
	if err := os.Symlink(base.WorkDir, workLink); err != nil {
		t.Fatal(err)
	}
	tests := map[string]func(Config) Config{
		"negative maximum": func(c Config) Config { c.MaxJITBytes = -1; return c },
		"dirty runner":     func(c Config) Config { c.RunnerPath += "/../run.sh"; return c },
		"missing work":     func(c Config) Config { c.WorkDir = filepath.Join(t.TempDir(), "missing"); return c },
		"work file":        func(c Config) Config { c.WorkDir = workFile; return c },
		"work symlink":     func(c Config) Config { c.WorkDir = workLink; return c },
		"missing runner":   func(c Config) Config { c.RunnerPath = filepath.Join(c.WorkDir, "missing"); return c },
		"runner directory": func(c Config) Config { c.RunnerPath = runnerDirectory; return c },
		"runner mode":      func(c Config) Config { c.RunnerPath = nonExecutable; return c },
		"runner outside":   func(c Config) Config { c.RunnerPath = outsideRunner; return c },
		"log equals work":  func(c Config) Config { c.LogPath = c.WorkDir; return c },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := validateConfig(mutate(base)); !errors.Is(err, ErrFilesystem) {
				t.Fatalf("error=%v", err)
			}
		})
	}

	if err := (Bootstrap{}).Run(context.Background(), strings.NewReader("jit"), base); !errors.Is(err, ErrInput) {
		t.Fatalf("nil launcher error=%v", err)
	}
	if err := (Bootstrap{Launcher: &fakeLauncher{process: &fakeProcess{}}}).Run(context.Background(), nil, base); !errors.Is(err, ErrInput) {
		t.Fatalf("nil stdin error=%v", err)
	}
	if err := (Bootstrap{Launcher: &fakeLauncher{}}).Run(context.Background(), strings.NewReader("jit"), base); !errors.Is(err, ErrStart) {
		t.Fatalf("nil process error=%v", err)
	}

	t.Run("file in log directory path", func(t *testing.T) {
		config := bootstrapConfig(t)
		component := filepath.Join(config.WorkDir, "not-a-directory")
		if err := os.WriteFile(component, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		config.LogPath = filepath.Join(component, "runner.log")
		if err := (Bootstrap{Launcher: &fakeLauncher{process: &fakeProcess{}}}).Run(context.Background(), strings.NewReader("jit"), config); !errors.Is(err, ErrFilesystem) {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("oversized directory component", func(t *testing.T) {
		config := bootstrapConfig(t)
		config.LogPath = filepath.Join(config.WorkDir, strings.Repeat("x", 300), "runner.log")
		if err := (Bootstrap{Launcher: &fakeLauncher{process: &fakeProcess{}}}).Run(context.Background(), strings.NewReader("jit"), config); !errors.Is(err, ErrFilesystem) {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("log symlink", func(t *testing.T) {
		config := bootstrapConfig(t)
		directory := filepath.Dir(config.LogPath)
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(config.WorkDir, "target")
		if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, config.LogPath); err != nil {
			t.Fatal(err)
		}
		if err := (Bootstrap{Launcher: &fakeLauncher{process: &fakeProcess{}}}).Run(context.Background(), strings.NewReader("jit"), config); !errors.Is(err, ErrFilesystem) {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("broad log permissions", func(t *testing.T) {
		config := bootstrapConfig(t)
		if err := os.Mkdir(filepath.Dir(config.LogPath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(config.LogPath, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := (Bootstrap{Launcher: &fakeLauncher{process: &fakeProcess{}}}).Run(context.Background(), strings.NewReader("jit"), config); !errors.Is(err, ErrFilesystem) {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("broad directory permissions", func(t *testing.T) {
		config := bootstrapConfig(t)
		if err := os.Mkdir(filepath.Dir(config.LogPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := (Bootstrap{Launcher: &fakeLauncher{process: &fakeProcess{}}}).Run(context.Background(), strings.NewReader("jit"), config); !errors.Is(err, ErrFilesystem) {
			t.Fatalf("error=%v", err)
		}
	})
}

func TestSecureDirectoryRejectsEscapeAndInsecureExistingComponent(t *testing.T) {
	root := t.TempDir()
	if err := secureDirectory(root, filepath.Dir(root)); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("escape error=%v", err)
	}

	locked := filepath.Join(root, "locked")
	if err := os.Mkdir(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	if err := secureDirectory(root, filepath.Join(locked, "new")); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("insecure component error=%v", err)
	}
}

func TestSecureDirectoryPropagatesDirectoryCreationFailure(t *testing.T) {
	root := t.TempDir()
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	if err := secureDirectory(root, filepath.Join(root, "new")); err == nil {
		t.Fatal("directory creation failure was ignored")
	}
}

func TestExecLauncherStartsDetachedProcess(t *testing.T) {
	root := t.TempDir()
	log, err := os.OpenFile(filepath.Join(root, "log"), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	process, err := (ExecLauncher{SudoPath: "/usr/bin/true", ShutdownPath: "/usr/bin/true"}).Start(context.Background(), ProcessSpec{
		Path: "/usr/bin/true", Dir: root, Env: os.Environ(), Log: log,
	})
	if err != nil {
		t.Fatal(err)
	}
	if process == nil {
		t.Fatal("nil process")
	}
	if err := process.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestExecLauncherSupervisesRunnerAndPowersOffGuest(t *testing.T) {
	root := t.TempDir()
	runnerMarker := filepath.Join(root, "runner-finished")
	shutdownMarker := filepath.Join(root, "shutdown-requested")
	runner := filepath.Join(root, "run.sh")
	sudo := filepath.Join(root, "sudo")
	shutdown := filepath.Join(root, "shutdown")
	if err := os.WriteFile(runner, []byte("#!/bin/sh\nprintf finished > \"$RUNNER_MARKER\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sudo, []byte("#!/bin/sh\nprintf '%s' \"$*\" > \"$SHUTDOWN_MARKER\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shutdown, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	log, err := os.OpenFile(filepath.Join(root, "log"), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	process, err := (ExecLauncher{ShellPath: "/bin/sh", SudoPath: sudo, ShutdownPath: shutdown}).Start(context.Background(), ProcessSpec{
		Path: runner, Dir: root, Env: append(os.Environ(), "RUNNER_MARKER="+runnerMarker, "SHUTDOWN_MARKER="+shutdownMarker), Log: log,
	})
	if err != nil || process == nil {
		t.Fatalf("start supervisor = %v, %v", process, err)
	}
	if err := process.Release(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		runnerResult, runnerErr := os.ReadFile(runnerMarker)
		shutdownResult, shutdownErr := os.ReadFile(shutdownMarker)
		if runnerErr == nil && shutdownErr == nil {
			if string(runnerResult) != "finished" || string(shutdownResult) != "-n "+shutdown+" -h now" {
				t.Fatalf("runner/shutdown = %q/%q", runnerResult, shutdownResult)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("detached supervisor did not request guest shutdown after runner exit")
}

func TestExecLauncherValidationCancellationAndStartFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (ExecLauncher{}).Start(ctx, ProcessSpec{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled error=%v", err)
	}
	for _, spec := range []ProcessSpec{{}, {Path: "/usr/bin/true"}, {Path: "/usr/bin/true", Dir: t.TempDir()}} {
		if _, err := (ExecLauncher{}).Start(context.Background(), spec); !errors.Is(err, ErrStart) {
			t.Fatalf("invalid spec error=%v", err)
		}
	}
	log, err := os.OpenFile(filepath.Join(t.TempDir(), "log"), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	if _, err := (ExecLauncher{}).Start(context.Background(), ProcessSpec{Path: "/definitely/missing", Dir: t.TempDir(), Log: log}); err == nil {
		t.Fatal("missing executable started")
	}
	if _, err := (ExecLauncher{SudoPath: "/usr/bin/true", ShutdownPath: "/usr/bin/true"}).Start(context.Background(), ProcessSpec{
		Path: "/usr/bin/true", Dir: filepath.Join(t.TempDir(), "missing"), Log: log,
	}); err == nil {
		t.Fatal("missing working directory started")
	}
	for name, launcher := range map[string]ExecLauncher{
		"relative shell":   {ShellPath: "bin/sh"},
		"missing shell":    {ShellPath: "/definitely/missing-shell"},
		"missing sudo":     {SudoPath: "/definitely/missing-sudo"},
		"missing shutdown": {ShutdownPath: "/definitely/missing-shutdown"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := launcher.Start(context.Background(), ProcessSpec{Path: "/usr/bin/true", Dir: t.TempDir(), Log: log}); !errors.Is(err, ErrStart) {
				t.Fatalf("invalid supervisor path error=%v", err)
			}
		})
	}
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

func environmentValue(environment []string, key string) string {
	prefix := key + "="
	for _, value := range environment {
		if strings.HasPrefix(value, prefix) {
			return strings.TrimPrefix(value, prefix)
		}
	}
	return ""
}

func countEnvironment(environment []string, key string) int {
	prefix := key + "="
	count := 0
	for _, value := range environment {
		if strings.HasPrefix(value, prefix) {
			count++
		}
	}
	return count
}
