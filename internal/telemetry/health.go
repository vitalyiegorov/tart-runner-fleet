package telemetry

import (
	"errors"
	"math"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"
)

const (
	defaultReadyTickTTL           = 30 * time.Second
	defaultLiveTickTTL            = 2 * time.Minute
	defaultCriticalObservationTTL = 45 * time.Second
	defaultQueueSLO               = 10 * time.Minute
	defaultQueueIncidentSLO       = 30 * time.Minute
	// defaultStalledAttempts is how many failed attempts a durable operation may
	// spend before it is worth waking someone for. A healthy lifecycle operation
	// succeeds on its first attempt; the stop ladder has stopped being polite by
	// its sixth, so six failures is the earliest point at which repetition has
	// provably not helped (ADR 0039).
	defaultStalledAttempts = 6
	// defaultDrainHold is how long an instance may sit in a cleanup state before
	// the same is true of it. A healthy drain takes seconds. The 2026-08-10
	// incident spent 82 minutes in `deregistering` with nothing naming it.
	defaultDrainHold = 10 * time.Minute
)

var (
	errClockRequired       = errors.New("telemetry: clock is required")
	errInvalidHealthConfig = errors.New("telemetry: invalid health configuration")
	errUnknownObservation  = errors.New("telemetry: unknown observation")
	errInvalidObservation  = errors.New("telemetry: invalid observation freshness")
	errUnknownProfile      = errors.New("telemetry: unknown profile")
	errInvalidMetric       = errors.New("telemetry: invalid metric value")
	errInvalidMode         = errors.New("telemetry: invalid host mode")
)

// Clock makes all health and metric calculations deterministic in tests.
type Clock interface {
	Now() time.Time
}

type ObservationFreshness string

const (
	ObservationFresh       ObservationFreshness = "fresh"
	ObservationStale       ObservationFreshness = "stale"
	ObservationUnavailable ObservationFreshness = "unavailable"
)

type Mode string

const (
	ModeIdle        Mode = "idle"
	ModeLinux       Mode = "linux"
	ModeMacOS       Mode = "macos"
	ModeMixed       Mode = "mixed"
	ModeMaintenance Mode = "maintenance"
)

type HealthConfig struct {
	ReadyTickTTL           time.Duration
	LiveTickTTL            time.Duration
	CriticalObservationTTL time.Duration
	QueueSLO               time.Duration
	QueueIncidentSLO       time.Duration
	// StalledAttempts and DrainHold are the two thresholds the progress check
	// judges against: how many failed attempts an operation may spend, and how
	// long an instance may be held in a cleanup state.
	StalledAttempts      int
	DrainHold            time.Duration
	Profiles             []string
	CriticalObservations []string
	// FailureComponents is the closed set of control-plane loops whose failures
	// may be counted. Naming them here rather than inside telemetry keeps the
	// loop inventory with the process that starts the loops, exactly as
	// CriticalObservations keeps the observation inventory with its wiring.
	FailureComponents []string
}

type QueueMetrics struct {
	Count            int
	OldestEnqueuedAt time.Time
}

// ScopeQueueMetrics is one binding's queue depth attributed to its scope, so the
// per-profile aggregate above never hides an idle scope behind a busy one.
type ScopeQueueMetrics struct {
	Scope            string
	Profile          string
	ScaleSetID       int64
	Count            int
	OldestEnqueuedAt time.Time
	// Tiers is the same depth broken down by the priority tier each waiting
	// demand was classified into (issue #224). It is empty for a fleet that
	// declares no tier.
	Tiers []QueueTierMetrics
}

// QueueTierMetrics is one priority tier's share of one scope's queue. Rank
// travels with the name so a renderer orders tiers as the operator declared
// them without reading the configuration.
type QueueTierMetrics struct {
	Tier             string
	Rank             int
	Count            int
	OldestEnqueuedAt time.Time
}

type InstanceMetrics struct {
	Count     int
	CPU       int
	MemoryMiB int
}

// OccupancyMetric is one instance's hold on the resource vector its profile
// reserves. It is deliberately per-instance rather than per-profile: the
// condition it exists to make visible is a SINGLE instance holding a vector too
// long (issue #223), and a per-profile aggregate averages that away. The set is
// bounded by maxOccupancy for the same reason the dead-letter set is.
type OccupancyMetric struct {
	Instance   string
	Profile    string
	Repo       string
	CPU        int
	MemoryMiB  int
	Age        time.Duration
	Budget     time.Duration
	Warned     bool
	OverBudget bool
	// StarvesQueuedDemand reports that queued work would fit inside the vector
	// this instance is holding. An over-budget hold with nothing waiting is a
	// slow job; an over-budget hold with work that fits behind it is the fleet
	// incident, and only the second is worth waking anyone for.
	StarvesQueuedDemand bool
}

