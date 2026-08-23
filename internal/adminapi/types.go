// Package adminapi defines the stable, read-only operator API shared by
// fleetd and fleetctl. These DTOs are compatibility contracts; persistence and
// domain structs must not leak into this package.
package adminapi

import "time"

const (
	APIVersion  = "fleet.v1"
	StatusPath  = "/v1/status"
	HealthPath  = "/healthz"
	ReadyPath   = "/readyz"
	MetricsPath = "/metrics"
)

type StatusEnvelope struct {
	APIVersion  string    `json:"apiVersion"`
	Kind        string    `json:"kind"`
	GeneratedAt time.Time `json:"generatedAt"`
	Revision    uint64    `json:"revision"`
	Data        Status    `json:"data"`
	Warnings    []Warning `json:"warnings"`
}

type Status struct {
	ControllerVersion  string    `json:"controllerVersion"`
	ControllerMode     string    `json:"controllerMode"`
	HostMode           string    `json:"hostMode"`
	LastLoopTick       time.Time `json:"lastLoopTick"`
	LastSuccessfulTick time.Time `json:"lastSuccessfulTick"`
	Live               Check     `json:"live"`
	Ready              Check     `json:"ready"`
	QueueSLO           *Check    `json:"queueSlo,omitempty"`
	// Occupancy is an additive fleet.v1 field: how long each live instance has
	// held its profile's resource vector, and whether that hold is past the
	// profile's ceiling while work that would fit it waits. An older daemon
	// omitted it entirely, which is why EffectiveOccupancy exists.
	Occupancy      []Occupancy `json:"occupancy,omitempty"`
	OccupancyCheck *Check      `json:"occupancyCheck,omitempty"`
	// Reservation is an additive fleet.v1 field: the vector the scheduler is
	// holding for its aged global-FIFO head, and WHICH of the two axes is
	// holding that head out of admission. Nil means no reservation is held; an
	// older daemon omitted it entirely, which is why EffectiveReservationCheck
	// exists. Issue #226 ran in production unobserved precisely because none of
	// this was published.
	Reservation      *Reservation `json:"reservation,omitempty"`
	ReservationCheck *Check       `json:"reservationCheck,omitempty"`
	// Stalled is an additive fleet.v1 field: the durable operations that are
	// still retrying and the instances still held in a cleanup state, with the
	// step, the attempt count, and the elapsed time an operator needs to name a
	// wedge without reading the database. An older daemon published none of it,
	// which is why EffectiveProgress exists.
	Stalled       []Stalled `json:"stalled,omitempty"`
	ProgressCheck *Check    `json:"progressCheck,omitempty"`
	// GuestSilences is an additive fleet.v1 field: every instance whose guest has
	// stopped answering the node's liveness probe, with the probe timeline and the
	// job that dies with it. An older daemon asked its guests nothing and published
	// nothing, which is why EffectiveGuestLiveness exists — and why issue #236's
	// eight production deaths left no fleet artifact at all.
	GuestSilences      []GuestSilence `json:"guestSilences,omitempty"`
	GuestLivenessCheck *Check         `json:"guestLivenessCheck,omitempty"`
	// RunnerImages is an additive fleet.v1 field: the `actions/runner` version
	// each of this node's guest images carries, and the floor GitHub's minimum
	// version enforcement judges it against. It exists so the version in service
	// is readable without SSH-ing into a guest — until issue #206 the fleet could
	// not answer the question at all, on either node, and a runner too old to
	// register looks exactly like a healthy idle fleet. An older daemon published
	// none of it, which is why EffectiveRunnerVersionCheck exists.
	RunnerImages       []RunnerImage `json:"runnerImages,omitempty"`
	RunnerVersionCheck *Check        `json:"runnerVersionCheck,omitempty"`
	// GuestConsole is an additive fleet.v1 field: whether this node boots Linux
	// guests and whether their serial consoles are written somewhere durable.
	// It exists because #236, #258 and #259 each ended at *trigger unidentified*
	// for one reason: the dead guest's own console across the death did not
	// exist. An older daemon published none of it, which is why
	// EffectiveGuestConsoleCheck exists.
	GuestConsole      *GuestConsole    `json:"guestConsole,omitempty"`
	GuestConsoleCheck *Check           `json:"guestConsoleCheck,omitempty"`
	Queues            []Queue          `json:"queues"`
	Instances         []Instance       `json:"instances"`
	ScopeQueues       []ScopeQueue     `json:"scopeQueues,omitempty"`
	Observations      []Observation    `json:"observations"`
	Operations        OperationSummary `json:"operations"`
	HostPressure      HostPressure     `json:"hostPressure"`
}

