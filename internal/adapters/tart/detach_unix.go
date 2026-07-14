//go:build unix

package tart

import (
	"os/exec"
	"syscall"
)

// configureDetached prevents a launchd restart of fleetd from signalling the
// long-lived Tart VM process group. The durable controller reconnects to the
// still-running VM after restart instead of converting active jobs to orphans.
func configureDetached(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
