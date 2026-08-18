package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
)

const (
	defaultReadTimeout       = 5 * time.Second
	defaultWriteTimeout      = 10 * time.Second
	defaultIdleTimeout       = 15 * time.Second
	defaultReadHeaderTimeout = 2 * time.Second
	defaultMaxHeaderBytes    = 16 << 10
	maximumServerTimeout     = 30 * time.Second
	maximumHeaderBytes       = 1 << 20
)

var (
	errHealthRequired      = errors.New("telemetry: health is required")
	errInvalidServerConfig = errors.New("telemetry: invalid server configuration")
	errListenerRequired    = errors.New("telemetry: listener is required")
	errUnsafeListener      = errors.New("telemetry: listener must be local")
	errServerAlreadyServed = errors.New("telemetry: server already served")
	errServerFailed        = errors.New("telemetry: server failed")
)

// Mutator is the daemon-side authority for guarded operator mutations. It is
// wired only into the private Unix-socket server; the loopback health listener
// leaves it nil and therefore never registers a mutating route at all, so the
// read-only surface stays read-only by construction rather than by check.
type Mutator interface {
	DischargeDeadLetter(context.Context, adminapi.DischargeRequest) (adminapi.DischargeResult, error)
}

type ServerConfig struct {
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ReadHeaderTimeout time.Duration
	MaxHeaderBytes    int
	ControllerVersion string
	ControllerMode    string
	Mutator           Mutator
}

type Server struct {
	mu         sync.Mutex
	served     bool
	httpServer *http.Server
}

func NewServer(health *Health, config ServerConfig) (*Server, error) {
	if health == nil {
		return nil, errHealthRequired
	}
	if !validOptionalTimeout(config.ReadTimeout) || !validOptionalTimeout(config.WriteTimeout) ||
		!validOptionalTimeout(config.IdleTimeout) || !validOptionalTimeout(config.ReadHeaderTimeout) ||
		config.MaxHeaderBytes < 0 || config.MaxHeaderBytes > maximumHeaderBytes {
		return nil, errInvalidServerConfig
	}
	if config.ReadTimeout == 0 {
		config.ReadTimeout = defaultReadTimeout
	}
	if config.WriteTimeout == 0 {
		config.WriteTimeout = defaultWriteTimeout
	}
	if config.IdleTimeout == 0 {
		config.IdleTimeout = defaultIdleTimeout
	}
	if config.ReadHeaderTimeout == 0 {
		config.ReadHeaderTimeout = defaultReadHeaderTimeout
	}
	if config.MaxHeaderBytes == 0 {
		config.MaxHeaderBytes = defaultMaxHeaderBytes
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthHandler(health.Live, "live", "not_live"))
	mux.HandleFunc("/readyz", healthHandler(health.Ready, "ready", "not_ready"))
	mux.HandleFunc("/metrics", metricsHandler(health))
	mux.HandleFunc(adminapi.StatusPath, statusHandler(health, config.ControllerVersion, config.ControllerMode))
	if config.Mutator != nil {
		mux.HandleFunc(adminapi.DischargePath, dischargeHandler(config.Mutator))
	}
	return &Server{httpServer: &http.Server{
		Handler: mux, ReadTimeout: config.ReadTimeout, WriteTimeout: config.WriteTimeout,
		IdleTimeout: config.IdleTimeout, ReadHeaderTimeout: config.ReadHeaderTimeout,
		MaxHeaderBytes: config.MaxHeaderBytes,
	}}, nil
}

func statusHandler(health *Health, controllerVersion, controllerMode string) http.HandlerFunc {
	if controllerVersion == "" {
		controllerVersion = "dev"
	}
	if controllerMode == "" {
		controllerMode = "unknown"
	}
	return func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			response.Header().Set("Allow", http.MethodGet)
			http.Error(response, "method_not_allowed", http.StatusMethodNotAllowed)
			return
		}
		snapshot := health.Snapshot()
		etag := fmt.Sprintf(`"%d"`, snapshot.Revision)
		response.Header().Set("ETag", etag)
		response.Header().Set("Cache-Control", "no-store")
		if request.Header.Get("If-None-Match") == etag {
			response.WriteHeader(http.StatusNotModified)
			return
		}
		response.Header().Set("Content-Type", "application/json; charset=utf-8")
		envelope := statusEnvelope(snapshot, controllerVersion, controllerMode, health.Live(), health.Ready(),
			health.QueueHealth(), health.Occupancy(), health.Reservation(), health.Progress(), health.GuestLiveness(),
			health.RunnerVersions())
		if err := json.NewEncoder(response).Encode(envelope); err != nil {
			return
		}
	}
}

