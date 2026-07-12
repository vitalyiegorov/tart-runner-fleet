package telemetry

import (
	"context"
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
	return &Server{httpServer: &http.Server{
		Handler: mux, ReadTimeout: config.ReadTimeout, WriteTimeout: config.WriteTimeout,
		IdleTimeout: config.IdleTimeout, ReadHeaderTimeout: config.ReadHeaderTimeout,
		MaxHeaderBytes: config.MaxHeaderBytes,
	}}, nil
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

	writeHelpType("fleet_mode", "Current mutually exclusive host mode.", "gauge")
	for _, mode := range []Mode{ModeIdle, ModeLinux, ModeMacOS, ModeMaintenance} {
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
