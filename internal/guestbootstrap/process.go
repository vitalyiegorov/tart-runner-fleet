package guestbootstrap

import (
	"context"
	"os/exec"
)

type ExecLauncher struct{}

func (ExecLauncher) Start(ctx context.Context, spec ProcessSpec) (Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if spec.Path == "" || spec.Dir == "" || spec.Log == nil {
		return nil, ErrStart
	}
	// Deliberately do not use CommandContext: the runner must survive the
	// short-lived bootstrap helper and its Tart exec session.
	// #nosec G204 -- runner path is fixed by the trusted guest image and was
	// validated as an executable regular file before this boundary.
	command := exec.Command(spec.Path, spec.Args...)
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