// dischargeHandler serves the one guarded mutation. It accepts POST only, reads a
// bounded body, and answers a refusal with its closed-vocabulary code so an
// operator learns which guard refused instead of reading an HTTP status. It never
// echoes the request reason or any upstream text back to the caller.
func dischargeHandler(mutator Mutator) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			response.Header().Set("Allow", http.MethodPost)
			http.Error(response, "method_not_allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(request.Body, adminapi.MaxRequestBytes+1))
		if err != nil || len(body) > adminapi.MaxRequestBytes {
			writeRefusal(response, adminapi.RefusalInvalidRequest)
			return
		}
		var decoded adminapi.DischargeRequest
		if json.Unmarshal(body, &decoded) != nil {
			writeRefusal(response, adminapi.RefusalInvalidRequest)
			return
		}
		result, err := mutator.DischargeDeadLetter(request.Context(), decoded)
		response.Header().Set("Cache-Control", "no-store")
		if err != nil {
			var refusal adminapi.Refusal
			if errors.As(err, &refusal) && adminapi.ValidRefusalCode(refusal.Code) {
				writeRefusal(response, refusal.Code)
				return
			}
			writeRefusal(response, adminapi.RefusalStoreUnavailable)
			return
		}
		response.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(response).Encode(result)
	}
}

func writeRefusal(response http.ResponseWriter, code string) {
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(adminapi.RefusalStatus(code))
	_ = json.NewEncoder(response).Encode(struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Code       string `json:"code"`
	}{adminapi.APIVersion, adminapi.RefusalKind, code})
}

// queueTiers projects the per-tier breakdown, ages computed against the same
// snapshot instant as every other age in the document.
func queueTiers(snapshot Snapshot, rows []QueueTierMetrics) []adminapi.QueueTier {
	if len(rows) == 0 {
		return nil
	}
	tiers := make([]adminapi.QueueTier, 0, len(rows))
	for _, row := range rows {
		age := time.Duration(0)
		if !row.OldestEnqueuedAt.IsZero() && snapshot.Now.After(row.OldestEnqueuedAt) {
			age = snapshot.Now.Sub(row.OldestEnqueuedAt)
		}
		tiers = append(tiers, adminapi.QueueTier{Tier: row.Tier, Jobs: row.Count,
			OldestEnqueuedAt: row.OldestEnqueuedAt, OldestAgeSeconds: age.Seconds()})
	}
	return tiers
}