// HostPressure is the host evidence behind the latest admission decision.
// SwapOutRatePerSecond and SwapOutRateObserved are additive fleet.v1 fields:
// the swap guardrail refuses admission only when SwapUsedMiB exceeds the
// configured ceiling AND the host is measurably paging out, so the level alone
// cannot reproduce the decision, and SwapOuts is cumulative and undifferenceable
// from a single document. Observed=false means the rate could not be measured
// (no prior sample, a non-advancing clock, or a counter reset by a reboot) and
// the guard fell back to the level; it never means a quiet host.
type HostPressure struct {
	AvailableMemoryMiB   int64   `json:"availableMemoryMiB"`
	FreeDiskGiB          int64   `json:"freeDiskGiB"`
	SwapUsedMiB          int64   `json:"swapUsedMiB"`
	SwapOuts             int64   `json:"swapOuts"`
	SwapOutRatePerSecond float64 `json:"swapOutRatePerSecond"`
	SwapOutRateObserved  bool    `json:"swapOutRateObserved"`
	CPUIdlePercent       float64 `json:"cpuIdlePercent"`
	LoadAverage          float64 `json:"loadAverage"`
	AdmissionAllowed     bool    `json:"admissionAllowed"`
	AdmissionReason      string  `json:"admissionReason"`
}

type Check struct {
	OK      bool     `json:"ok"`
	Reasons []string `json:"reasons"`
}

// Occupancy is one instance's hold on the vector its profile reserves. It is the
// only per-instance row in the status document besides a dead letter, and it is
// per-instance for the reason ADR 0036 gives: the fault is one instance holding
// one vector too long, which no per-profile aggregate can express.
type Occupancy struct {
	Instance   string  `json:"instance"`
	Profile    string  `json:"profile"`
	Repo       string  `json:"repo,omitempty"`
	CPU        int     `json:"cpu"`
	MemoryMiB  int     `json:"memoryMiB"`
	AgeSeconds float64 `json:"ageSeconds"`
	// BudgetSeconds is zero for a profile with no ceiling. OverBudget and Warned
	// are then always false: an unbounded hold cannot be past a bound.
	BudgetSeconds       float64 `json:"budgetSeconds"`
	Warned              bool    `json:"warned"`
	OverBudget          bool    `json:"overBudget"`
	StarvesQueuedDemand bool    `json:"starvesQueuedDemand"`
}

// EffectiveQueueSLO keeps a new fleetctl compatible with an older fleet.v1
// daemon during atomic handoff. Older daemons did not emit queueSlo; their
// existing ready and queue fields remain authoritative until the new daemon is
// loaded.
func (s Status) EffectiveQueueSLO() Check {
	if s.QueueSLO == nil {
		return Check{OK: true, Reasons: []string{}}
	}
	return *s.QueueSLO
}

// EffectiveOccupancy keeps a new fleet CLI compatible with an older fleet.v1
// daemon during atomic handoff, exactly as EffectiveQueueSLO does. An older
// daemon measured no occupancy at all, and a daemon that cannot see a condition
// must never be rendered as having found none: absence is reported as an
// unspecified check rather than as a pass.
func (s Status) EffectiveOccupancy() Check {
	if s.OccupancyCheck == nil {
		return Check{OK: true, Reasons: []string{}}
	}
	return *s.OccupancyCheck
}

// GuestSilence is one instance whose guest has refused an unbroken run of
// liveness probes. It is per-instance for the reason ADR 0040 gives: the fault
// is one guest that stopped executing, and the vector it is still holding.
type GuestSilence struct {
	Instance  string `json:"instance"`
	Profile   string `json:"profile"`
	Repo      string `json:"repo,omitempty"`
	CPU       int    `json:"cpu"`
	MemoryMiB int    `json:"memoryMiB"`
	// Refusals and SilenceSeconds are the measurement; RequiredRefusals and
	// WindowSeconds are the bound it is judged against. The bound travels with the
	// measurement because two refusals is a hiccup and five is a verdict only if
	// something says which. Both bounds are zero on a node that probes nothing.
	Refusals         int     `json:"refusals"`
	SilenceSeconds   float64 `json:"silenceSeconds"`
	RequiredRefusals int     `json:"requiredRefusals"`
	WindowSeconds    float64 `json:"windowSeconds"`
	// Unresponsive is the verdict: the fleet has called this guest dead and is
	// reclaiming its vector.
	Unresponsive bool `json:"unresponsive"`
	// RunID and JobID name the job that dies with the guest, so a reclaimed runner
	// is distinguishable from a flake without opening the daemon log.
	RunID int64 `json:"runId,omitempty"`
	JobID int64 `json:"jobId,omitempty"`
}

