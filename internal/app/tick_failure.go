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
	// ReasonPlanCommitContended is an optimistic-concurrency loss: the durable
	// state moved between the observation this plan was built from and the
	// compare-and-set that would have persisted it. Nothing is broken and no
	// operator action applies — the next tick reads the newer state and proceeds.
	// It is named separately because the operator response is "ignore" while
	// plan_commit_failed's is "look at the database", and because the rate
	// limiter keys on reason: collapsing the two lets a frequent, harmless
	// conflict suppress a genuine write failure for a whole minute.
	ReasonPlanCommitContended = "plan_commit_contended"
	// ReasonPlanCommitRejected is a plan the durable layer refused as malformed:
	// an unknown profile, a drain of an instance whose durable state cannot be
	// drained, a dangling operation dependency. Unlike a conflict this does not
	// clear on its own — the same inputs produce the same rejection on every
	// tick — so it is the one commit failure that warrants an operator. During
	// the 2026-08-01 incident an eight-minute run of identical warnings could not
	// be told apart from ordinary contention because both logged the same token.
	ReasonPlanCommitRejected = "plan_commit_rejected"
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
	ReasonPlanCommitContended:        true,
	ReasonPlanCommitRejected:         true,
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

// Transient reports whether this failure clears without operator action and
// without any change to the inputs the tick observed. Only an optimistic
// concurrency loss qualifies: the writer that won already advanced the durable
// state, so re-observing is exactly the repair. Everything else in the
// vocabulary — a corrupt scheduler row, an unreachable store, a rejected plan —
// persists until something outside this loop changes.
func (e tickFailure) Transient() bool { return e.reason == ReasonPlanCommitContended }

// classifyTick wraps a tick error with its reason, preserving a nil error so
// call sites can classify unconditionally.
func classifyTick(reason string, err error) error {
	if err == nil {
		return nil
	}
	return tickFailure{reason: reason, err: err}
}
