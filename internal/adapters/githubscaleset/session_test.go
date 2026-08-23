package githubscaleset

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

const brokerSecret = "Bearer AAABBBCCC message queue token"

func TestSessionTerminalSeparatesTerminalFromTransientFailures(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		terminal bool
	}{
		{name: "no failure"},
		{name: "message queue token permanently rejected",
			err: fmt.Errorf("get next message: %w", scaleset.MessageQueueTokenExpiredError), terminal: true},
		{name: "session identity unknown to broker", err: &APIError{Kind: NotFound, Status: 404}, terminal: true},
		{name: "scale set acquired by another session", err: &APIError{Kind: Conflict, Status: 409}, terminal: true},
		{name: "session credentials rejected", err: &APIError{Kind: Authentication, Status: 401}, terminal: true},
		{name: "rate limited", err: &APIError{Kind: RateLimited, Status: 429}},
		{name: "broker unavailable", err: &APIError{Kind: Server, Status: 502}},
		{name: "secondary limit", err: &APIError{Kind: Authorization, Status: 403, RetryAfter: time.Second}},
		{name: "unexpected status", err: &APIError{Kind: Unexpected, Status: 418}},
		{name: "ephemeral runner already removed",
			err: fmt.Errorf("remove scale-set runner: %w", scaleset.RunnerNotFoundError)},
		{name: "long poll deadline", err: context.DeadlineExceeded},
		{name: "opaque session refresh failure", err: errors.New("failed to refresh message session")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := SessionTerminal(test.err); got != test.terminal {
				t.Fatalf("SessionTerminal(%v) = %v, want %v", test.err, got, test.terminal)
			}
		})
	}
}

func TestSessionRecoveryPolicyBoundsAmbiguousFailures(t *testing.T) {
	start := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	policy := SessionRecoveryPolicy{MaxConsecutiveFailures: 3, FailureWindow: 5 * time.Minute}
	ambiguous := errors.New("failed to refresh message session")
	tests := []struct {
		name     string
		policy   SessionRecoveryPolicy
		state    SessionFailureState
		err      error
		now      time.Time
		reason   string
		discard  bool
		expected int
	}{
		{name: "first ambiguous failure retries the same session", policy: policy, err: ambiguous, now: start,
			reason: ReasonMessagePollFailed, expected: 1},
		{name: "second ambiguous failure still retries", policy: policy,
			state: SessionFailureState{Consecutive: 1, Since: start}, err: ambiguous, now: start.Add(time.Second),
			reason: ReasonMessagePollFailed, expected: 2},
		{name: "consecutive bound forces a recreate", policy: policy,
			state: SessionFailureState{Consecutive: 2, Since: start}, err: ambiguous, now: start.Add(2 * time.Second),
			reason: ReasonRecreatedAfterFailures, discard: true, expected: 3},
		{name: "failure window forces a recreate before the count", policy: policy,
			state: SessionFailureState{Consecutive: 1, Since: start}, err: ambiguous, now: start.Add(5 * time.Minute),
			reason: ReasonRecreatedAfterFailures, discard: true, expected: 2},
		{name: "terminal failure discards on the first observation", policy: policy,
			err: &APIError{Kind: NotFound, Status: 404}, now: start,
			reason: ReasonSessionExpired, discard: true, expected: 1},
		{name: "zero policy normalizes to the shipped defaults",
			state: SessionFailureState{Consecutive: defaultSessionMaxIngestFailures - 1, Since: start},
			err:   ambiguous, now: start.Add(time.Second),
			reason: ReasonRecreatedAfterFailures, discard: true, expected: defaultSessionMaxIngestFailures},
		{name: "zero policy keeps retrying below the default bound",
			state: SessionFailureState{Consecutive: 1, Since: start}, err: ambiguous, now: start.Add(time.Second),
			reason: ReasonMessagePollFailed, expected: 2},
		// A session is not what refused the message, so escalation must not
		// rename a durable conflict after itself (issue #165). It keeps the
		// session and reports what actually happened, past the bound.
		{name: "durable commit conflict is named past the escalation bound", policy: policy,
			state:  SessionFailureState{Consecutive: 9, Since: start},
			err:    fmt.Errorf("commit demand message 100000004: %w", operations.ErrConflict),
			now:    start.Add(time.Hour),
			reason: ReasonDemandCommitConflict, expected: 10},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := test.policy.OnFailure(test.state, test.err, test.now)
			if decision.Reason != test.reason || decision.Discard != test.discard {
				t.Fatalf("decision = %+v, want reason %q discard %v", decision, test.reason, test.discard)
			}
			if decision.State.Consecutive != test.expected {
				t.Fatalf("consecutive = %d, want %d", decision.State.Consecutive, test.expected)
			}
			if decision.State.Since.IsZero() {
				t.Fatal("failure window start was not retained")
			}
		})
	}
}