// EffectiveGuestLiveness keeps a new fleet CLI compatible with an older fleet.v1
// daemon during atomic handoff, exactly as EffectiveOccupancy does. An older
// daemon never probed a guest, and a daemon that cannot see a condition must
// never render as having found none.
func (s Status) EffectiveGuestLiveness() Check {
	if s.GuestLivenessCheck == nil {
		return Check{OK: true, Reasons: []string{}}
	}
	return *s.GuestLivenessCheck
}

// Stalled is one operation that will not finish, one instance that will not let
// go, or both. Operation and Kind are empty for an instance whose drain has
// already dead-lettered: there is no operation left to name, and that is exactly
// when the instance is most stuck.
type Stalled struct {
	Operation string `json:"operation,omitempty"`
	Kind      string `json:"kind,omitempty"`
	// Code is the closed lifecycle vocabulary naming the STEP that keeps failing.
	Code            string  `json:"code,omitempty"`
	Instance        string  `json:"instance"`
	Attempts        int     `json:"attempts"`
	RetryingSeconds float64 `json:"retryingSeconds"`
	// DrainState is empty for an instance that is not tearing down.
	DrainState  string  `json:"drainState,omitempty"`
	HeldSeconds float64 `json:"heldSeconds"`
}

// EffectiveProgress keeps a new fleet CLI compatible with an older fleet.v1
// daemon during atomic handoff, exactly as EffectiveOccupancy does. An older
// daemon could not see a stalled drain at all — which is the condition issue
// #233 ran in production unnamed — so its absence is an unspecified check rather
// than a measured pass.
func (s Status) EffectiveProgress() Check {
	if s.ProgressCheck == nil {
		return Check{OK: true, Reasons: []string{}}
	}
	return *s.ProgressCheck
}

// EffectiveRunnerVersionCheck is the handoff accessor for the runner-version
// floor judgement. Its absence means the daemon predates issue #206 and never
// looked, which is reported as an unspecified pass rather than as a measured
// "every image is current" — the CLI must not invent a compliance answer an old
// daemon never gave.
//
// Note the deliberate asymmetry with the check itself: absence of the whole
// FIELD is a pass, because that is a version-handoff fact about the daemon.
// Absence of a declared VERSION on a published row is a failure, because that is
// a fact about an image nobody can vouch for.
func (s Status) EffectiveRunnerVersionCheck() Check {
	if s.RunnerVersionCheck == nil {
		return Check{OK: true, Reasons: []string{}}
	}
	return *s.RunnerVersionCheck
}

// RunnerImage is one guest image's `actions/runner` version and the floor it is
// judged against. Platform is a closed vocabulary (`linux`, `macOS`): a node has
// exactly one image per platform, and each answers only for the profiles it
// boots.
type RunnerImage struct {
	Platform string `json:"platform"`
	VM       string `json:"vm"`
	// Version is empty when the node declares none for this image, which is a
	// failing state and carries a Reason.
	Version string `json:"version,omitempty"`
	Floor   string `json:"floor"`
	// BelowFloor is the verdict, published beside the two versions because a
	// reader comparing them by eye is a reader who will eventually compare 2.9.0
	// against 2.10.0 and be wrong.
	BelowFloor bool `json:"belowFloor"`
	// Reason is empty when the image is compliant.
	Reason string `json:"reason,omitempty"`
}

// EffectiveReservationCheck is the same handoff accessor for the reservation
// judgement. An older daemon published no reservation at all — which is the
// condition issue #226 exploited — so its absence is reported as an unspecified
// pass rather than as a measured "nothing is held".
func (s Status) EffectiveReservationCheck() Check {
	if s.ReservationCheck == nil {
		return Check{OK: true, Reasons: []string{}}
	}
	return *s.ReservationCheck
}

// EffectiveGuestConsoleCheck is the handoff accessor for the guest-console
// judgement, on the same terms as every Effective accessor above: an older
// daemon that never published the field never looked, and an unspecified fact
// must not render as a measured failure.
func (s Status) EffectiveGuestConsoleCheck() Check {
	if s.GuestConsoleCheck == nil {
		return Check{OK: true, Reasons: []string{}}
	}
	return *s.GuestConsoleCheck
}

// GuestConsole is this node's answer to one question: when a Linux guest's
// kernel dies mid-job, will there be anything to read afterwards? BootsLinuxGuests
// is false on a node whose backend boots nothing of the kind, and the pair is
// then trivially satisfied. SerialLogConfigured is the whole point: false means
// the node is silent by construction about exactly the class #236 and #258 and
// #259 died in.
type GuestConsole struct {
	BootsLinuxGuests    bool `json:"bootsLinuxGuests"`
	SerialLogConfigured bool `json:"serialLogConfigured"`
}