// ReservationMetric is the vector the scheduler is holding for its aged
// global-FIFO head, and WHY it is holding it.
//
// It exists because issue #226 was invisible on a live fleet. A reserved head
// its own repository cap was holding sterilized the residual for the entire
// runtime of the blocking job, and nothing published named the reservation, its
// repository, or which axis held it -- so no artifact would have shown the
// wedge, and only a deterministic simulator found it. `grep reservation` over
// the authority log returned nothing at all, because there was nothing to find.
//
// Axis is the closed vocabulary scheduler.ReservationAxis defines: `vector`,
// `repository_cap`, `both`, `none`, or empty when the plan judged nothing. It is
// the operator's whole diagnosis. A `vector` hold ends when live instances
// release; a `repository_cap` hold ends only when one of the head's OWN
// repository's instances exits, and freeing CPU cannot hasten it by a tick.
type ReservationMetric struct {
	// Demand and Repo name the head. They travel in the status document, never
	// as metric labels: a demand key is unbounded cardinality.
	Demand    string
	Repo      string
	Profile   string
	CPU       int
	MemoryMiB int
	Slots     int
	Held      time.Duration
	Axis      string
	// LendsVector reports that ADR 0017 or ADR 0038 releases the head's vector
	// to work it outranks rather than withholding it. A held reservation that
	// does NOT lend is the expensive one: it is standing capacity down.
	LendsVector bool
}

type ObservationMetric struct {
	Freshness  ObservationFreshness
	ObservedAt time.Time
	// Detail is a bounded, credential-free diagnostic (e.g. a scheduler plan
	// block reason). It is optional and empty for observations that carry none.
	Detail string
}

type HostPressureMetric struct {
	AvailableMemoryMiB int64
	FreeDiskGiB        int64
	SwapUsedMiB        int64
	SwapOuts           int64
	// SwapOutRatePerSecond and SwapOutRateObserved are the swap guardrail's
	// deciding signal: the level is refused only when the host is also paging
	// out (ADR 0018). An unobserved rate is a fail-closed fallback to the level,
	// not a measurement of a quiet host.
	SwapOutRatePerSecond float64
	SwapOutRateObserved  bool
	CPUIdlePercent       float64
	LoadAverage          float64
	AdmissionAllowed     bool
	AdmissionReason      string
}

// Snapshot is an immutable point-in-time copy suitable for rendering.
type Snapshot struct {
	Revision           uint64
	Now                time.Time
	LastLoopTick       time.Time
	LastSuccessfulTick time.Time
	Mode               Mode
	Queues             map[string]QueueMetrics
	ScopeQueues        []ScopeQueueMetrics
	Instances          map[string]InstanceMetrics
	Observations       map[string]ObservationMetric
	OperationRetries   int
	DeadOperations     int
	OperationFailures  []OperationFailure
	ComponentFailures  []ComponentFailure
	DeadLetters        []DeadLetter
	Stalled            []Stalled
	Occupancy          []OccupancyMetric
	GuestSilences      []GuestSilenceMetric
	RunnerImages       []RunnerImageMetric
	GuestConsole       *GuestConsoleMetric
	Reservation        *ReservationMetric
	HostPressure       HostPressureMetric
	ObservationTTL     time.Duration
}

type HealthResult struct {
	OK      bool
	Reasons []string
}

// Health owns the small in-memory telemetry state. Every read and update is
// protected by one mutex so a rendered snapshot cannot combine two ticks.
type Health struct {
	mu sync.RWMutex

	clock                  Clock
	createdAt              time.Time
	readyTickTTL           time.Duration
	liveTickTTL            time.Duration
	criticalObservationTTL time.Duration
	queueSLO               time.Duration
	queueIncidentSLO       time.Duration
	stalledAttempts        int
	drainHold              time.Duration
	profiles               map[string]struct{}
	critical               map[string]struct{}
	failureComponents      map[string]struct{}

	lastLoopTick       time.Time
	lastSuccessfulTick time.Time
	mode               Mode
	queues             map[string]QueueMetrics
	scopeQueues        []ScopeQueueMetrics
	instances          map[string]InstanceMetrics
	observations       map[string]ObservationMetric
	operationRetries   int
	deadOperations     int
	operationFailures  []OperationFailure
	componentFailures  map[componentFailureKey]int
	deadLetters        []DeadLetter
	stalled            []Stalled
	occupancy          []OccupancyMetric
	guestSilences      []GuestSilenceMetric
	runnerImages       []RunnerImageMetric
	guestConsole       *GuestConsoleMetric
	reservation        *ReservationMetric
	hostPressure       HostPressureMetric
	revision           uint64
}

