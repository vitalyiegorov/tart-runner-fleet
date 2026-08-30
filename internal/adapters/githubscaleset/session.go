package githubscaleset

import (
	"errors"
	"time"

	"github.com/actions/scaleset"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// The ingest failure vocabulary is closed. A reason is the only part of a
// broker failure an operator may ever see through the admin API, so it can
// never carry a token, a JIT configuration, or an upstream response body.
const (
	// ReasonSessionExpired marks a broker session GitHub has invalidated. The
	// same session handle can never succeed again; it must be recreated.
	ReasonSessionExpired = "session_expired"
	// ReasonSessionReleaseFailed marks a failed session whose broker-side
	// release did not complete, so recreation was withheld for this attempt.
	ReasonSessionReleaseFailed = "session_release_failed"
	// ReasonSessionCreateFailed marks a binding left without a usable session
	// because the replacement could not be opened.
	ReasonSessionCreateFailed = "session_create_failed"
	// ReasonRecreatedAfterFailures marks the bounded escalation: the failure
	// could not be proven terminal, but the session exceeded its consecutive
	// failure count or failure window and was discarded anyway.
	ReasonRecreatedAfterFailures = "recreated_after_failures"
	// ReasonMessagePollFailed marks an ordinary long-poll failure.
	ReasonMessagePollFailed = "message_poll_failed"
	// ReasonRateLimited marks a failure GitHub answered with its primary or a
	// secondary rate limit. The remedy is backoff, and a count that climbs
	// means this node polls too aggressively — a different operator response
	// from every other transient, which is why it stopped degrading into
	// ReasonMessagePollFailed (issue #259 measured ~70 unattributable ingest
	// failures a day reaching stderr as one reason).
	ReasonRateLimited = "rate_limited"
	// ReasonServerError marks a 5xx GitHub answered with: the network path and
	// credentials are fine and the fault is GitHub's own.
	ReasonServerError = "server_error"
	// ReasonQueueObservationFailed marks an unavailable REST queue observation.
	ReasonQueueObservationFailed = "queue_observation_failed"
	// ReasonQueueObservationStale marks a REST queue observation that is
	// coherent but older than its freshness budget.
	ReasonQueueObservationStale = "queue_observation_stale"
	// ReasonQueueReconcileFailed marks a REST queue snapshot that could not be
	// reconciled durably.
	ReasonQueueReconcileFailed = "queue_reconcile_failed"
	// ReasonDemandCommitConflict marks a broker message the durable store
	// refused. It is not a network condition and no retry of the same message
	// can clear it: the fleet holds state the message contradicts, and until
	// that is resolved every redelivery fails the same way. Degrading it to
	// ReasonMessagePollFailed reported a durable write conflict as broker
	// flapping for three days (issue #165), which is the opposite operator
	// response.
	ReasonDemandCommitConflict = "demand_commit_conflict"
)

// The ingest PHASE vocabulary is closed for the same reason the failure reasons
// are: it is rendered, and an open one is unbounded cardinality. It answers a
// question the reason alone cannot when a later phase overrides an earlier
// classification — whether the fleet failed to hear GitHub, to hand a session
// back, or to open a new one.
const (
	PhasePoll    = "poll"
	PhaseRelease = "release"
	PhaseCreate  = "create"
	PhaseObserve = "observe"
	PhaseCommit  = "commit"
)

// IngestFailure is the whole bounded account of one ingest failure: what it was
// classified as, which phase it happened in, and what GitHub actually answered.
//
// It exists because the reason alone identified nothing. Between July and
// 2026-08 this fleet emitted 2,065 warnings reading `reason=message_poll_failed`
// with no status, no correlation id, and no phase — which is the same as
// emitting none, because every one of them is consistent with a transient
// network blip, a misconfigured App, a throttle, and a broker outage (issue
// #292). A status code and GitHub's own request id separate those in one line.
//
// Every field is safe to render. Status and RetryAfter are numbers GitHub put in
// a response header; RequestID is the opaque `x-github-request-id` correlation
// token, which carries no credential and is the only handle GitHub support will
// accept. The upstream body, the JIT configuration and the installation token
// never appear here — they stay inside SessionFailure.Cause, reachable by
// errors.As and by nothing that writes.
type IngestFailure struct {
	// Reason is the closed-vocabulary classification. It is the metric label and
	// the only field that existed before issue #292.
	Reason string
	// Phase is where in the session's life the failure happened. It is not always
	// derivable from Reason: a poll that failed and then could not be released
	// reports session_release_failed, and Poll below keeps what was lost.
	Phase string
	// Poll is the classification of the underlying poll failure when a later
	// phase overrode it, and empty when Reason already names the whole story.
	// A rate limit that then fails to release is a throttle to back off from; a
	// 404 that then fails to release is a dead session. Both reported
	// session_release_failed and nothing else.
	Poll string
	// Status is the HTTP status GitHub answered with, or 0 when the failure never
	// reached an HTTP response.
	Status int
	// RequestID is GitHub's `x-github-request-id`, or empty. It is the handle an
	// operator quotes upstream.
	RequestID string
	// RetryAfter is how long GitHub asked the fleet to wait, or 0.
	RetryAfter time.Duration
}

// phaseFor names the phase a reason belongs to. Every reason maps to exactly
// one, and an unrecognised reason is a poll: ingestion's default activity is
// listening, and the catch-all classification is a failed listen.
func phaseFor(reason string) string {
	switch reason {
	case ReasonSessionReleaseFailed:
		return PhaseRelease
	case ReasonSessionCreateFailed:
		return PhaseCreate
	case ReasonQueueObservationFailed, ReasonQueueObservationStale, ReasonQueueReconcileFailed:
		return PhaseObserve
	case ReasonDemandCommitConflict:
		return PhaseCommit
	default:
		return PhasePoll
	}
}

// DescribeIngestFailure reduces any ingest failure to the whole bounded account.
// It is total, and its Reason is exactly what IngestFailureDetail returns.
func DescribeIngestFailure(err error) IngestFailure {
	reason := IngestFailureDetail(err)
	if reason == "" {
		return IngestFailure{}
	}
	failure := IngestFailure{Reason: reason, Phase: phaseFor(reason)}
	var session *SessionFailure
	if errors.As(err, &session) && session.Poll != reason && ValidFailureReason(session.Poll) {
		failure.Poll = session.Poll
	}
	var api *APIError
	if errors.As(err, &api) {
		failure.Status = api.Status
		failure.RequestID = api.RequestID
		failure.RetryAfter = api.RetryAfter
	}
	return failure
}

// ValidFailureReason guards the closed vocabulary at every recording site so an
// unclassified string can never reach health, the admin API, or a log line.
func ValidFailureReason(reason string) bool {
	switch reason {
	case ReasonSessionExpired, ReasonSessionReleaseFailed, ReasonSessionCreateFailed,
		ReasonRecreatedAfterFailures, ReasonMessagePollFailed, ReasonRateLimited,
		ReasonServerError, ReasonQueueObservationFailed,
		ReasonQueueObservationStale, ReasonQueueReconcileFailed, ReasonDemandCommitConflict:
		return true
	default:
		return false
	}
}

// SessionFailure pairs a closed-vocabulary reason with the concrete broker
// failure. Only Reason is renderable; Cause stays available to errors.Is and
// errors.As callers but never reaches the error message.
type SessionFailure struct {
	Reason string
	// Poll is the classification the poll itself earned, kept when a later
	// release or create failure overrode Reason. Without it a rate-limited poll
	// and a dead session are the same line (issue #292).
	Poll  string
	Cause error
}

func (e *SessionFailure) Error() string {
	if e == nil {
		return "<nil>"
	}
	return "scale-set session failure: " + e.Reason
}

func (e *SessionFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// FailureReason exposes the bounded diagnostic to the service failure hook. An
// unrecognized reason is withheld rather than logged.
func (e *SessionFailure) FailureReason() string {
	if e == nil || !ValidFailureReason(e.Reason) {
		return ""
	}
	return e.Reason
}

// SessionTerminal reports whether err proves this broker session can never
// succeed again, so retrying the same handle is pointless and the only recovery
// is to discard it and create a replacement.
//
// The official client refreshes an expired message-queue token in place and
// only surfaces MessageQueueTokenExpiredError when the refreshed token is
// rejected again. A 404 means GitHub no longer knows the session identity and a
// 409 means another session acquired the scale set. Rate limits, broker 5xx,
// secondary limits, deadlines, and runner-scoped errors are transient and must
// keep the current session.
func SessionTerminal(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, scaleset.MessageQueueTokenExpiredError) {
		return true
	}
	var api *APIError
	if !errors.As(err, &api) {
		return false
	}
	return api.Kind == NotFound || api.Kind == Conflict || api.Kind == Authentication
}

// IngestFailureDetail reduces any ingest failure to one closed-vocabulary
// reason. It is total: an unrecognized failure degrades to the generic poll
// reason rather than exposing upstream text.
//
// A durable commit conflict is classified before that degradation. It reaches
// here as operations.ErrConflict from the store, carries nothing renderable, and
// means something categorically different from a failed poll: the fleet, not the
// network, refused the message, and no amount of redelivery will change that.
func IngestFailureDetail(err error) string {
	if err == nil {
		return ""
	}
	var failure *SessionFailure
	if errors.As(err, &failure) && ValidFailureReason(failure.Reason) {
		return failure.Reason
	}
	if SessionTerminal(err) {
		return ReasonSessionExpired
	}
	if errors.Is(err, operations.ErrConflict) {
		return ReasonDemandCommitConflict
	}
	// A rate limit and a server error are different conditions with different
	// remedies, and the kinds already existed on APIError — the same judgement
	// SessionTerminal makes one branch above. A secondary limit arrives as an
	// Authorization kind carrying a Retry-After, which is how this fleet's own
	// adapter classifies it for retry purposes.
	var api *APIError
	if errors.As(err, &api) {
		if api.Kind == RateLimited || (api.Kind == Authorization && api.RetryAfter > 0) {
			return ReasonRateLimited
		}
		if api.Kind == Server {
			return ReasonServerError
		}
	}
	return ReasonMessagePollFailed
}

const (
	defaultSessionMaxIngestFailures = 5
	defaultSessionFailureWindow     = 5 * time.Minute
)

// SessionRecoveryPolicy bounds how long one broker session may keep failing
// before it is discarded even though its failure cannot be proven terminal.
// Without a bound, a session GitHub refuses to release pins an entire scope's
// ingestion to a dead handle until the daemon is restarted.
type SessionRecoveryPolicy struct {
	MaxConsecutiveFailures int
	FailureWindow          time.Duration
}

func (p SessionRecoveryPolicy) normalized() SessionRecoveryPolicy {
	if p.MaxConsecutiveFailures <= 0 {
		p.MaxConsecutiveFailures = defaultSessionMaxIngestFailures
	}
	if p.FailureWindow <= 0 {
		p.FailureWindow = defaultSessionFailureWindow
	}
	return p
}

// SessionFailureState is the immutable per-binding failure accumulator. It is
// carried by the caller so the policy stays pure.
type SessionFailureState struct {
	Consecutive int
	Since       time.Time
}

// SessionRecoveryDecision is the pure outcome of observing one ingest failure.
// Discard means the failed session must be abandoned even if the broker refuses
// to release it.
type SessionRecoveryDecision struct {
	State   SessionFailureState
	Reason  string
	Discard bool
}

// OnFailure advances the accumulator and classifies the failure. Time enters
// through now so the escalation deadline stays deterministic in tests.
func (p SessionRecoveryPolicy) OnFailure(state SessionFailureState, err error, now time.Time) SessionRecoveryDecision {
	policy := p.normalized()
	next := SessionFailureState{Consecutive: state.Consecutive + 1, Since: state.Since}
	if next.Since.IsZero() {
		next.Since = now
	}
	if SessionTerminal(err) {
		return SessionRecoveryDecision{State: next, Reason: ReasonSessionExpired, Discard: true}
	}
	// A durable commit conflict is classified before the bounded escalation, and
	// deliberately: escalation reports recreated_after_failures, which is a
	// statement about the SESSION, and a session is not what refused the message.
	// Reporting it that way is how a three-day durable defect read as broker
	// flapping (issue #165).
	if errors.Is(err, operations.ErrConflict) {
		return SessionRecoveryDecision{State: next, Reason: ReasonDemandCommitConflict}
	}
	if next.Consecutive >= policy.MaxConsecutiveFailures || now.Sub(next.Since) >= policy.FailureWindow {
		return SessionRecoveryDecision{State: next, Reason: ReasonRecreatedAfterFailures, Discard: true}
	}
	return SessionRecoveryDecision{State: next, Reason: ReasonMessagePollFailed}
}
