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
	Occupancy      []Occupancy      `json:"occupancy,omitempty"`
	OccupancyCheck *Check           `json:"occupancyCheck,omitempty"`
	Queues         []Queue          `json:"queues"`
	Instances      []Instance       `json:"instances"`
	ScopeQueues    []ScopeQueue     `json:"scopeQueues,omitempty"`
	Observations   []Observation    `json:"observations"`
	Operations     OperationSummary `json:"operations"`
	HostPressure   HostPressure     `json:"hostPressure"`
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