func statusEnvelope(snapshot Snapshot, controllerVersion, controllerMode string,
	live, ready, queueSLO, occupancy, reservation, progress, guestLiveness, runnerVersions HealthResult,
) adminapi.StatusEnvelope {
	queues := make([]adminapi.Queue, 0, len(snapshot.Queues))
	for _, profile := range sortedKeys(snapshot.Queues) {
		metric := snapshot.Queues[profile]
		age := time.Duration(0)
		if !metric.OldestEnqueuedAt.IsZero() && snapshot.Now.After(metric.OldestEnqueuedAt) {
			age = snapshot.Now.Sub(metric.OldestEnqueuedAt)
		}
		queues = append(queues, adminapi.Queue{Profile: profile, Jobs: metric.Count,
			OldestEnqueuedAt: metric.OldestEnqueuedAt, OldestAgeSeconds: age.Seconds()})
	}
	scopeQueues := make([]adminapi.ScopeQueue, 0, len(snapshot.ScopeQueues))
	for _, row := range snapshot.ScopeQueues {
		age := time.Duration(0)
		if !row.OldestEnqueuedAt.IsZero() && snapshot.Now.After(row.OldestEnqueuedAt) {
			age = snapshot.Now.Sub(row.OldestEnqueuedAt)
		}
		scopeQueues = append(scopeQueues, adminapi.ScopeQueue{Scope: row.Scope, Profile: row.Profile,
			ScaleSetID: row.ScaleSetID, Jobs: row.Count, OldestEnqueuedAt: row.OldestEnqueuedAt,
			OldestAgeSeconds: age.Seconds(), Tiers: queueTiers(snapshot, row.Tiers)})
	}
	instances := make([]adminapi.Instance, 0, len(snapshot.Instances))
	for _, profile := range sortedKeys(snapshot.Instances) {
		metric := snapshot.Instances[profile]
		instances = append(instances, adminapi.Instance{Profile: profile, Count: metric.Count, CPU: metric.CPU, MemoryMiB: metric.MemoryMiB})
	}
	observations := make([]adminapi.Observation, 0, len(snapshot.Observations))
	for _, name := range sortedKeys(snapshot.Observations) {
		metric := snapshot.Observations[name]
		age := time.Duration(0)
		if !metric.ObservedAt.IsZero() && snapshot.Now.After(metric.ObservedAt) {
			age = snapshot.Now.Sub(metric.ObservedAt)
		}
		observations = append(observations, adminapi.Observation{Name: name, Freshness: string(metric.Freshness),
			ObservedAt: metric.ObservedAt, AgeSeconds: age.Seconds(), Detail: metric.Detail})
	}
	queueCheck := adminapi.Check{OK: queueSLO.OK, Reasons: nonNilStrings(queueSLO.Reasons)}
	occupancyCheck := adminapi.Check{OK: occupancy.OK, Reasons: nonNilStrings(occupancy.Reasons)}
	reservationCheck := adminapi.Check{OK: reservation.OK, Reasons: nonNilStrings(reservation.Reasons)}
	progressCheck := adminapi.Check{OK: progress.OK, Reasons: nonNilStrings(progress.Reasons)}
	guestLivenessCheck := adminapi.Check{OK: guestLiveness.OK, Reasons: nonNilStrings(guestLiveness.Reasons)}
	runnerVersionCheck := adminapi.Check{OK: runnerVersions.OK, Reasons: nonNilStrings(runnerVersions.Reasons)}
	return adminapi.StatusEnvelope{APIVersion: adminapi.APIVersion, Kind: "Status", GeneratedAt: snapshot.Now,
		Revision: snapshot.Revision, Warnings: []adminapi.Warning{}, Data: adminapi.Status{
			ControllerVersion: controllerVersion, ControllerMode: controllerMode, HostMode: string(snapshot.Mode),
			LastLoopTick: snapshot.LastLoopTick, LastSuccessfulTick: snapshot.LastSuccessfulTick,
			Live:      adminapi.Check{OK: live.OK, Reasons: nonNilStrings(live.Reasons)},
			Ready:     adminapi.Check{OK: ready.OK, Reasons: nonNilStrings(ready.Reasons)},
			QueueSLO:  &queueCheck,
			Occupancy: occupancyRows(snapshot), OccupancyCheck: &occupancyCheck,
			Reservation: reservationRow(snapshot), ReservationCheck: &reservationCheck,
			Stalled: stalledRows(snapshot), ProgressCheck: &progressCheck,
			GuestSilences: guestSilenceRows(snapshot), GuestLivenessCheck: &guestLivenessCheck,
			RunnerImages: runnerImageRows(snapshot), RunnerVersionCheck: &runnerVersionCheck,
			Queues: queues, ScopeQueues: scopeQueues, Instances: instances, Observations: observations,
			Operations: adminapi.OperationSummary{Retrying: snapshot.OperationRetries, Dead: snapshot.DeadOperations,
				Failures:    operationFailures(snapshot.OperationFailures),
				DeadLetters: deadLetters(snapshot.DeadLetters)},
			HostPressure: adminapi.HostPressure{AvailableMemoryMiB: snapshot.HostPressure.AvailableMemoryMiB,
				FreeDiskGiB: snapshot.HostPressure.FreeDiskGiB, SwapUsedMiB: snapshot.HostPressure.SwapUsedMiB,
				SwapOuts:             snapshot.HostPressure.SwapOuts,
				SwapOutRatePerSecond: snapshot.HostPressure.SwapOutRatePerSecond,
				SwapOutRateObserved:  snapshot.HostPressure.SwapOutRateObserved,
				CPUIdlePercent:       snapshot.HostPressure.CPUIdlePercent,
				LoadAverage:          snapshot.HostPressure.LoadAverage, AdmissionAllowed: snapshot.HostPressure.AdmissionAllowed,
				AdmissionReason: snapshot.HostPressure.AdmissionReason},
		}}
}

// occupancyRows projects each live instance's hold into the versioned DTO. Nil
// stays nil so a fleet holding nothing emits exactly the document older clients
// already saw.
func occupancyRows(snapshot Snapshot) []adminapi.Occupancy {
	if len(snapshot.Occupancy) == 0 {
		return nil
	}
	rows := make([]adminapi.Occupancy, 0, len(snapshot.Occupancy))
	for _, metric := range snapshot.Occupancy {
		rows = append(rows, adminapi.Occupancy{Instance: metric.Instance, Profile: metric.Profile, Repo: metric.Repo,
			CPU: metric.CPU, MemoryMiB: metric.MemoryMiB, AgeSeconds: metric.Age.Seconds(),
			BudgetSeconds: metric.Budget.Seconds(), Warned: metric.Warned, OverBudget: metric.OverBudget,
			StarvesQueuedDemand: metric.StarvesQueuedDemand})
	}
	return rows
}

