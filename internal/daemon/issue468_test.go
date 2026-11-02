package daemon

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// TestIssue468ABindingFailureLineNamesItsTransportCause makes the statusless
// poll line that flooded every node on 2026-11-02 say which transport failure
// it was, and keeps an unclassified cause off the line entirely.
func TestIssue468ABindingFailureLineNamesItsTransportCause(t *testing.T) {
	var buffer bytes.Buffer
	now := time.Date(2026, 11, 2, 17, 54, 0, 0, time.UTC)
	reporter := newFailureReporter(&buffer, func() time.Time { return now })
	binding := app.Binding{StoreKey: 44, ScaleSetID: 9, Scope: "sudoku-repo",
		Profile: domain.Profile{ID: "linux-2x4", Route: "linux-portable-2x4", Platform: domain.PlatformLinux}}

	reporter.reportBinding(binding, githubscaleset.DescribeIngestFailure(&githubscaleset.SessionFailure{
		Reason: githubscaleset.ReasonMessagePollFailed, Cause: fmt.Errorf("poll message: %w", context.DeadlineExceeded)}))
	line := buffer.String()
	for _, want := range []string{"reason=" + githubscaleset.ReasonMessagePollFailed, "phase=" + githubscaleset.PhasePoll, "cause=" + githubscaleset.CauseTimeout} {
		if !strings.Contains(line, want) {
			t.Fatalf("binding failure line lacks %q: %q", want, line)
		}
	}
	if strings.Contains(line, "deadline exceeded") || strings.Contains(line, "status=") {
		t.Fatalf("the line rendered error text or an invented status: %q", line)
	}

	buffer.Reset()
	other := app.Binding{StoreKey: 45, ScaleSetID: 10, Scope: "sudoku-repo", Profile: binding.Profile}
	reporter.reportBinding(other, githubscaleset.IngestFailure{Reason: githubscaleset.ReasonMessagePollFailed,
		Phase: githubscaleset.PhasePoll, Cause: "read tcp 10.0.0.1: i/o timeout"})
	if line := buffer.String(); strings.Contains(line, "cause=") || strings.Contains(line, "10.0.0.1") {
		t.Fatalf("an unclassified cause reached the log: %q", line)
	}
}
