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
	Profiles               []string
	CriticalObservations   []string
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
	Occupancy          []OccupancyMetric
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
	occupancy          []OccupancyMetric
	hostPressure       HostPressureMetric
	revision           uint64
}

func NewHealth(clock Clock, config HealthConfig) (*Health, error) {
	if clock == nil {
		return nil, errClockRequired
	}
	if config.ReadyTickTTL < 0 || config.LiveTickTTL < 0 || config.CriticalObservationTTL < 0 ||
		config.QueueSLO < 0 || config.QueueIncidentSLO < 0 {
		return nil, errInvalidHealthConfig
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
		Occupancy:         append([]OccupancyMetric(nil), h.occupancy...),
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
