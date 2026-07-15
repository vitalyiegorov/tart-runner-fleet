package telemetry

import (
	"errors"
	"math"
	"sort"
	"sync"
	"time"
)

const (
	defaultReadyTickTTL           = 30 * time.Second
	defaultLiveTickTTL            = 2 * time.Minute
	defaultCriticalObservationTTL = 45 * time.Second
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
	Profiles               []string
	CriticalObservations   []string
}

type QueueMetrics struct {
	Count            int
	OldestEnqueuedAt time.Time
}

type InstanceMetrics struct {
	Count     int
	CPU       int
	MemoryMiB int
}

type ObservationMetric struct {
	Freshness  ObservationFreshness
	ObservedAt time.Time
}

type HostPressureMetric struct {
	AvailableMemoryMiB int64
	FreeDiskGiB        int64
	SwapUsedMiB        int64
	SwapOuts           int64
	CPUIdlePercent     float64
	LoadAverage        float64
	AdmissionAllowed   bool
	AdmissionReason    string
}

// Snapshot is an immutable point-in-time copy suitable for rendering.
type Snapshot struct {
	Revision           uint64
	Now                time.Time
	LastLoopTick       time.Time
	LastSuccessfulTick time.Time
	Mode               Mode
	Queues             map[string]QueueMetrics
	Instances          map[string]InstanceMetrics
	Observations       map[string]ObservationMetric
	OperationRetries   int
	DeadOperations     int
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
	profiles               map[string]struct{}
	critical               map[string]struct{}

	lastLoopTick       time.Time
	lastSuccessfulTick time.Time
	mode               Mode
	queues             map[string]QueueMetrics
	instances          map[string]InstanceMetrics
	observations       map[string]ObservationMetric
	operationRetries   int
	deadOperations     int
	hostPressure       HostPressureMetric
	revision           uint64
}

func NewHealth(clock Clock, config HealthConfig) (*Health, error) {
	if clock == nil {
		return nil, errClockRequired
	}
	if config.ReadyTickTTL < 0 || config.LiveTickTTL < 0 || config.CriticalObservationTTL < 0 {
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

	now := clock.Now()
	h := &Health{
		clock: clock, createdAt: now,
		readyTickTTL: config.ReadyTickTTL, liveTickTTL: config.LiveTickTTL,
		criticalObservationTTL: config.CriticalObservationTTL,
		profiles:               profiles, critical: critical, mode: ModeIdle,
		queues:       make(map[string]QueueMetrics, len(profiles)),
		instances:    make(map[string]InstanceMetrics, len(profiles)),
		observations: make(map[string]ObservationMetric, len(critical)),
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
	if !validFreshness(freshness) {
		return errInvalidObservation
	}
	now := h.clock.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.critical[name]; !ok {
		return errUnknownObservation
	}
	h.observations[name] = ObservationMetric{Freshness: freshness, ObservedAt: now}
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
	h.revision++
	return nil
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
		Mode: h.mode, Queues: cloneMap(h.queues), Instances: cloneMap(h.instances),
		Observations: cloneMap(h.observations), OperationRetries: h.operationRetries,
		DeadOperations: h.deadOperations, HostPressure: h.hostPressure, ObservationTTL: h.criticalObservationTTL,
	}
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
