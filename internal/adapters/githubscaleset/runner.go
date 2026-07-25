package githubscaleset

import (
	"errors"

	"github.com/actions/scaleset"
)

// The runner-administration failure vocabulary is closed for the same reason the
// ingest vocabulary is (see session.go): a drain failure is persisted on a
// durable operation and rendered to operators, so it can never carry an upstream
// response body, a token, or generated runner credentials.
//
// It is a separate predicate set from SessionTerminal on purpose. A session 404
// proves the broker forgot the session and a 401 proves the handle is finished,
// so both are terminal there. Runner administration must draw the opposite
// conclusion: a denied request proves nothing about the runner, and absence is
// never inferred from a failure at all — only a successful observation that
// returns no runner may conclude the deregistration post-condition already
// holds. Folding those together would let a permissions regression masquerade as
// completed cleanup.
const (
	// ReasonRunnerBusy marks GitHub's authoritative refusal to remove a runner it
	// still considers to be executing a job. ADR 0007 keeps cleanup retrying
	// through that refusal; naming it is what makes the wait explainable.
	ReasonRunnerBusy = "runner_busy"
	// ReasonRunnerForbidden marks an authentication or authorization failure on
	// runner administration. It fails closed: the runner's existence stays
	// unknown, so cleanup keeps waiting instead of assuming success.
	ReasonRunnerForbidden = "runner_forbidden"
	// ReasonRunnerAdminUnavailable marks a local precondition failure — no runner
	// administration port on the binding, or a name outside the scale-set token
	// grammar — so no GitHub call was ever made.
	ReasonRunnerAdminUnavailable = "runner_admin_unavailable"
	// ReasonRunnerScopeUnresolved marks an instance that could not be bound to
	// the exact registration scope and profile that emitted its demand.
	ReasonRunnerScopeUnresolved = "runner_scope_unresolved"
	// ReasonRunnerLookupFailed marks a failed pre-removal runner observation.
	ReasonRunnerLookupFailed = "runner_lookup_failed"
	// ReasonRunnerRemovalFailed marks a runner removal GitHub accepted the
	// request for but did not complete.
	ReasonRunnerRemovalFailed = "runner_removal_failed"
	// ReasonDeregisterFailed is the total fallback: an unrecognized failure is
	// still reported as a bounded reason rather than as upstream text.
	ReasonDeregisterFailed = "deregister_failed"
)

// These sentinels mark which step of runner administration failed. The step is
// the part an operator acts on — a failing observation points at scope or
// permissions, a failing removal points at the runner itself — and the official
// preview client does not classify HTTP status for these calls, so the step is
// also the most specific fact available without parsing upstream text.
var (
	ErrRunnerAdminUnavailable = errors.New("runner administration is unavailable")
	ErrRunnerLookup           = errors.New("scale-set runner observation failed")
	ErrRunnerRemoval          = errors.New("scale-set runner removal failed")
	ErrRunnerScopeUnresolved  = errors.New("runner registration scope unresolved")
)

// RunnerFailureReasons lists the closed vocabulary in classification order.
func RunnerFailureReasons() []string {
	return []string{ReasonRunnerBusy, ReasonRunnerForbidden, ReasonRunnerAdminUnavailable,
		ReasonRunnerScopeUnresolved, ReasonRunnerLookupFailed, ReasonRunnerRemovalFailed, ReasonDeregisterFailed}
}

// ValidRunnerFailureReason guards the closed vocabulary at every recording site
// so an unclassified string can never reach a durable operation, the admin API,
// or a log line.
func ValidRunnerFailureReason(reason string) bool {
	switch reason {
	case ReasonRunnerBusy, ReasonRunnerForbidden, ReasonRunnerAdminUnavailable, ReasonRunnerScopeUnresolved,
		ReasonRunnerLookupFailed, ReasonRunnerRemovalFailed, ReasonDeregisterFailed:
		return true
	default:
		return false
	}
}

// RunnerFailureDetail reduces any runner-administration failure to exactly one
// closed-vocabulary reason. It is total: an unrecognized failure degrades to the
// generic reason rather than exposing upstream text. Denied access is checked
// before the failing step because a permissions regression is what the operator
// must act on, whichever call surfaced it.
func RunnerFailureDetail(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, scaleset.JobStillRunningError):
		return ReasonRunnerBusy
	case errors.Is(err, ErrRunnerAdminUnavailable):
		return ReasonRunnerAdminUnavailable
	case errors.Is(err, ErrRunnerScopeUnresolved):
		return ReasonRunnerScopeUnresolved
	case runnerAccessDenied(err):
		return ReasonRunnerForbidden
	case errors.Is(err, ErrRunnerLookup):
		return ReasonRunnerLookupFailed
	case errors.Is(err, ErrRunnerRemoval):
		return ReasonRunnerRemovalFailed
	default:
		return ReasonDeregisterFailed
	}
}

func runnerAccessDenied(err error) bool {
	var api *APIError
	if !errors.As(err, &api) {
		return false
	}
	return api.Kind == Authentication || api.Kind == Authorization
}
