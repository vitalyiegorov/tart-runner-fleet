package githubscaleset_test

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// TestAnIngestFailureNamesWhatGitHubActuallyAnswered is issue #292's ask 2.
//
// Between July and 2026-08 this fleet emitted 2,065 warnings reading
// `reason=message_poll_failed` and nothing else. Every one of them is equally
// consistent with a network blip, a throttle, a misconfigured App and a broker
// outage, so the line identified nothing at all. The status GitHub answered with
// and GitHub's own request id separate those in one reading.
func TestAnIngestFailureNamesWhatGitHubActuallyAnswered(t *testing.T) {
	failure := githubscaleset.DescribeIngestFailure(&githubscaleset.APIError{
		Kind: githubscaleset.Server, Status: http.StatusBadGateway, RequestID: "C4A2:1F3D"})

	if failure.Reason != githubscaleset.ReasonServerError {
		t.Fatalf("reason = %q", failure.Reason)
	}
	if failure.Status != http.StatusBadGateway {
		t.Fatalf("the status GitHub answered with must survive: %#v", failure)
	}
	if failure.RequestID != "C4A2:1F3D" {
		t.Fatalf("GitHub's request id is the handle an operator quotes upstream: %#v", failure)
	}
}

// A throttle carries how long GitHub asked the fleet to wait, which is the whole
// operator response to it.
func TestARateLimitCarriesItsBackoff(t *testing.T) {
	failure := githubscaleset.DescribeIngestFailure(&githubscaleset.APIError{
		Kind: githubscaleset.RateLimited, Status: http.StatusForbidden, RetryAfter: 42 * time.Second})

	if failure.Reason != githubscaleset.ReasonRateLimited || failure.RetryAfter != 42*time.Second {
		t.Fatalf("rate limit description = %#v", failure)
	}
}

// The phase answers what the reason cannot once a later phase overrides an
// earlier classification.
func TestEveryReasonBelongsToExactlyOnePhase(t *testing.T) {
	for reason, want := range map[string]string{
		githubscaleset.ReasonSessionReleaseFailed:   githubscaleset.PhaseRelease,
		githubscaleset.ReasonSessionCreateFailed:    githubscaleset.PhaseCreate,
		githubscaleset.ReasonQueueObservationFailed: githubscaleset.PhaseObserve,
		githubscaleset.ReasonQueueObservationStale:  githubscaleset.PhaseObserve,
		githubscaleset.ReasonQueueReconcileFailed:   githubscaleset.PhaseObserve,
		githubscaleset.ReasonDemandCommitConflict:   githubscaleset.PhaseCommit,
		githubscaleset.ReasonMessagePollFailed:      githubscaleset.PhasePoll,
		githubscaleset.ReasonRateLimited:            githubscaleset.PhasePoll,
		githubscaleset.ReasonServerError:            githubscaleset.PhasePoll,
		githubscaleset.ReasonSessionExpired:         githubscaleset.PhasePoll,
	} {
		got := githubscaleset.DescribeIngestFailure(&githubscaleset.SessionFailure{Reason: reason})
		if got.Phase != want {
			t.Fatalf("%s is in phase %q, want %q", reason, got.Phase, want)
		}
	}
}

// TestAReleaseFailureKeepsThePollClassificationItOverrode is the sharp half.
//
// A rate-limited poll whose session then cannot be released and a dead session
// whose release fails both reported `session_release_failed` and nothing else.
// They call for opposite operator responses — back off, or stop waiting for a
// session that is never coming back.
func TestAReleaseFailureKeepsThePollClassificationItOverrode(t *testing.T) {
	failure := githubscaleset.DescribeIngestFailure(&githubscaleset.SessionFailure{
		Reason: githubscaleset.ReasonSessionReleaseFailed,
		Poll:   githubscaleset.ReasonRateLimited,
		Cause:  &githubscaleset.APIError{Kind: githubscaleset.RateLimited, Status: http.StatusForbidden}})

	if failure.Reason != githubscaleset.ReasonSessionReleaseFailed || failure.Phase != githubscaleset.PhaseRelease {
		t.Fatalf("the phase that failed is the one reported: %#v", failure)
	}
	if failure.Poll != githubscaleset.ReasonRateLimited {
		t.Fatalf("the poll's own classification must survive the override: %#v", failure)
	}
}

// A poll reason that merely repeats the reason is not carried: a line that says
// the same thing twice is noise in the artifact issue #292 exists to make
// readable.
func TestARedundantPollClassificationIsNotRepeated(t *testing.T) {
	failure := githubscaleset.DescribeIngestFailure(&githubscaleset.SessionFailure{
		Reason: githubscaleset.ReasonMessagePollFailed, Poll: githubscaleset.ReasonMessagePollFailed})

	if failure.Poll != "" {
		t.Fatalf("a poll reason equal to the reason must be omitted: %#v", failure)
	}
}

// An unclassified poll reason is withheld exactly as an unclassified reason is.
// This field is fed by a classifier and upstream text must never reach a log.
func TestAnUnclassifiedPollReasonIsWithheld(t *testing.T) {
	failure := githubscaleset.DescribeIngestFailure(&githubscaleset.SessionFailure{
		Reason: githubscaleset.ReasonSessionReleaseFailed, Poll: "Bearer AAABBBCCC"})

	if failure.Poll != "" {
		t.Fatalf("unclassified text reached a rendered field: %#v", failure)
	}
}

// The description is total, and its reason is exactly what the older accessor
// returns — the metric label cannot drift from the log line.
func TestTheDescriptionAgreesWithTheReasonAccessor(t *testing.T) {
	for _, err := range []error{
		nil,
		errors.New("something upstream"),
		operations.ErrConflict,
		&githubscaleset.APIError{Kind: githubscaleset.Server, Status: http.StatusInternalServerError},
	} {
		if got, want := githubscaleset.DescribeIngestFailure(err).Reason, githubscaleset.IngestFailureDetail(err); got != want {
			t.Fatalf("description reason %q disagrees with detail %q", got, want)
		}
	}
	if described := githubscaleset.DescribeIngestFailure(nil); described != (githubscaleset.IngestFailure{}) {
		t.Fatalf("no error is no failure: %#v", described)
	}
}

// A failure that never reached an HTTP response carries no status, so nothing
// downstream can render a zero as though GitHub had sent it.
func TestAFailureWithNoResponseCarriesNoStatus(t *testing.T) {
	failure := githubscaleset.DescribeIngestFailure(errors.New("dial tcp: connection refused"))

	if failure.Reason != githubscaleset.ReasonMessagePollFailed {
		t.Fatalf("reason = %q", failure.Reason)
	}
	if failure.Status != 0 || failure.RequestID != "" || failure.RetryAfter != 0 {
		t.Fatalf("a failure with no response must invent none: %#v", failure)
	}
}
