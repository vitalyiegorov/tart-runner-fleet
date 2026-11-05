package domain

import (
	"regexp"
	"time"
)

// RunnerRefusal is a guest's log naming its runner version as refused by the broker:
// which version, which guest, and when the fleet read it (ADR 0094).
//
// It is advisory and nothing else. A job runs as the user that owns that log and can
// write this line, so it may name a reason in `doctor`, and it can never hold, delay or
// suppress a launch or fail a check.
type RunnerRefusal struct {
	Version  string
	Instance string
	At       time.Time
}

// runnerRefusalLine is the complete line the runner prints, valid in both POSIX ERE and
// Go's syntax so the guest's grep and the host's matcher can never disagree. A job can
// write this log, so the grammar is what bounds the only guest text that reaches a log
// line or a status document: no version longer than 20 bytes is admitted, whatever is
// written. Matching all of the line keeps a longer one that begins this way out.
const runnerRefusalLine = `^An error occurred: Runner version v([0-9]{1,6}\.[0-9]{1,6}\.[0-9]{1,6}) is deprecated and cannot receive messages\.$`

// RunnerRefusalLogCommand prints the first refusal line from a guest's runner log and
// nothing else from it, however many there are. The log is the runner's standard output:
// its own status lines, never a job's, unless a job writes to it, which is why the
// answer is capped here and, since a job could replace these tools too, on the host
// (podman.GuestOutputLimit). The path is the one guestbootstrap writes.
func RunnerRefusalLogCommand() []string {
	return []string{"/bin/sh", "-c", "grep -m1 -E '" + runnerRefusalLine + `' "$HOME/actions-runner/.tart-runner-fleet/runner.log" | head -c 256`}
}

var runnerRefusalMatcher = regexp.MustCompile(`(?m)` + runnerRefusalLine)

// ParseRunnerRefusal returns the version a guest's log says was refused. Only a
// version the grammar admits leaves here, so no other guest text can reach status.
func ParseRunnerRefusal(output []byte) (string, bool) {
	match := runnerRefusalMatcher.FindSubmatch(output)
	if match == nil {
		return "", false
	}
	return string(match[1]), true
}
