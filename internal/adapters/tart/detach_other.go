//go:build !unix

package tart

import "os/exec"

func configureDetached(*exec.Cmd) {}