func NewHealth(clock Clock, config HealthConfig) (*Health, error) {
	if clock == nil {
		return nil, errClockRequired
	}
	if config.ReadyTickTTL < 0 || config.LiveTickTTL < 0 || config.CriticalObservationTTL < 0 ||
		config.QueueSLO < 0 || config.QueueIncidentSLO < 0 || config.StalledAttempts < 0 || config.DrainHold < 0 {
		return nil, errInvalidHealthConfig
	}
	if config.StalledAttempts == 0 {
		config.StalledAttempts = defaultStalledAttempts
	}
	if config.DrainHold == 0 {
		config.DrainHold = defaultDrainHold
	}
	if config.ReadyTickTTL == 0 {
		config.ReadyTickTTL = defaultReadyTickTTL
	}
	if config.LiveTickTTL == 0 {
		config.LiveTickTTL = defaultLiveTickTTL
	}
	if config.CriticalObservationTTL == 0 {
		config.CriticalObservationTTL = defaultCriticalObservationTTL
	}
	if config.QueueSLO == 0 {
		config.QueueSLO = defaultQueueSLO
	}
	if config.QueueIncidentSLO == 0 {
		config.QueueIncidentSLO = defaultQueueIncidentSLO
	}
	if config.QueueIncidentSLO < config.QueueSLO {
		return nil, errInvalidHealthConfig
	}
	if config.LiveTickTTL <= config.ReadyTickTTL {
		return nil, errInvalidHealthConfig
	}
	profiles, ok := uniqueNames(config.Profiles)
	if !ok {
		return nil, errInvalidHealthConfig
	}
	critical, ok := uniqueNames(config.CriticalObservations)
	if !ok {
		return nil, errInvalidHealthConfig
	}
	failureComponents, ok := uniqueNames(config.FailureComponents)
	if !ok {
		return nil, errInvalidHealthConfig
	}

	now := clock.Now()
	h := &Health{
		clock: clock, createdAt: now,
		readyTickTTL: config.ReadyTickTTL, liveTickTTL: config.LiveTickTTL,
		criticalObservationTTL: config.CriticalObservationTTL,
		queueSLO:               config.QueueSLO, queueIncidentSLO: config.QueueIncidentSLO,
		stalledAttempts: config.StalledAttempts, drainHold: config.DrainHold,
		profiles: profiles, critical: critical, failureComponents: failureComponents, mode: ModeIdle,
		componentFailures: make(map[componentFailureKey]int, len(failureComponents)),
		queues:            make(map[string]QueueMetrics, len(profiles)),
		instances:         make(map[string]InstanceMetrics, len(profiles)),
		observations:      make(map[string]ObservationMetric, len(critical)),
	}
	for name := range critical {
		h.observations[name] = ObservationMetric{Freshness: ObservationUnavailable}
	}
	return h, nil
}

func uniqueNames(names []string) (map[string]struct{}, bool) {
	result := make(map[string]struct{}, len(names))
	for _, name := range names {
		if name == "" {
			return nil, false
		}
		if _, exists := result[name]; exists {
			return nil, false
		}
		result[name] = struct{}{}
	}
	return result, true
}

func (h *Health) RecordTick(successful bool) {
	now := h.clock.Now()
	h.mu.Lock()
	h.revision++
	h.lastLoopTick = now
	if successful {
		h.lastSuccessfulTick = now
	}
	h.mu.Unlock()
}

func (h *Health) RecordObservation(name string, freshness ObservationFreshness) error {
	return h.RecordObservationDetail(name, freshness, "")
}

// RecordObservationDetail records a critical observation together with a
// bounded, credential-free diagnostic detail. The detail is closed vocabulary
// (see scheduler.Plan.Reason) and must never carry wrapped error text.
func (h *Health) RecordObservationDetail(name string, freshness ObservationFreshness, detail string) error {
	if !validFreshness(freshness) {
		return errInvalidObservation
	}
	now := h.clock.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.critical[name]; !ok {
		return errUnknownObservation
	}
	h.observations[name] = ObservationMetric{Freshness: freshness, ObservedAt: now, Detail: detail}
	h.revision++
	return nil
}

func validFreshness(freshness ObservationFreshness) bool {
	return freshness == ObservationFresh || freshness == ObservationStale || freshness == ObservationUnavailable
}

func (h *Health) SetQueue(profile string, count int, oldestEnqueuedAt time.Time) error {
	if count < 0 {
		return errInvalidMetric
	}
	now := h.clock.Now()
	if oldestEnqueuedAt.After(now) || (count > 0 && oldestEnqueuedAt.IsZero()) {
		return errInvalidMetric
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.profiles[profile]; !ok {
		return errUnknownProfile
	}
	if count == 0 {
		oldestEnqueuedAt = time.Time{}
	}
	h.queues[profile] = QueueMetrics{Count: count, OldestEnqueuedAt: oldestEnqueuedAt}
	return nil
}

// SetScopeQueues replaces the per-scope breakdown. It is a whole-set replacement
// because a binding that disappears from configuration must stop being reported,
// and a partial update cannot express that.
func (h *Health) SetScopeQueues(rows []ScopeQueueMetrics) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, row := range rows {
		if row.Scope == "" || row.Profile == "" || row.Count < 0 {
			return errInvalidMetric
		}
		for _, tier := range row.Tiers {
			if tier.Tier == "" || tier.Count < 0 {
				return errInvalidMetric
			}
		}
	}
	h.scopeQueues = cloneScopeQueues(rows)
	h.revision++
	return nil
}

