package app

// Closed vocabulary for scheduler tick failures. Each token names a distinct
// repair: a corrupt scheduler row, an unreachable durable store, a broker
// outage, and a refused plan commit are different incidents with different
// operator responses, and a reasonless warning cannot tell them apart.
//
// The rate limiter behind OnFailure keys on component and reason together, so
// classifying also stops one recurring cause from suppressing a newly appearing
// one for a whole minute.
const (
	ReasonEngineInvalid              = "engine_invalid"
	ReasonSchedulerStateUnreadable   = "scheduler_state_unreadable"
	ReasonSchedulerStateReseedFailed = "scheduler_state_reseed_failed"
	ReasonSchedulerStateCorrupt      = "scheduler_state_corrupt"
	ReasonDemandUnreadable           = "demand_unreadable"
	ReasonQueueSummaryUnreadable     = "queue_summary_unreadable"
	ReasonPlanCommitFailed           = "plan_commit_failed"
	// ReasonPlanInvalid separates a plan the scheduler could not form from a plan
	// it formed but could not persist. Commit reports both through one error, yet
	// they need different repairs: an unrecognized instance platform is inventory
	// reconciliation, a refused write is the database.
	ReasonPlanInvalid = "plan_invalid"
	// ReasonPlanCommitSuperseded names a lost optimistic race: ApplyPlan's
	// version guards found the snapshot stale because lifecycle progress landed
	// between the tick's read and its write. The next tick re-plans from fresh
	// state; this is snapshot concurrency working, not a store fault.
	ReasonPlanCommitSuperseded = "plan_commit_superseded"
)

// tickReasons is the closed set the failure hook may publish. A reason outside
// it is withheld exactly as the lifecycle and ingest paths withhold theirs, so
// no upstream text -- a store error string, a token, a JIT payload -- can reach
// the logs through this path.
var tickReasons = map[string]bool{
	ReasonEngineInvalid:              true,
	ReasonSchedulerStateUnreadable:   true,
	ReasonSchedulerStateReseedFailed: true,
	ReasonSchedulerStateCorrupt:      true,
	ReasonDemandUnreadable:           true,
	ReasonQueueSummaryUnreadable:     true,
	ReasonPlanCommitFailed:           true,
	ReasonPlanInvalid:                true,
	ReasonPlanCommitSuperseded:       true,
}

// tickFailure attaches a bounded reason to a scheduler tick error. It wraps
// rather than replaces, so every errors.Is check callers already perform against
// operations.ErrInvalid or an underlying store error keeps working: this
// classification is additive and changes no control flow.
type tickFailure struct {
	reason string
	err    error
}

func (e tickFailure) Error() string {
	if e.err == nil {
		return "scheduler tick failed: " + e.reason
	}
	return "scheduler tick failed (" + e.reason + "): " + e.err.Error()
}

func (e tickFailure) Unwrap() error { return e.err }

// FailureReason exposes the bounded diagnostic to the failure hook. An
// unrecognized reason is withheld rather than surfaced.
func (e tickFailure) FailureReason() string {
	if !tickReasons[e.reason] {
		return ""
	}
	return e.reason
}

// classifyTick wraps a tick error with its reason, preserving a nil error so
// call sites can classify unconditionally.
func classifyTick(reason string, err error) error {
	if err == nil {
		return nil
	}
	return tickFailure{reason: reason, err: err}
}
