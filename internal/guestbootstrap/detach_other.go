//go:build !darwin && !linux

package guestbootstrap

import "os/exec"

func configureDetached(*exec.Cmd) {}