// cloneScopeQueues copies the rows AND the tier slice inside each one. A
// shallow copy would publish storage the caller still owns, which is exactly the
// data race a snapshot exists to prevent.
func cloneScopeQueues(rows []ScopeQueueMetrics) []ScopeQueueMetrics {
	out := append([]ScopeQueueMetrics(nil), rows...)
	for i := range out {
		out[i].Tiers = append([]QueueTierMetrics(nil), rows[i].Tiers...)
	}
	return out
}

func (h *Health) SetInstances(profile string, count, cpu, memoryMiB int) error {
	if count < 0 || cpu < 0 || memoryMiB < 0 {
		return errInvalidMetric
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.profiles[profile]; !ok {
		return errUnknownProfile
	}
	h.instances[profile] = InstanceMetrics{Count: count, CPU: cpu, MemoryMiB: memoryMiB}
	h.revision++
	return nil
}

// OperationFailure is the bounded per-code view of durable operations that are
// not progressing.
type OperationFailure struct {
	Kind     string
	Code     string
	Count    int
	Attempts int
}

// UnclassifiedFailureReason stands in for a failure whose cause the classifier
// could not name. An empty label would still be a series, so it is better to
// name the gap: a rising unclassified count is itself the signal that a failure
// path is missing from the closed vocabulary.
const UnclassifiedFailureReason = "unclassified"

// ComponentFailure counts one control-plane loop's failures under one bounded
// reason. It is monotonic for the life of the process.
//
// The reporter that logs these failures rate-limits to one line per component
// and reason per minute, so the log records that a loop failed but never how
// often. During the 2026-08-02 wedge that turned roughly one failure per tick
// into eight log lines, and nothing in the metrics distinguished a loop that
// failed once from one that had not committed anything for half an hour: the
// only ingest-adjacent series, fleet_observation_fresh, is a gauge that
// self-heals faster than a scrape interval.
type ComponentFailure struct {
	Component string
	Reason    string
	Count     int
}

// maxOperationFailures bounds both the status document and the metric label
// cardinality. The producing vocabulary is closed and far smaller than this.
const maxOperationFailures = 32

// boundedFailureToken is the grammar every published kind and code must satisfy.
// The durable side already reduces stored text to a closed vocabulary; this is
// the independent boundary check that keeps upstream text, URLs, and credential
// material out of the operator API even if a future producer regresses.
var boundedFailureToken = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}(:[a-z][a-z0-9_]{0,31})?$`)

// SetOperationFailures publishes why operations are retrying or dead. An empty
// aggregate is the healthy case; anything unbounded is rejected outright rather
// than truncated, so a rejected observation never masquerades as "no failures".
func (h *Health) SetOperationFailures(failures []OperationFailure) error {
	if len(failures) > maxOperationFailures {
		return errInvalidMetric
	}
	recorded := make([]OperationFailure, 0, len(failures))
	for _, failure := range failures {
		if failure.Count < 0 || failure.Attempts < 0 || !boundedFailureToken.MatchString(failure.Kind) ||
			!boundedFailureToken.MatchString(failure.Code) {
			return errInvalidMetric
		}
		recorded = append(recorded, failure)
	}
	h.mu.Lock()
	h.revision++
	h.operationFailures = recorded
	h.mu.Unlock()
	return nil
}

// DeadLetter is the identified view of one parked durable operation. It carries
// the identity the aggregate withholds, because an operator cannot discharge a
// count.
type DeadLetter struct {
	OperationID string
	Kind        string
	Code        string
	ResourceID  string
	Attempts    int
	Parked      bool
}

// maxDeadLetters bounds the published document. Dead-lettering takes 720 failed
// attempts, so a fleet with more parked operations than this has a systemic fault
// that the failure aggregate and the degraded observation already report.
const maxDeadLetters = 32

// boundedResourceID is the grammar a published resource identity must satisfy. It
// admits exactly the controller's own VM and operation identifiers, which keeps
// any future producer from routing arbitrary text through the operator API.
var boundedResourceID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

// SetDeadLetters publishes the parked operations an operator may discharge. An
// over-long or ungrammatical set is rejected outright rather than truncated, so a
// rejected observation can never masquerade as "nothing is parked" — the reading
// that would silently re-enable automatic updates on a fleet that is not parked
// at all.
func (h *Health) SetDeadLetters(letters []DeadLetter) error {
	if len(letters) > maxDeadLetters {
		return errInvalidMetric
	}
	recorded := make([]DeadLetter, 0, len(letters))
	for _, letter := range letters {
		if letter.Attempts < 0 || !boundedResourceID.MatchString(letter.OperationID) ||
			!boundedResourceID.MatchString(letter.ResourceID) ||
			!boundedFailureToken.MatchString(letter.Kind) || !boundedFailureToken.MatchString(letter.Code) {
			return errInvalidMetric
		}
		recorded = append(recorded, letter)
	}
	h.mu.Lock()
	h.revision++
	h.deadLetters = recorded
	h.mu.Unlock()
	return nil
}

// maxOccupancy bounds the published set. The fleet's physical envelope cannot
// hold this many instances at once on any node it runs on, so an over-long set
// is a producer fault rather than a busy host.
const maxOccupancy = 32

// SetOccupancy publishes how long each live instance has held its vector. It
// replaces the whole set rather than merging, because an instance that released
// its vector must disappear from the document; a merged map would keep
// reporting a hold that has ended. An over-long or ungrammatical set is rejected
// outright rather than truncated, so a rejected observation can never
// masquerade as "nothing is holding anything for too long".
func (h *Health) SetOccupancy(occupancy []OccupancyMetric) error {
	if len(occupancy) > maxOccupancy {
		return errInvalidMetric
	}
	recorded := make([]OccupancyMetric, 0, len(occupancy))
	for _, metric := range occupancy {
		if metric.Age < 0 || metric.Budget < 0 || metric.CPU < 0 || metric.MemoryMiB < 0 ||
			!boundedResourceID.MatchString(metric.Instance) {
			return errInvalidMetric
		}
		if _, ok := h.profiles[metric.Profile]; !ok {
			return errUnknownProfile
		}
		recorded = append(recorded, metric)
	}
	h.mu.Lock()
	h.revision++
	h.occupancy = recorded
	h.mu.Unlock()
	return nil
}

// Occupancy reports whether any instance is holding its vector past its
// profile's budget WHILE queued work that would fit it waits. Either half alone
// is not a fault: a long job is allowed to be long, and a queue is allowed to be
// deep. Together they are the 2026-08-09 incident, and they are what `fleet
// doctor` must name while it is happening rather than afterwards (ADR 0036).
func (h *Health) Occupancy() HealthResult {
	h.mu.RLock()
	defer h.mu.RUnlock()
	reasons := []string{}
	for _, metric := range h.occupancy {
		if !metric.OverBudget || !metric.StarvesQueuedDemand {
			continue
		}
		reasons = append(reasons, "instance "+metric.Instance+" of profile "+metric.Profile+
			" has held "+strconv.Itoa(metric.CPU)+" cpu / "+strconv.Itoa(metric.MemoryMiB)+
			" MiB for "+metric.Age.Round(time.Second).String()+" against a "+
			metric.Budget.Round(time.Second).String()+" budget, and queued work fits it")
	}
	return HealthResult{OK: len(reasons) == 0, Reasons: reasons}
}

