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

// containerSupervisorScript is the same program for a guest with no power
// switch. There is nothing to press when the ephemeral listener exits, so the
// supervisor becomes the runner and its exit is the whole of the teardown: the
// daemon stops and removes the container, exactly as it does for a guest that
// powered itself off (ADR 0010, amendment 2026-08-25).
const containerSupervisorScript = `runner=$1
ready=$2
shift 2
printf 'ready\n' > "$ready" || exit 71
exec "$runner" "$@"`

const (
	supervisorReadyValue = "ready\n"
	supervisorReadyLimit = 10 * time.Second
)

// ExecLauncher starts the runner for real. Its paths describe a virtual
// machine's guest: the transient-scope wrapper and the two poweroff tools of ADR
// 0010. A ProcessSpec that says it is a container reads none of the three — see
// Start — which is why a container guest needs no path override, no shim in the
// image, and no way to be misconfigured into using an init system it does not
// have.
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
	if spec.Path == "" || spec.Dir == "" || spec.Log == nil || !cleanAbsolute(shell) {
		return nil, ErrStart
	}
	paths := []string{shell}
	if spec.Container {
		// A container has no init system to hand the runner to and no machine to
		// power off, so the wrapper and the poweroff tools are neither required nor
		// read. Probing for them would prove nothing either way: the binaries exist
		// in an image that installed the systemd package as a dependency, and they
		// still cannot work (issue #273).
		systemdRun = ""
	} else {
		if !cleanAbsolute(sudo) || !cleanAbsolute(shutdown) {
			return nil, ErrStart
		}
		paths = append(paths, sudo, shutdown)
	}
	if systemdRun != "" {
		if !cleanAbsolute(systemdRun) {
			return nil, ErrStart
		}
		paths = append(paths, systemdRun)
	}
	runnerInfo, err := os.Lstat(spec.Path)
	if err != nil || !runnerInfo.Mode().IsRegular() || runnerInfo.Mode()&os.ModeSymlink != 0 || runnerInfo.Mode().Perm()&0o111 == 0 {
		return nil, ErrStart
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
	defer func() {
		_ = ready.Close()
		_ = os.Remove(readyPath)
	}()
	// Deliberately do not use CommandContext: the runner must survive the
	// short-lived bootstrap helper. In a Tart guest, systemd-run first moves the
	// supervisor into a transient scope outside the guest-agent exec cgroup, and a
	// fixed shell program remains as the runner's parent so an ephemeral listener
	// exit always powers off the guest. In a container there is no scope and no
	// poweroff, so the shell becomes the runner and detachment alone carries it
	// past the `podman exec` session. The JIT value stays inherited in the
	// environment and is never copied into these arguments in either shape.
	supervisorArgs := []string{"-c", supervisorScript, "tart-runner-fleet-supervisor", spec.Path, sudo, shutdown, readyPath}
	if spec.Container {
		supervisorArgs = []string{"-c", containerSupervisorScript, "tart-runner-fleet-supervisor", spec.Path, readyPath}
	}
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