// guestSilenceRows projects every guest that has stopped answering into the
// versioned DTO. Nil stays nil so a fleet whose guests all answer emits exactly
// the document older clients already saw.
func guestSilenceRows(snapshot Snapshot) []adminapi.GuestSilence {
	if len(snapshot.GuestSilences) == 0 {
		return nil
	}
	rows := make([]adminapi.GuestSilence, 0, len(snapshot.GuestSilences))
	for _, metric := range snapshot.GuestSilences {
		rows = append(rows, adminapi.GuestSilence{Instance: metric.Instance, Profile: metric.Profile,
			Repo: metric.Repo, CPU: metric.CPU, MemoryMiB: metric.MemoryMB, Refusals: metric.Refusals,
			SilenceSeconds: metric.Silence.Seconds(), RequiredRefusals: metric.RequiredRefusals,
			WindowSeconds: metric.Window.Seconds(), Unresponsive: metric.Unresponsive,
			RunID: metric.RunID, JobID: metric.JobID})
	}
	return rows
}

// runnerImageRows projects each base image's declared runner version into the
// versioned DTO. Nil stays nil so a daemon that has recorded nothing emits
// exactly the document older clients already saw.
func runnerImageRows(snapshot Snapshot) []adminapi.RunnerImage {
	if len(snapshot.RunnerImages) == 0 {
		return nil
	}
	rows := make([]adminapi.RunnerImage, 0, len(snapshot.RunnerImages))
	for _, metric := range snapshot.RunnerImages {
		rows = append(rows, adminapi.RunnerImage{Platform: metric.Platform, VM: metric.VM,
			Version: metric.Version, Floor: metric.Floor, BelowFloor: metric.Reason != "", Reason: metric.Reason})
	}
	return rows
}

// stalledRows projects the operations that will not finish and the instances
// that will not let go into the versioned DTO. Nil stays nil so a fleet where
// everything is progressing emits exactly the document older clients already
// saw.
func stalledRows(snapshot Snapshot) []adminapi.Stalled {
	if len(snapshot.Stalled) == 0 {
		return nil
	}
	rows := make([]adminapi.Stalled, 0, len(snapshot.Stalled))
	for _, row := range snapshot.Stalled {
		rows = append(rows, adminapi.Stalled{Operation: row.Operation, Kind: row.Kind, Code: row.Code,
			Instance: row.Instance, Attempts: row.Attempts, RetryingSeconds: row.Retrying.Seconds(),
			DrainState: row.DrainState, HeldSeconds: row.Held.Seconds()})
	}
	return rows
}

// reservationRow projects the held reservation into the versioned DTO. Nil stays
// nil so a fleet holding nothing emits exactly the document older clients saw --
// and, more importantly, so "no reservation" and "a reservation nobody
// published" stop being the same observation, which is what let issue #226 run
// unseen.
func reservationRow(snapshot Snapshot) *adminapi.Reservation {
	metric := snapshot.Reservation
	if metric == nil {
		return nil
	}
	return &adminapi.Reservation{Demand: metric.Demand, Repo: metric.Repo, Profile: metric.Profile,
		CPU: metric.CPU, MemoryMiB: metric.MemoryMiB, Slots: metric.Slots,
		HeldSeconds: metric.Held.Seconds(), Axis: metric.Axis, LendsVector: metric.LendsVector}
}

// operationFailures projects the bounded failure aggregate into the versioned
// DTO. Nil stays nil so a healthy fleet omits the field entirely and older
// clients see exactly the document they saw before.
func operationFailures(failures []OperationFailure) []adminapi.OperationFailure {
	if len(failures) == 0 {
		return nil
	}
	projected := make([]adminapi.OperationFailure, 0, len(failures))
	for _, failure := range failures {
		projected = append(projected, adminapi.OperationFailure{Kind: failure.Kind, Code: failure.Code,
			Count: failure.Count, Attempts: failure.Attempts})
	}
	return projected
}

// deadLetters projects the identified parked operations into the versioned DTO.
// Nil stays nil so a fleet with nothing parked emits exactly the document older
// clients already saw.
func deadLetters(letters []DeadLetter) []adminapi.DeadLetter {
	if len(letters) == 0 {
		return nil
	}
	projected := make([]adminapi.DeadLetter, 0, len(letters))
	for _, letter := range letters {
		projected = append(projected, adminapi.DeadLetter{OperationID: letter.OperationID, Kind: letter.Kind,
			Code: letter.Code, ResourceID: letter.ResourceID, Attempts: letter.Attempts, Parked: letter.Parked})
	}
	return projected
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func validOptionalTimeout(value time.Duration) bool {
	return value >= 0 && value <= maximumServerTimeout
}

func healthHandler(check func() HealthResult, healthy, unhealthy string) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			response.Header().Set("Allow", http.MethodGet)
			http.Error(response, "method_not_allowed", http.StatusMethodNotAllowed)
			return
		}
		result := check()
		response.Header().Set("Content-Type", "application/json; charset=utf-8")
		response.Header().Set("Cache-Control", "no-store")
		if result.OK {
			_, _ = io.WriteString(response, `{"status":"`+healthy+`"}`+"\n")
			return
		}
		response.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(response, `{"status":"`+unhealthy+`","reasons":[`)
		for index, reason := range result.Reasons {
			if index > 0 {
				_, _ = io.WriteString(response, ",")
			}
			_, _ = io.WriteString(response, strconv.Quote(reason))
		}
		_, _ = io.WriteString(response, "]}\n")
	}
}