// GuestSilenceMetric is one instance whose guest has refused an unbroken run of
// liveness probes: how many, for how long, against what bound, and whether the
// fleet has called it dead.
//
// It exists because issue #236's eight production deaths produced no fleet
// artifact at all. Every one of them was visible host-side within seconds — the
// guest agent stopped answering while `tart list` still said `running` — and
// nothing asked, so nothing could say. The set is per-instance and bounded by
// maxOccupancy for the same reason the occupancy set is: the fault is one
// instance, and an aggregate averages it away.
type GuestSilenceMetric struct {
	Instance string
	Profile  string
	Repo     string
	CPU      int
	MemoryMB int
	// Refusals is the length of the unbroken run and Silence is how long it has
	// lasted. RequiredRefusals and Window are the bound they are judged against,
	// published beside them because the measurement alone is unreadable.
	Refusals         int
	Silence          time.Duration
	RequiredRefusals int
	Window           time.Duration
	// Unresponsive is the verdict: both halves of the bound are satisfied and the
	// fleet is reclaiming this instance.
	Unresponsive bool
	// RunID and JobID name the job that dies with the guest. They are carried in
	// the status document rather than as metric labels, which are a closed
	// vocabulary by design.
	RunID int64
	JobID int64
}

// SetGuestSilences publishes every guest currently in a run of refusals. It
// replaces the whole set rather than merging: a guest that answered again must
// disappear from the document, and a merged map would keep reporting a silence
// that has ended. An over-long or ungrammatical set is rejected outright rather
// than truncated, so a rejected observation can never masquerade as "every guest
// is answering".
func (h *Health) SetGuestSilences(silences []GuestSilenceMetric) error {
	if len(silences) > maxOccupancy {
		return errInvalidMetric
	}
	recorded := make([]GuestSilenceMetric, 0, len(silences))
	for _, metric := range silences {
		if metric.Refusals < 0 || metric.Silence < 0 || metric.RequiredRefusals < 0 || metric.Window < 0 ||
			metric.CPU < 0 || metric.MemoryMB < 0 || !boundedResourceID.MatchString(metric.Instance) {
			return errInvalidMetric
		}
		if _, ok := h.profiles[metric.Profile]; !ok {
			return errUnknownProfile
		}
		recorded = append(recorded, metric)
	}
	h.mu.Lock()
	h.revision++
	h.guestSilences = recorded
	h.mu.Unlock()
	return nil
}