// Reservation is the aged head the fleet is standing capacity by for.
//
// Axis is a closed vocabulary: `vector` (the head's resource vector does not fit
// the starvation envelope, so it waits on live instances to release),
// `repository_cap` (the vector fits and the head's own repository is at its cap,
// so it waits on one of that repository's instances to exit and freeing CPU
// cannot hasten it), `both`, `none` (neither term refuses the head — a fleet
// reserving a vector for work it could have started), or empty when the plan
// judged nothing.
type Reservation struct {
	Demand      string  `json:"demand"`
	Repo        string  `json:"repo"`
	Profile     string  `json:"profile"`
	CPU         int     `json:"cpu"`
	MemoryMiB   int     `json:"memoryMiB"`
	Slots       int     `json:"slots"`
	HeldSeconds float64 `json:"heldSeconds"`
	Axis        string  `json:"axis"`
	// LendsVector reports that the withheld vector is lent to work the head
	// outranks (ADR 0017 on the vector axis, ADR 0038 on the repository-cap
	// axis). False is the expensive case: capacity standing idle.
	LendsVector bool `json:"lendsVector"`
}

type Queue struct {
	Profile          string    `json:"profile"`
	Jobs             int       `json:"jobs"`
	OldestEnqueuedAt time.Time `json:"oldestEnqueuedAt"`
	OldestAgeSeconds float64   `json:"oldestAgeSeconds"`
}

// ScopeQueue attributes queue depth to the scope and scale set that own it. The
// aggregated Queue rows above cannot distinguish an idle scope from a busy one
// sharing a profile, which is the question an incident asks first.
type ScopeQueue struct {
	Scope            string    `json:"scope"`
	Profile          string    `json:"profile"`
	ScaleSetID       int64     `json:"scaleSetId"`
	Jobs             int       `json:"jobs"`
	OldestEnqueuedAt time.Time `json:"oldestEnqueuedAt"`
	OldestAgeSeconds float64   `json:"ageSeconds"`
	// Tiers is the same queue broken down by the priority tier each waiting
	// demand was classified into (issue #224). It is additive and absent both on
	// daemons older than the feature and on a fleet that declares no tier, so a
	// consumer must treat its absence as "no policy", never as "no demand".
	Tiers []QueueTier `json:"tiers,omitempty"`
}

// QueueTier is one priority tier's share of a scope queue. `tier` is the
// declared tier name, or `default` for the tier every unmatched demand lands in.
type QueueTier struct {
	Tier             string    `json:"tier"`
	Jobs             int       `json:"jobs"`
	OldestEnqueuedAt time.Time `json:"oldestEnqueuedAt"`
	OldestAgeSeconds float64   `json:"ageSeconds"`
}

type Instance struct {
	Profile   string `json:"profile"`
	Count     int    `json:"count"`
	CPU       int    `json:"cpu"`
	MemoryMiB int    `json:"memoryMiB"`
}

type Observation struct {
	Name       string    `json:"name"`
	Freshness  string    `json:"freshness"`
	ObservedAt time.Time `json:"observedAt"`
	AgeSeconds float64   `json:"ageSeconds"`
	// Detail is an optional bounded, credential-free diagnostic (e.g. the
	// scheduler plan block reason) so `fleetctl status`/`doctor` can explain a
	// non-fresh observation without exposing free-form error text.
	Detail string `json:"detail,omitempty"`
}

type OperationSummary struct {
	Retrying int `json:"retrying"`
	Dead     int `json:"dead"`
	// Failures explains the retrying and dead counts. Each entry pairs an
	// operation kind with one closed-vocabulary failure code — never stored
	// upstream text — so an operator can tell a busy-runner refusal from a
	// permission regression without opening the database. Absent on older
	// daemons, which published only the counts.
	Failures []OperationFailure `json:"failures,omitempty"`
	// DeadLetters names the individual parked operations behind the dead count, so
	// an operator can discharge one by identity instead of guessing it from logs.
	// Absent on older daemons and on a fleet with no dead letters.
	DeadLetters []DeadLetter `json:"deadLetters,omitempty"`
}

// OperationFailure is the bounded per-code view of operations that are not
// progressing. Attempts is the worst attempt count in the group, which is what
// distinguishes a momentary retry from a cleanup that has been stuck for hours.
type OperationFailure struct {
	Kind     string `json:"kind"`
	Code     string `json:"code"`
	Count    int    `json:"count"`
	Attempts int    `json:"attempts"`
}

type Warning struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	Remediation string `json:"remediation"`
}
