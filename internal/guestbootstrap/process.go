package guestbootstrap

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"time"
)

const supervisorScript = `runner=$1
sudo=$2
shutdown=$3
ready=$4
shift 4
printf 'ready\n' > "$ready" || exit 71
status=0
"$runner" "$@" || status=$?
"$sudo" -n "$shutdown" -h now || exit 70
exit "$status"`

const (
	supervisorReadyValue = "ready\n"
	supervisorReadyLimit = 10 * time.Second
)

type ExecLauncher struct {
	SystemdRunPath *string
	ReadyTimeout   time.Duration
	ShellPath      string
	SudoPath       string
	ShutdownPath   string
}

func (l ExecLauncher) Start(ctx context.Context, spec ProcessSpec) (Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	systemdRun, shell, sudo, shutdown := l.paths()
	if spec.Path == "" || spec.Dir == "" || spec.Log == nil || !cleanAbsolute(shell) || !cleanAbsolute(sudo) || !cleanAbsolute(shutdown) {
		return nil, ErrStart
	}
	if systemdRun != "" && !cleanAbsolute(systemdRun) {
		return nil, ErrStart
	}
	runnerInfo, err := os.Lstat(spec.Path)
	if err != nil || !runnerInfo.Mode().IsRegular() || runnerInfo.Mode()&os.ModeSymlink != 0 || runnerInfo.Mode().Perm()&0o111 == 0 {
		return nil, ErrStart
	}
	paths := []string{shell, sudo, shutdown}
	if systemdRun != "" {
		paths = append(paths, systemdRun)
	}
	for _, path := range paths {
		if !executablePath(path) {
			return nil, ErrStart
		}
	}
	ready, err := os.CreateTemp(spec.Dir, ".tart-runner-fleet-supervisor-ready-")
	if err != nil {
		return nil, ErrStart
	}
	readyPath := ready.Name()
	if closeErr := ready.Close(); closeErr != nil {
		_ = os.Remove(readyPath)
		return nil, ErrStart
	}
	defer func() { _ = os.Remove(readyPath) }()
	// Deliberately do not use CommandContext: the runner must survive the
	// short-lived bootstrap helper. On Linux, systemd-run first moves the
	// supervisor into a transient scope outside the Tart guest-agent exec
	// cgroup. A fixed shell program remains as the runner's parent so an
	// ephemeral listener exit always powers off the guest. The JIT value stays
	// inherited in the environment and is never copied into these arguments.
	supervisorArgs := []string{"-c", supervisorScript, "tart-runner-fleet-supervisor", spec.Path, sudo, shutdown, readyPath}
	supervisorArgs = append(supervisorArgs, spec.Args...)
	executable := shell
	args := supervisorArgs
	if systemdRun != "" {
		executable = systemdRun
		args = []string{"--scope", "--collect", "--quiet", "--unit=tart-runner-fleet-runner", "--", shell}
		args = append(args, supervisorArgs...)
	}
	// #nosec G204 -- all executable paths are fixed defaults or trusted test
	// injection and are validated as clean absolute executable paths.
	command := exec.Command(executable, args...)
	command.Dir = spec.Dir
	command.Env = append([]string(nil), spec.Env...)
	command.Stdin = nil
	command.Stdout = spec.Log
	command.Stderr = spec.Log
	configureDetached(command)
	if err := command.Start(); err != nil {
		return nil, err
	}
	readyTimeout := l.ReadyTimeout
	if readyTimeout <= 0 {
		readyTimeout = supervisorReadyLimit
	}
	if err := waitSupervisorReady(ctx, readyPath, readyTimeout); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return nil, ErrStart
	}
	return command.Process, nil
}

func waitSupervisorReady(ctx context.Context, path string, limit time.Duration) error {
	timer := time.NewTimer(limit)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		// #nosec G304 -- path is the random marker created by os.CreateTemp
		// inside ExecLauncher.Start and is never accepted from external input.
		value, err := os.ReadFile(path)
		if err == nil && string(value) == supervisorReadyValue {
			return nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return ErrStart
		case <-ticker.C:
		}
	}
}

func executablePath(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

func (l ExecLauncher) paths() (string, string, string, string) {
	systemdRun := defaultSystemdRunPath()
	if l.SystemdRunPath != nil {
		systemdRun = *l.SystemdRunPath
	}
	shell, sudo, shutdown := l.ShellPath, l.SudoPath, l.ShutdownPath
	if shell == "" {
		shell = "/bin/sh"
	}
	if sudo == "" {
		sudo = "/usr/bin/sudo"
	}
	if shutdown == "" {
		shutdown = "/sbin/shutdown"
	}
	return systemdRun, shell, sudo, shutdown
}