func TestIngestFailureReasonVocabularyIsClosed(t *testing.T) {
	closed := []string{ReasonSessionExpired, ReasonSessionReleaseFailed, ReasonSessionCreateFailed,
		ReasonRecreatedAfterFailures, ReasonMessagePollFailed, ReasonQueueObservationFailed,
		ReasonQueueObservationStale, ReasonQueueReconcileFailed, ReasonDemandCommitConflict}
	for _, reason := range closed {
		if !ValidFailureReason(reason) {
			t.Fatalf("ValidFailureReason(%q) = false", reason)
		}
	}
	for _, reason := range []string{"", "session_expired ", brokerSecret, "arbitrary upstream body"} {
		if ValidFailureReason(reason) {
			t.Fatalf("ValidFailureReason(%q) = true", reason)
		}
	}
}

func TestIngestFailureDetailOnlyEmitsClosedVocabulary(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		detail string
	}{
		{name: "no failure", detail: ""},
		{name: "classified session failure",
			err:    &SessionFailure{Reason: ReasonRecreatedAfterFailures, Cause: errors.New(brokerSecret)},
			detail: ReasonRecreatedAfterFailures},
		{name: "wrapped session failure",
			err:    fmt.Errorf("ingest binding 7: %w", &SessionFailure{Reason: ReasonSessionCreateFailed}),
			detail: ReasonSessionCreateFailed},
		{name: "session failure carrying an unknown reason",
			err:    &SessionFailure{Reason: brokerSecret, Cause: errors.New(brokerSecret)},
			detail: ReasonMessagePollFailed},
		{name: "unclassified terminal failure",
			err:    fmt.Errorf("%s: %w", brokerSecret, scaleset.MessageQueueTokenExpiredError),
			detail: ReasonSessionExpired},
		{name: "unclassified transient failure", err: errors.New(brokerSecret), detail: ReasonMessagePollFailed},
		// A rate limit and a server error are different conditions an operator
		// responds to differently — backoff tuning versus waiting GitHub out —
		// and ~70 ingest failures a day were reaching stderr as one
		// indistinguishable reason (issue #259's follow-up). The kinds already
		// existed on APIError; the detail now names them instead of degrading
		// both into the generic poll failure.
		{name: "github rate limit",
			err:    &APIError{Kind: RateLimited, Status: 403},
			detail: ReasonRateLimited},
		{name: "github secondary rate limit",
			err:    &APIError{Kind: Authorization, Status: 403, RetryAfter: 30 * time.Second},
			detail: ReasonRateLimited},
		{name: "github server error",
			err:    &APIError{Kind: Server, Status: 502},
			detail: ReasonServerError},
		// A durable commit conflict is the fleet refusing the message, not the
		// network failing to deliver it. Reporting it as a poll failure is what
		// hid a three-day outage (issue #165).
		{name: "durable commit conflict",
			err:    fmt.Errorf("commit demand message 100000004: %w", operations.ErrConflict),
			detail: ReasonDemandCommitConflict},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			detail := IngestFailureDetail(test.err)
			if detail != test.detail {
				t.Fatalf("IngestFailureDetail() = %q, want %q", detail, test.detail)
			}
			if strings.Contains(detail, brokerSecret) {
				t.Fatalf("detail leaked upstream text: %q", detail)
			}
		})
	}
}

func TestSessionFailureKeepsUpstreamTextOutOfItsMessage(t *testing.T) {
	cause := errors.New(brokerSecret)
	failure := &SessionFailure{Reason: ReasonSessionExpired, Cause: cause}
	if strings.Contains(failure.Error(), brokerSecret) {
		t.Fatalf("SessionFailure.Error() leaked the cause: %q", failure.Error())
	}
	if !strings.Contains(failure.Error(), ReasonSessionExpired) {
		t.Fatalf("SessionFailure.Error() dropped the reason: %q", failure.Error())
	}
	if !errors.Is(failure, cause) {
		t.Fatal("SessionFailure lost its wrapped cause")
	}
	if failure.FailureReason() != ReasonSessionExpired {
		t.Fatalf("FailureReason() = %q", failure.FailureReason())
	}
	unclassified := &SessionFailure{Reason: brokerSecret, Cause: cause}
	if unclassified.FailureReason() != "" {
		t.Fatalf("unclassified FailureReason() = %q", unclassified.FailureReason())
	}
	var nilFailure *SessionFailure
	if nilFailure.Error() == "" || nilFailure.Unwrap() != nil || nilFailure.FailureReason() != "" {
		t.Fatalf("nil SessionFailure = %q, %v, %q", nilFailure.Error(), nilFailure.Unwrap(), nilFailure.FailureReason())
	}
}