func metricsHandler(health *Health) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			response.Header().Set("Allow", http.MethodGet)
			http.Error(response, "method_not_allowed", http.StatusMethodNotAllowed)
			return
		}
		response.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		response.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(response, renderMetrics(health.Snapshot()))
	}
}

func renderMetrics(snapshot Snapshot) string {
	var output strings.Builder
	writeHelpType := func(name, help, metricType string) {
		fmt.Fprintf(&output, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, metricType)
	}
	writeHelpType("fleet_queue_jobs", "Queued jobs by bounded runner profile.", "gauge")
	writeHelpType("fleet_queue_oldest_age_seconds", "Age of the oldest queued job by bounded runner profile.", "gauge")
	for _, profile := range sortedKeys(snapshot.Queues) {
		queue := snapshot.Queues[profile]
		age := time.Duration(0)
		if !queue.OldestEnqueuedAt.IsZero() && snapshot.Now.After(queue.OldestEnqueuedAt) {
			age = snapshot.Now.Sub(queue.OldestEnqueuedAt)
		}
		label := prometheusLabel(profile)
		fmt.Fprintf(&output, "fleet_queue_jobs{profile=%s} %d\n", label, queue.Count)
		fmt.Fprintf(&output, "fleet_queue_oldest_age_seconds{profile=%s} %s\n", label, seconds(age))
	}

	writeHelpType("fleet_instances", "Live instances by bounded runner profile.", "gauge")
	writeHelpType("fleet_instance_cpu", "Allocated virtual CPUs by bounded runner profile.", "gauge")
	writeHelpType("fleet_instance_memory_mib", "Allocated memory in MiB by bounded runner profile.", "gauge")
	for _, profile := range sortedKeys(snapshot.Instances) {
		instance := snapshot.Instances[profile]
		label := prometheusLabel(profile)
		fmt.Fprintf(&output, "fleet_instances{profile=%s} %d\n", label, instance.Count)
		fmt.Fprintf(&output, "fleet_instance_cpu{profile=%s} %d\n", label, instance.CPU)
		fmt.Fprintf(&output, "fleet_instance_memory_mib{profile=%s} %d\n", label, instance.MemoryMiB)
	}

	writeHelpType("fleet_operation_retries", "Durable operation retry count.", "gauge")
	fmt.Fprintf(&output, "fleet_operation_retries %d\n", snapshot.OperationRetries)
	writeHelpType("fleet_operations_dead", "Dead durable operation count.", "gauge")
	fmt.Fprintf(&output, "fleet_operations_dead %d\n", snapshot.DeadOperations)
	// Parked is the alertable subset: a dead letter whose resource cannot advance
	// without an operator running `fleet operations discharge`. It is also exactly
	// what `fleet update` discounts from the quiescence gate, so an alert on a
	// non-zero value is an alert on capacity nothing will reclaim on its own.
	writeHelpType("fleet_operations_parked", "Dead-lettered operations whose resource cannot advance without an operator.", "gauge")
	parked := 0
	for _, letter := range snapshot.DeadLetters {
		if letter.Parked {
			parked++
		}
	}
	fmt.Fprintf(&output, "fleet_operations_parked %d\n", parked)
	if len(snapshot.Occupancy) > 0 {
		// Per-instance rather than per-profile: the fault this exists for is ONE
		// instance holding a vector too long (issue #223), and an aggregate averages
		// exactly that away. Cardinality is bounded by maxOccupancy and by the
		// physical envelope — an instance is a VM, not a request.
		writeHelpType("fleet_instance_occupancy_seconds", "How long a live instance has held its profile's resource vector.", "gauge")
		for _, metric := range snapshot.Occupancy {
			fmt.Fprintf(&output, "fleet_instance_occupancy_seconds{profile=%s,instance=%s} %s\n",
				prometheusLabel(metric.Profile), prometheusLabel(metric.Instance), seconds(metric.Age))
		}
		// Publishing the budget beside the age is what makes the age readable: a
		// forty-minute hold is healthy on a macOS builder and a leak on a small
		// Linux profile, and an alert cannot tell them apart from the age alone.
		// Zero is a profile with no ceiling, never a ceiling of zero.
		writeHelpType("fleet_instance_occupancy_budget_seconds", "The occupancy ceiling of the instance's profile; 0 means no ceiling.", "gauge")
		for _, metric := range snapshot.Occupancy {
			fmt.Fprintf(&output, "fleet_instance_occupancy_budget_seconds{profile=%s,instance=%s} %s\n",
				prometheusLabel(metric.Profile), prometheusLabel(metric.Instance), seconds(metric.Budget))
		}
		// The alertable conjunction: over budget AND queued work would fit the held
		// vector. Either half alone is ordinary; together they are the incident.
		writeHelpType("fleet_instance_occupancy_starving", "1 when an over-budget instance holds a vector that queued demand would fit.", "gauge")
		for _, metric := range snapshot.Occupancy {
			fmt.Fprintf(&output, "fleet_instance_occupancy_starving{profile=%s,instance=%s} %d\n",
				prometheusLabel(metric.Profile), prometheusLabel(metric.Instance),
				boolGauge(metric.OverBudget && metric.StarvesQueuedDemand))
		}
	}
	if len(snapshot.GuestSilences) > 0 {
		// Per-instance for the same reason occupancy is: the fault is ONE guest that
		// stopped executing while still holding a vector. Cardinality is bounded by
		// maxOccupancy and by the physical envelope. The demand key is deliberately
		// not a label — it is unbounded — and travels in the status document.
		writeHelpType("fleet_instance_guest_silence_seconds", "How long an instance's guest has been refusing its liveness probe.", "gauge")
		for _, metric := range snapshot.GuestSilences {
			fmt.Fprintf(&output, "fleet_instance_guest_silence_seconds{profile=%s,instance=%s} %s\n",
				prometheusLabel(metric.Profile), prometheusLabel(metric.Instance), seconds(metric.Silence))
		}
		// The count and the bound travel together, because the count alone is
		// unreadable: two refusals is a hiccup against a five-refusal bound and a
		// verdict against a two-refusal one. Zero required refusals is a node that
		// probes nothing, never a node that declares every guest dead.
		writeHelpType("fleet_instance_guest_probe_refusals", "Consecutive liveness probes an instance's guest has refused.", "gauge")
		for _, metric := range snapshot.GuestSilences {
			fmt.Fprintf(&output, "fleet_instance_guest_probe_refusals{profile=%s,instance=%s} %d\n",
				prometheusLabel(metric.Profile), prometheusLabel(metric.Instance), metric.Refusals)
		}
		writeHelpType("fleet_instance_guest_probe_refusals_required", "Consecutive refusals this node requires before it declares a guest dead; 0 means it probes nothing.", "gauge")
		for _, metric := range snapshot.GuestSilences {
			fmt.Fprintf(&output, "fleet_instance_guest_probe_refusals_required{profile=%s,instance=%s} %d\n",
				prometheusLabel(metric.Profile), prometheusLabel(metric.Instance), metric.RequiredRefusals)
		}
		// The alertable fact, and the one nothing published eight times: the fleet
		// has declared this guest dead and is ending the job it was running.
		writeHelpType("fleet_instance_guest_unresponsive", "1 when the fleet has declared an instance's guest dead and is reclaiming its vector.", "gauge")
		for _, metric := range snapshot.GuestSilences {
			fmt.Fprintf(&output, "fleet_instance_guest_unresponsive{profile=%s,instance=%s} %d\n",
				prometheusLabel(metric.Profile), prometheusLabel(metric.Instance), boolGauge(metric.Unresponsive))
		}
	}
	if len(snapshot.RunnerImages) > 0 {
		// One series, one label, at most two rows per node. The version strings are
		// deliberately NOT labels: a version changes on every image rebuild, which
		// would churn the series and make a query for "is this node behind" depend
		// on knowing what the answer used to be. The verdict is the metric; the
		// versions travel in the status document, which is where an operator who
		// needs to know WHICH release to install reads them.
		writeHelpType("fleet_runner_image_below_floor",
			"1 when a base image's actions/runner version is below the enforcement floor, or is not declared at all.", "gauge")
		for _, metric := range snapshot.RunnerImages {
			fmt.Fprintf(&output, "fleet_runner_image_below_floor{platform=%s} %d\n",
				prometheusLabel(metric.Platform), boolGauge(metric.Reason != ""))
		}
	}
	if reservation := snapshot.Reservation; reservation != nil {
		// A reservation is singular by design (State.Reservation is one pointer),
		// so these are scalars. The head's demand key and repository are
		// deliberately NOT labels: they are unbounded cardinality, and they travel
		// in the status document instead.
		writeHelpType("fleet_reservation_held_seconds", "How long the scheduler has been holding a vector for its aged head.", "gauge")
		fmt.Fprintf(&output, "fleet_reservation_held_seconds{profile=%s} %s\n",
			prometheusLabel(reservation.Profile), seconds(reservation.Held))
		writeHelpType("fleet_reservation_vector_cpu", "vCPU withheld for the reserved head.", "gauge")
		fmt.Fprintf(&output, "fleet_reservation_vector_cpu{profile=%s} %d\n",
			prometheusLabel(reservation.Profile), reservation.CPU)
		// The axis is the diagnosis, and it is a closed vocabulary so the label
		// cannot open a new time series: a `vector` hold ends when live instances
		// release, and a `repository_cap` hold ends only when one of the head's own
		// repository's instances exits (issue #226, ADR 0038).
		writeHelpType("fleet_reservation_axis", "1 for the axis holding the reserved head out of admission.", "gauge")
		for _, axis := range reservationAxes {
			fmt.Fprintf(&output, "fleet_reservation_axis{axis=%s} %d\n",
				prometheusLabel(axis), boolGauge(axis == reservationAxisOrUnjudged(reservation.Axis)))
		}
		// 0 is the expensive state: a vector standing idle rather than lent to work
		// the head outranks.
		writeHelpType("fleet_reservation_lends_vector", "1 when the reserved head lends the vector it cannot use to work it outranks.", "gauge")
		fmt.Fprintf(&output, "fleet_reservation_lends_vector %d\n", boolGauge(reservation.LendsVector))
	}
	if len(snapshot.OperationFailures) > 0 {
		// The failure code is closed vocabulary, so label cardinality is bounded and
		// an alert can name the cause: a cleanup stuck on a busy-runner refusal reads
		// differently from one stuck on denied runner administration.
		writeHelpType("fleet_operation_failures", "Operations not progressing by kind and bounded failure code.", "gauge")
		for _, failure := range snapshot.OperationFailures {
			fmt.Fprintf(&output, "fleet_operation_failures{kind=%s,code=%s} %d\n",
				prometheusLabel(failure.Kind), prometheusLabel(failure.Code), failure.Count)
		}
		writeHelpType("fleet_operation_failure_attempts", "Worst attempt count among operations sharing a failure code.", "gauge")
		for _, failure := range snapshot.OperationFailures {
			fmt.Fprintf(&output, "fleet_operation_failure_attempts{kind=%s,code=%s} %d\n",
				prometheusLabel(failure.Kind), prometheusLabel(failure.Code), failure.Attempts)
		}
	}

	if len(snapshot.ComponentFailures) > 0 {
		// Both labels are closed vocabulary authored in-process — the component is a
		// static loop name and the reason is the bounded failure token — so label
		// cardinality is bounded and an alert can rate() the exact incident: a
		// scheduler refused by the durable layer reads differently from one losing a
		// harmless race, and the log's one-line-per-minute rate limit tells neither.
		writeHelpType("fleet_component_failures_total", "Control-plane loop failures by component and bounded reason.", "counter")
		for _, failure := range snapshot.ComponentFailures {
			fmt.Fprintf(&output, "fleet_component_failures_total{component=%s,reason=%s} %d\n",
				prometheusLabel(failure.Component), prometheusLabel(failure.Reason), failure.Count)
		}
	}

	writeHelpType("fleet_host_available_memory_mib", "Host reclaimable memory available for admission in MiB.", "gauge")
	fmt.Fprintf(&output, "fleet_host_available_memory_mib %d\n", snapshot.HostPressure.AvailableMemoryMiB)
	writeHelpType("fleet_host_free_disk_gib", "Host filesystem free space in GiB.", "gauge")
	fmt.Fprintf(&output, "fleet_host_free_disk_gib %d\n", snapshot.HostPressure.FreeDiskGiB)
	writeHelpType("fleet_host_swap_used_mib", "Host swap currently used in MiB.", "gauge")
	fmt.Fprintf(&output, "fleet_host_swap_used_mib %d\n", snapshot.HostPressure.SwapUsedMiB)
	writeHelpType("fleet_host_swapouts_total", "Cumulative host swap-out page count.", "counter")
	fmt.Fprintf(&output, "fleet_host_swapouts_total %d\n", snapshot.HostPressure.SwapOuts)
	writeHelpType("fleet_host_swapout_rate_pages_per_second",
		"Host swap-out page rate the swap guardrail decides on; meaningful only when fleet_host_swapout_rate_observed is 1.", "gauge")
	fmt.Fprintf(&output, "fleet_host_swapout_rate_pages_per_second %s\n",
		strconv.FormatFloat(snapshot.HostPressure.SwapOutRatePerSecond, 'f', -1, 64))
	writeHelpType("fleet_host_swapout_rate_observed",
		"Whether the swap-out rate could be measured; 0 means the guardrail fell back to the swap level, never that the host is quiet.", "gauge")
	fmt.Fprintf(&output, "fleet_host_swapout_rate_observed %d\n", boolGauge(snapshot.HostPressure.SwapOutRateObserved))
	writeHelpType("fleet_host_cpu_idle_percent", "Host CPU idle percentage at the latest admission snapshot.", "gauge")
	fmt.Fprintf(&output, "fleet_host_cpu_idle_percent %s\n", strconv.FormatFloat(snapshot.HostPressure.CPUIdlePercent, 'f', -1, 64))
	writeHelpType("fleet_host_load_average", "Host one-minute load average at the latest admission snapshot.", "gauge")
	fmt.Fprintf(&output, "fleet_host_load_average %s\n", strconv.FormatFloat(snapshot.HostPressure.LoadAverage, 'f', -1, 64))
	writeHelpType("fleet_host_admission_allowed", "Whether latest host pressure permits new VM admission.", "gauge")
	fmt.Fprintf(&output, "fleet_host_admission_allowed %d\n", boolGauge(snapshot.HostPressure.AdmissionAllowed))

	writeHelpType("fleet_observation_fresh", "Whether a bounded critical observation is fresh.", "gauge")
	writeHelpType("fleet_observation_age_seconds", "Age of a bounded critical observation.", "gauge")
	for _, observation := range sortedKeys(snapshot.Observations) {
		metric := snapshot.Observations[observation]
		fresh := 0
		if metric.Freshness == ObservationFresh && !metric.ObservedAt.IsZero() && snapshot.Now.Sub(metric.ObservedAt) <= snapshot.ObservationTTL {
			fresh = 1
		}
		age := time.Duration(0)
		if !metric.ObservedAt.IsZero() && snapshot.Now.After(metric.ObservedAt) {
			age = snapshot.Now.Sub(metric.ObservedAt)
		}
		label := prometheusLabel(observation)
		fmt.Fprintf(&output, "fleet_observation_fresh{observation=%s} %d\n", label, fresh)
		fmt.Fprintf(&output, "fleet_observation_age_seconds{observation=%s} %s\n", label, seconds(age))
	}

	writeHelpType("fleet_mode", "Current host allocation mode.", "gauge")
	for _, mode := range []Mode{ModeIdle, ModeLinux, ModeMacOS, ModeMixed, ModeMaintenance} {
		value := 0
		if snapshot.Mode == mode {
			value = 1
		}
		fmt.Fprintf(&output, "fleet_mode{mode=%s} %d\n", prometheusLabel(string(mode)), value)
	}
	writeHelpType("fleet_last_successful_tick_timestamp_seconds", "Unix timestamp of the last successful reconciliation tick.", "gauge")
	lastSuccessful := int64(0)
	if !snapshot.LastSuccessfulTick.IsZero() {
		lastSuccessful = snapshot.LastSuccessfulTick.Unix()
	}
	fmt.Fprintf(&output, "fleet_last_successful_tick_timestamp_seconds %d\n", lastSuccessful)
	return output.String()
}