// GuestLiveness reports every instance the fleet has declared guest-dead. It
// fails only on the verdict, never on a partial run: a guest that has refused
// two of five probes is a guest the fleet is watching, and waking an operator
// for that would make the check unreadable within a week.
//
// Each reason names the instance, the job that died with it, and the probe
// timeline, because those are the three facts that were missing eight times.
func (h *Health) GuestLiveness() HealthResult {
	h.mu.RLock()
	defer h.mu.RUnlock()
	reasons := []string{}
	for _, metric := range h.guestSilences {
		if !metric.Unresponsive {
			continue
		}
		reasons = append(reasons, "instance "+metric.Instance+" of profile "+metric.Profile+
			" stopped answering its guest probe "+metric.Silence.Round(time.Second).String()+" ago ("+
			strconv.Itoa(metric.Refusals)+" consecutive refusals), holding "+strconv.Itoa(metric.CPU)+
			" cpu / "+strconv.Itoa(metric.MemoryMB)+" MiB for run "+strconv.FormatInt(metric.RunID, 10)+
			" job "+strconv.FormatInt(metric.JobID, 10))
	}
	return HealthResult{OK: len(reasons) == 0, Reasons: reasons}
}

// Stalled is one durable operation that is still retrying, one instance still
// held in a cleanup state, or both at once.
//
// It is the answer to "nothing named it". On 2026-08-10 `fleet doctor` reported
// the queue symptom and `PASS occupancy`, and the cause — a drain that had
// failed 67 times at the stop step for 82 minutes while holding the whole node —
// could only be read by copying the SQLite file off the host. Every field below
// was already durable; none of it was published.
type Stalled struct {
	// Operation and Kind are empty for an instance held in a cleanup state with no
	// operation still retrying on it, which is when it is most stuck.
	Operation string
	Kind      string
	// Code is the closed lifecycle failure vocabulary: the STEP that keeps
	// failing, which is the single most useful word in the diagnosis.
	Code     string
	Instance string
	Attempts int
	Retrying time.Duration
	// DrainState is the durable cleanup state, and Held how long the instance has
	// been in it.
	DrainState string
	Held       time.Duration
}

// maxStalled bounds the published set for the same reason maxDeadLetters does:
// no node this fleet runs on can hold this many instances at once, so an
// over-long set is a producer fault rather than a busy host.
const maxStalled = 32

// validDrainState is the closed set of durable cleanup states a published row
// may name. It is enforced rather than trusted because the state is rendered as
// a metric LABEL.
var validDrainState = map[string]bool{"": true, "draining": true, "deregistering": true, "stopping": true}

// SetStalled replaces the whole set. It is a replacement rather than a merge
// because an operation that finished must disappear from the document, and a
// merged map would go on reporting a wedge that has cleared. An over-long or
// ungrammatical set is rejected outright rather than truncated, so a rejected
// observation can never masquerade as "everything is progressing".
func (h *Health) SetStalled(stalled []Stalled) error {
	if len(stalled) > maxStalled {
		return errInvalidMetric
	}
	recorded := make([]Stalled, 0, len(stalled))
	for _, row := range stalled {
		if row.Attempts < 0 || row.Retrying < 0 || row.Held < 0 || !boundedResourceID.MatchString(row.Instance) ||
			!validDrainState[row.DrainState] {
			return errInvalidMetric
		}
		if row.Operation != "" && (!boundedResourceID.MatchString(row.Operation) ||
			!boundedFailureToken.MatchString(row.Kind) || !boundedFailureToken.MatchString(row.Code)) {
			return errInvalidMetric
		}
		recorded = append(recorded, row)
	}
	h.mu.Lock()
	h.revision++
	h.stalled = recorded
	h.mu.Unlock()
	return nil
}

// Progress reports the two conditions the 2026-08-10 incident presented and
// nothing published: an operation retrying far past the point where repetition
// could plausibly help, and an instance held in a cleanup state far past the
// point where a drain could plausibly still be working.
//
// Each reason names the instance, the step, the attempt count, and the elapsed
// time, because those four facts together are the diagnosis — and each of them
// had to be read out of a hand-copied database instead.
func (h *Health) Progress() HealthResult {
	h.mu.RLock()
	defer h.mu.RUnlock()
	reasons := []string{}
	for _, row := range h.stalled {
		if row.Operation != "" && row.Attempts >= h.stalledAttempts {
			reasons = append(reasons, "operation "+row.Operation+" ("+row.Kind+") has failed "+
				strconv.Itoa(row.Attempts)+" times at "+row.Code+" over "+row.Retrying.Round(time.Second).String()+
				", holding instance "+row.Instance)
		}
		if row.DrainState != "" && row.Held >= h.drainHold {
			reasons = append(reasons, "instance "+row.Instance+" has been held in "+row.DrainState+
				" for "+row.Held.Round(time.Second).String())
		}
	}
	return HealthResult{OK: len(reasons) == 0, Reasons: reasons}
}

