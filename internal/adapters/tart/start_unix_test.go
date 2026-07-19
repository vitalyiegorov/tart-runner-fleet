//go:build unix

package tart

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestExecRunnerStartCapturesOutputOfRealProcess exercises ExecRunner.Start's
// real (non-faked) capture path: a detached /bin/echo process is started,
// polled via StartedCommand.Exited() until it has exited, and its bounded
// combined output is asserted to contain the argument it echoed.
func TestExecRunnerStartCapturesOutputOfRealProcess(t *testing.T) {
	started, err := (ExecRunner{Binary: "/bin/echo"}).Start(context.Background(), "quota-watchdog-probe")
	if err != nil {
		t.Fatalf("start real process: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for !started.Exited() {
		if time.Now().After(deadline) {
			t.Fatal("process did not report exited within 2s")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if output := string(started.Output()); !strings.Contains(output, "quota-watchdog-probe") {
		t.Fatalf("expected output to contain the echoed argument, got %q", output)
	}
}
