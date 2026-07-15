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

type ServerConfig struct {
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ReadHeaderTimeout time.Duration
	MaxHeaderBytes    int
	ControllerVersion string
	ControllerMode    string
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
		envelope := statusEnvelope(snapshot, controllerVersion, controllerMode, health.Live(), health.Ready())
		if err := json.NewEncoder(response).Encode(envelope); err != nil {
			return
		}
	}
}

func statusEnvelope(snapshot Snapshot, controllerVersion, controllerMode string, live, ready HealthResult) adminapi.StatusEnvelope {
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
			ObservedAt: metric.ObservedAt, AgeSeconds: age.Seconds()})
	}
	return adminapi.StatusEnvelope{APIVersion: adminapi.APIVersion, Kind: "Status", GeneratedAt: snapshot.Now,
		Revision: snapshot.Revision, Warnings: []adminapi.Warning{}, Data: adminapi.Status{
			ControllerVersion: controllerVersion, ControllerMode: controllerMode, HostMode: string(snapshot.Mode),
			LastLoopTick: snapshot.LastLoopTick, LastSuccessfulTick: snapshot.LastSuccessfulTick,
			Live:   adminapi.Check{OK: live.OK, Reasons: nonNilStrings(live.Reasons)},
			Ready:  adminapi.Check{OK: ready.OK, Reasons: nonNilStrings(ready.Reasons)},
			Queues: queues, Instances: instances, Observations: observations,
			Operations: adminapi.OperationSummary{Retrying: snapshot.OperationRetries, Dead: snapshot.DeadOperations},
			HostPressure: adminapi.HostPressure{AvailableMemoryMiB: snapshot.HostPressure.AvailableMemoryMiB,
				FreeDiskGiB: snapshot.HostPressure.FreeDiskGiB, SwapUsedMiB: snapshot.HostPressure.SwapUsedMiB,
				SwapOuts: snapshot.HostPressure.SwapOuts, CPUIdlePercent: snapshot.HostPressure.CPUIdlePercent,
				LoadAverage: snapshot.HostPressure.LoadAverage, AdmissionAllowed: snapshot.HostPressure.AdmissionAllowed,
				AdmissionReason: snapshot.HostPressure.AdmissionReason},
		}}
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

	writeHelpType("fleet_host_available_memory_mib", "Host reclaimable memory available for admission in MiB.", "gauge")
	fmt.Fprintf(&output, "fleet_host_available_memory_mib %d\n", snapshot.HostPressure.AvailableMemoryMiB)
	writeHelpType("fleet_host_free_disk_gib", "Host filesystem free space in GiB.", "gauge")
	fmt.Fprintf(&output, "fleet_host_free_disk_gib %d\n", snapshot.HostPressure.FreeDiskGiB)
	writeHelpType("fleet_host_swap_used_mib", "Host swap currently used in MiB.", "gauge")
	fmt.Fprintf(&output, "fleet_host_swap_used_mib %d\n", snapshot.HostPressure.SwapUsedMiB)
	writeHelpType("fleet_host_swapouts_total", "Cumulative host swap-out page count.", "counter")
	fmt.Fprintf(&output, "fleet_host_swapouts_total %d\n", snapshot.HostPressure.SwapOuts)
	writeHelpType("fleet_host_cpu_idle_percent", "Host CPU idle percentage at the latest admission snapshot.", "gauge")
	fmt.Fprintf(&output, "fleet_host_cpu_idle_percent %s\n", strconv.FormatFloat(snapshot.HostPressure.CPUIdlePercent, 'f', -1, 64))
	writeHelpType("fleet_host_load_average", "Host one-minute load average at the latest admission snapshot.", "gauge")
	fmt.Fprintf(&output, "fleet_host_load_average %s\n", strconv.FormatFloat(snapshot.HostPressure.LoadAverage, 'f', -1, 64))
	writeHelpType("fleet_host_admission_allowed", "Whether latest host pressure permits new VM admission.", "gauge")
	allowed := 0
	if snapshot.HostPressure.AdmissionAllowed {
		allowed = 1
	}
	fmt.Fprintf(&output, "fleet_host_admission_allowed %d\n", allowed)

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
