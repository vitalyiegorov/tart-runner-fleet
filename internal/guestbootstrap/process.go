package guestbootstrap

import (
	"context"
	"os"
	"os/exec"
)

const supervisorScript = `runner=$1
sudo=$2
shutdown=$3
shift 3
status=0
"$runner" "$@" || status=$?
"$sudo" -n "$shutdown" -h now || exit 70
exit "$status"`

type ExecLauncher struct {
	ShellPath    string
	SudoPath     string
	ShutdownPath string
}

func (l ExecLauncher) Start(ctx context.Context, spec ProcessSpec) (Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	shell, sudo, shutdown := l.paths()
	if spec.Path == "" || spec.Dir == "" || spec.Log == nil || !cleanAbsolute(shell) || !cleanAbsolute(sudo) || !cleanAbsolute(shutdown) {
		return nil, ErrStart
	}
	runnerInfo, err := os.Lstat(spec.Path)
	if err != nil || !runnerInfo.Mode().IsRegular() || runnerInfo.Mode()&os.ModeSymlink != 0 || runnerInfo.Mode().Perm()&0o111 == 0 {
		return nil, ErrStart
	}
	for _, path := range []string{shell, sudo, shutdown} {
		if !executablePath(path) {
			return nil, ErrStart
		}
	}
	// Deliberately do not use CommandContext: the runner must survive the
	// short-lived bootstrap helper and its Tart exec session. A fixed shell
	// program remains as the runner's detached parent so an ephemeral listener
	// exit always powers off the guest. Paths are validated positional
	// arguments; no path or JIT value is interpolated into the program.
	// #nosec G204 -- all executable paths are fixed defaults or trusted test
	// injection and are validated as clean absolute paths.
	args := []string{"-c", supervisorScript, "tart-runner-fleet-supervisor", spec.Path, sudo, shutdown}
	args = append(args, spec.Args...)
	command := exec.Command(shell, args...)
	command.Dir = spec.Dir
	command.Env = append([]string(nil), spec.Env...)
	command.Stdin = nil
	command.Stdout = spec.Log
	command.Stderr = spec.Log
	configureDetached(command)
	if err := command.Start(); err != nil {
		return nil, err
	}
	return command.Process, nil
}

func executablePath(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

func (l ExecLauncher) paths() (string, string, string) {
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
	return shell, sudo, shutdown
}
