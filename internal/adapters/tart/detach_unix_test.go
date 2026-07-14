//go:build unix

package tart

import (
	"os/exec"
	"testing"
)

func TestTartRunProcessStartsInIndependentSession(t *testing.T) {
	command := exec.Command("/usr/bin/true")
	configureDetached(command)
	if command.SysProcAttr == nil || !command.SysProcAttr.Setsid {
		t.Fatalf("Tart child is not detached from the fleetd launchd process group: %#v", command.SysProcAttr)
	}
}