// validReservationAxis is the closed vocabulary scheduler.ReservationAxis
// defines. It is enforced here rather than trusted, because the axis is rendered
// as a metric LABEL and an open vocabulary there is unbounded cardinality.
var validReservationAxis = map[string]bool{
	"": true, "vector": true, "repository_cap": true, "both": true, "none": true,
}

// SetReservation publishes the vector the scheduler is holding for its aged
// head, or clears it when no reservation is held. Nil is the "nothing held"
// value and is published as an ABSENCE rather than as a zero row, so a fleet
// holding nothing cannot be read as a fleet holding an unnamed something.
//
// An ungrammatical metric is rejected outright rather than clamped, for the same
// reason SetOccupancy rejects one: a mangled reservation must never masquerade
// as "no vector is standing idle".
func (h *Health) SetReservation(reservation *ReservationMetric) error {
	if reservation != nil {
		if reservation.Held < 0 || reservation.CPU < 0 || reservation.MemoryMiB < 0 || reservation.Slots < 0 {
			return errInvalidMetric
		}
		if !validReservationAxis[reservation.Axis] {
			return errInvalidMetric
		}
		if _, ok := h.profiles[reservation.Profile]; !ok {
			return errUnknownProfile
		}
	}
	h.mu.Lock()
	h.revision++
	h.reservation = cloneReservation(reservation)
	h.mu.Unlock()
	return nil
}

// cloneReservation keeps the published document from aliasing the caller's
// struct, exactly as the occupancy and dead-letter slices are copied.
func cloneReservation(reservation *ReservationMetric) *ReservationMetric {
	if reservation == nil {
		return nil
	}
	copied := *reservation
	return &copied
}

// Reservation reports whether a held reservation is standing capacity down
// without lending it. A reservation is normal and mostly cheap: the head is
// first in line and, on both of ADR 0017's and ADR 0038's axes, the vector it
// cannot use is lent to work it outranks. The expensive case is the one that
// does NOT lend, because that is an idle vector the size of the head's profile
// for however long the blocking job runs (ADR 0029's units), and it is the shape
// issue #226 ran in production unseen.
func (h *Health) Reservation() HealthResult {
	h.mu.RLock()
	defer h.mu.RUnlock()
	reasons := []string{}
	if reservation := h.reservation; reservation != nil && !reservation.LendsVector {
		reasons = append(reasons, "reservation for "+reservation.Demand+" of profile "+reservation.Profile+
			" has withheld "+strconv.Itoa(reservation.CPU)+" cpu / "+strconv.Itoa(reservation.MemoryMiB)+
			" MiB for "+reservation.Held.Round(time.Second).String()+" on the "+
			reservationAxisLabel(reservation.Axis)+" axis")
	}
	return HealthResult{OK: len(reasons) == 0, Reasons: reasons}
}

// reservationAxisLabel renders an unjudged plan's empty axis as a word rather
// than as nothing, so a diagnosis never reads as a missing sentence.
func reservationAxisLabel(axis string) string {
	if axis == "" {
		return "unjudged"
	}
	return axis
}

func (h *Health) SetOperations(retries, dead int) error {
	if retries < 0 || dead < 0 {
		return errInvalidMetric
	}
	h.mu.Lock()
	h.revision++
	h.operationRetries = retries
	h.deadOperations = dead
	h.mu.Unlock()
	return nil
}

func (h *Health) SetHostPressure(metric HostPressureMetric) error {
	if metric.AvailableMemoryMiB < 0 || metric.FreeDiskGiB < 0 || metric.SwapUsedMiB < 0 || metric.SwapOuts < 0 ||
		metric.CPUIdlePercent < 0 || metric.CPUIdlePercent > 100 || metric.LoadAverage < 0 ||
		metric.SwapOutRatePerSecond < 0 || math.IsNaN(metric.SwapOutRatePerSecond) ||
		math.IsInf(metric.SwapOutRatePerSecond, 0) ||
		math.IsNaN(metric.CPUIdlePercent) || math.IsInf(metric.CPUIdlePercent, 0) ||
		math.IsNaN(metric.LoadAverage) || math.IsInf(metric.LoadAverage, 0) || !validAdmissionReason(metric.AdmissionReason) {
		return errInvalidMetric
	}
	h.mu.Lock()
	h.revision++
	h.hostPressure = metric
	h.mu.Unlock()
	return nil
}

func validAdmissionReason(reason string) bool {
	switch reason {
	case "capacity available", "disk reserve", "memory reserve", "swap pressure", "cpu pressure":
		return true
	default:
		return false
	}
}

func (h *Health) SetMode(mode Mode) error {
	if mode != ModeIdle && mode != ModeLinux && mode != ModeMacOS && mode != ModeMixed && mode != ModeMaintenance {
		return errInvalidMode
	}
	h.mu.Lock()
	h.revision++
	h.mode = mode
	h.mu.Unlock()
	return nil
}

