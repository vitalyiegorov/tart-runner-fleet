package domain

import (
	"strings"
	"testing"
	"time"
)

func TestParseRunnerRefusalReadsOnlyTheRunnersOwnLine(t *testing.T) {
	version, ok := ParseRunnerRefusal([]byte("An error occurred: Runner version v2.336.0 is deprecated and cannot receive messages.\n"))
	if !ok || version != "2.336.0" {
		t.Fatalf("got %q %v", version, ok)
	}
	for _, line := range []string{
		"", "2026-11-05 08:54:16Z: Listening for Jobs\n",
		"2026-11-05 08:54:16Z: Running job: Runner version v2.336.0 is deprecated and cannot receive messages\n",
		"An error occurred: Runner version v2.336 is deprecated and cannot receive messages\n",
		"An error occurred: something else\n",
	} {
		if version, ok := ParseRunnerRefusal([]byte(line)); ok {
			t.Fatalf("%q read as a refusal of %q", line, version)
		}
	}
}

func TestAHoldEndsOneProbeIntervalAfterTheRead(t *testing.T) {
	at := time.Date(2026, 11, 5, 8, 54, 0, 0, time.UTC)
	if got := (RunnerRefusal{At: at}).RetryAt(); !got.Equal(at.Add(30 * time.Minute)) {
		t.Fatalf("got %s", got)
	}
}

// The command runs in a guest and takes no external value, and it must read the
// file guestbootstrap writes.
func TestTheRefusalCommandReadsTheBootstrapLogOnly(t *testing.T) {
	command := RunnerRefusalLogCommand()
	if len(command) != 3 || command[0] != "/bin/sh" || command[1] != "-c" {
		t.Fatalf("got %q", command)
	}
	if !strings.Contains(command[2], `"$HOME/actions-runner/.tart-runner-fleet/runner.log"`) || !strings.HasPrefix(command[2], "grep -E ") {
		t.Fatalf("got %q", command[2])
	}
}
