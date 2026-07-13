package guestbootstrap

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		t.Fatalf("JIT leaked to argv: %q", joinedArgs)
	}
	if got := environmentValue(launcher.spec.Env, JITEnvironment); got != "encoded-jit" {
		t.Fatalf("child JIT environment=%q", got)
	}
	if countEnvironment(launcher.spec.Env, JITEnvironment) != 1 {
		t.Fatalf("duplicate JIT environment: %#v", launcher.spec.Env)
	}
	if launcher.spec.Path != config.RunnerPath || launcher.spec.Dir != config.WorkDir {
		t.Fatalf("process spec=%#v", launcher.spec)
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
			if err == nil || strings.Contains(err.Error(), secret) {
				t.Fatalf("unsafe error=%v", err)
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
			if err == nil || strings.Contains(err.Error(), secret) {
				t.Fatalf("unsafe error=%v", err)
			}
		})
	}
}

func TestExecLauncherStartsDetachedProcess(t *testing.T) {
	root := t.TempDir()
	log, err := os.OpenFile(filepath.Join(root, "log"), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	process, err := (ExecLauncher{}).Start(context.Background(), ProcessSpec{
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
