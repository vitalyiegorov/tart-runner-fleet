package domain

import (
	"regexp"
	"time"
)

// RunnerRefusal is a guest's own log saying the broker refuses its runner version:
// which version, which guest said so, and when the fleet read it (ADR 0094).
type RunnerRefusal struct {
	Version  string
	Instance string
	At       time.Time
}

// RunnerRefusalProbeInterval is how long a refused image launches nothing before
// one guest is spent finding out whether it still works. GitHub's enforcement began
// as four-hour brownouts (ADR 0041), so a refusal is never taken as permanent, and
// no job GitHub would now accept waits longer than this for the fleet to notice.
const RunnerRefusalProbeInterval = 30 * time.Minute

// RetryAt is when a probe may next launch from the refused image.
func (r RunnerRefusal) RetryAt() time.Time { return r.At.Add(RunnerRefusalProbeInterval) }

// RunnerRefusalLogCommand prints the refusal line from a guest's runner log, and
// nothing else from it. The log is the runner's standard output: its own status
// lines, never a job's. The path is the one guestbootstrap writes.
func RunnerRefusalLogCommand() []string {
	return []string{"/bin/sh", "-c",
		`grep -E '^An error occurred: Runner version v[0-9]+\.[0-9]+\.[0-9]+ is deprecated and cannot receive messages' ` +
			`"$HOME/actions-runner/.tart-runner-fleet/runner.log"`}
}

var runnerRefusalLine = regexp.MustCompile(`(?m)^An error occurred: Runner version v([0-9]+\.[0-9]+\.[0-9]+) is deprecated and cannot receive messages`)

// ParseRunnerRefusal returns the version a guest's log says was refused. Only a
// version the grammar admits leaves here, so no other guest text can reach status.
func ParseRunnerRefusal(output []byte) (string, bool) {
	match := runnerRefusalLine.FindSubmatch(output)
	if match == nil {
		return "", false
	}
	return string(match[1]), true
}