func (h *Health) Snapshot() Snapshot {
	now := h.clock.Now()
	h.mu.RLock()
	defer h.mu.RUnlock()
	return Snapshot{
		Revision: h.revision, Now: now, LastLoopTick: h.lastLoopTick, LastSuccessfulTick: h.lastSuccessfulTick,
		Mode: h.mode, Queues: cloneMap(h.queues), ScopeQueues: cloneScopeQueues(h.scopeQueues),
		Instances:    cloneMap(h.instances),
		Observations: cloneMap(h.observations), OperationRetries: h.operationRetries,
		DeadOperations: h.deadOperations, OperationFailures: append([]OperationFailure(nil), h.operationFailures...),
		ComponentFailures: h.sortedComponentFailures(),
		DeadLetters:       append([]DeadLetter(nil), h.deadLetters...),
		Stalled:           append([]Stalled(nil), h.stalled...),
		Occupancy:         append([]OccupancyMetric(nil), h.occupancy...),
		GuestSilences:     append([]GuestSilenceMetric(nil), h.guestSilences...),
		RunnerImages:      append([]RunnerImageMetric(nil), h.runnerImages...),
		GuestConsole:      h.guestConsole,
		Reservation:       cloneReservation(h.reservation),
		HostPressure:      h.hostPressure, ObservationTTL: h.criticalObservationTTL,
	}
}

type componentFailureKey struct{ component, reason string }

// RecordComponentFailure counts one loop failure. It admits only components the
// process declared, so a mistyped or upstream-derived name can never open a new
// time series, and it never rate-limits: undercounting is the deficiency this
// exists to repair.
func (h *Health) RecordComponentFailure(component, reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.failureComponents[component]; !ok {
		return
	}
	if reason == "" {
		reason = UnclassifiedFailureReason
	}
	h.componentFailures[componentFailureKey{component: component, reason: reason}]++
}

// sortedComponentFailures renders the counters in a deterministic order so
// operators, JSON consumers, and metric diffs never depend on map iteration.
func (h *Health) sortedComponentFailures() []ComponentFailure {
	if len(h.componentFailures) == 0 {
		return nil
	}
	result := make([]ComponentFailure, 0, len(h.componentFailures))
	for key, count := range h.componentFailures {
		result = append(result, ComponentFailure{Component: key.component, Reason: key.reason, Count: count})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Component != result[j].Component {
			return result[i].Component < result[j].Component
		}
		return result[i].Reason < result[j].Reason
	})
	return result
}

func cloneMap[K comparable, V any](source map[K]V) map[K]V {
	result := make(map[K]V, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func (h *Health) Live() HealthResult {
	now := h.clock.Now()
	h.mu.RLock()
	last := h.lastLoopTick
	if last.IsZero() {
		last = h.createdAt
	}
	ttl := h.liveTickTTL
	h.mu.RUnlock()
	if now.Sub(last) > ttl {
		return HealthResult{Reasons: []string{"event_loop_expired"}}
	}
	return HealthResult{OK: true}
}

func (h *Health) Ready() HealthResult {
	now := h.clock.Now()
	h.mu.RLock()
	defer h.mu.RUnlock()
	reasons := make(map[string]struct{})
	if h.lastSuccessfulTick.IsZero() {
		reasons["successful_tick_missing"] = struct{}{}
	} else if now.Sub(h.lastSuccessfulTick) > h.readyTickTTL {
		reasons["successful_tick_expired"] = struct{}{}
	}
	for name := range h.critical {
		observation := h.observations[name]
		switch observation.Freshness {
		case ObservationFresh:
			if observation.ObservedAt.IsZero() || now.Sub(observation.ObservedAt) > h.criticalObservationTTL {
				reasons["critical_observation_expired"] = struct{}{}
			}
		case ObservationStale:
			reasons["critical_observation_stale"] = struct{}{}
		default:
			reasons["critical_observation_unavailable"] = struct{}{}
		}
	}
	ordered := make([]string, 0, len(reasons))
	for reason := range reasons {
		ordered = append(ordered, reason)
	}
	sort.Strings(ordered)
	return HealthResult{OK: len(ordered) == 0, Reasons: ordered}
}

// QueueHealth reports service SLO degradation independently from authority
// readiness. Capacity backlog must page operators, but it must not make a
// healthy authority daemon fail updater verification or restart in a loop.
func (h *Health) QueueHealth() HealthResult {
	now := h.clock.Now()
	h.mu.RLock()
	defer h.mu.RUnlock()
	reasons := make(map[string]struct{})
	for _, queue := range h.queues {
		if queue.Count <= 0 || queue.OldestEnqueuedAt.IsZero() {
			continue
		}
		age := now.Sub(queue.OldestEnqueuedAt)
		if age > h.queueSLO {
			reasons["queue_slo_breached"] = struct{}{}
		}
		if age > h.queueIncidentSLO {
			reasons["queue_incident"] = struct{}{}
		}
	}
	ordered := make([]string, 0, len(reasons))
	for reason := range reasons {
		ordered = append(ordered, reason)
	}
	sort.Strings(ordered)
	return HealthResult{OK: len(ordered) == 0, Reasons: ordered}
}