// reservationAxes is the closed label vocabulary fleet_reservation_axis emits,
// every value on every scrape, so an alert can say "the cap axis has been set
// for twenty minutes" instead of having to infer it from a series appearing.
var reservationAxes = []string{"vector", "repository_cap", "both", "none", "unjudged"}

// reservationAxisOrUnjudged maps a plan that judged nothing onto a word, so the
// axis label is never empty.
func reservationAxisOrUnjudged(axis string) string {
	if axis == "" {
		return "unjudged"
	}
	return axis
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func seconds(value time.Duration) string {
	return strconv.FormatFloat(value.Seconds(), 'f', -1, 64)
}

func prometheusLabel(value string) string {
	return strconv.Quote(value)
}

// boolGauge renders a boolean fact as the 0/1 gauge Prometheus expects.
func boolGauge(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (s *Server) Serve(listener net.Listener) error {
	if listener == nil {
		return errListenerRequired
	}
	if address, ok := listener.Addr().(*net.TCPAddr); ok && !address.IP.IsLoopback() {
		return errUnsafeListener
	}
	s.mu.Lock()
	if s.served {
		s.mu.Unlock()
		return errServerAlreadyServed
	}
	s.served = true
	s.mu.Unlock()
	if err := s.httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return errServerFailed
	}
	return nil
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}
