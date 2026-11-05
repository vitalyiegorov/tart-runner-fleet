package domain

import (
	"regexp"
	"time"
)

// RunnerRefusal is a guest's log naming its runner version as refused by the broker:
// which version, which guest, and when the fleet read it (ADR 0094).
//
// It is advisory and nothing else. A job runs as the user that owns that log and can
// write this line, so it may explain a hold the fleet has from other evidence, and it
// may never cause, lengthen or shorten one.
type RunnerRefusal struct {
	Version  string
	Instance string
	At       time.Time
}

// runnerRefusalLine is the complete line the runner prints, valid in both POSIX ERE
// and Go's syntax so the guest's grep and the host's matcher can never disagree. Matching
// all of it is what keeps a longer line that merely begins this way out.
const runnerRefusalLine = `^An error occurred: Runner version v([0-9]+\.[0-9]+\.[0-9]+) is deprecated and cannot receive messages\.$`

// RunnerRefusalLogCommand prints the refusal line from a guest's runner log, and
// nothing else from it. The log is the runner's standard output: its own status
// lines, never a job's. The path is the one guestbootstrap writes.
func RunnerRefusalLogCommand() []string {
	return []string{"/bin/sh", "-c", "grep -E '" + runnerRefusalLine + `' "$HOME/actions-runner/.tart-runner-fleet/runner.log"`}
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
