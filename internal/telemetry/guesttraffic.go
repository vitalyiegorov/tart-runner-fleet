package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
)

// GuestTrafficMetric is one job's end-of-job guest counters (ADR 0080). An
// unmeasured job carries no bytes and adds only to the unmeasured job count.
type GuestTrafficMetric struct {
	Platform      string
	Profile       string
	Repo          string
	Measured      bool
	ReceiveBytes  int64
	TransmitBytes int64
}

type guestTrafficKey struct{ platform, profile, repo string }

// GuestTrafficSeries is one accumulated {platform, profile, repo} series.
type GuestTrafficSeries struct {
	Platform, Profile, Repo     string
	ReceiveBytes, TransmitBytes int64
	MeasuredJobs, Unmeasured    int64
}

// maxGuestTrafficSeries bounds the label space. Repositories come from the
// configured targets, so a real node is far below it; a producer past it is
// refused rather than allowed to grow the endpoint without bound.
const maxGuestTrafficSeries = 512

var (
	guestTrafficPlatforms = map[string]struct{}{"linux": {}, "macos": {}}
	boundedRepository     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}/[A-Za-z0-9_.-]{1,100}$`)
)

// AddGuestTraffic adds one job to its series. The counters live for the
// process, as Prometheus counters do; the durable per-job record is the store's.
func (h *Health) AddGuestTraffic(metric GuestTrafficMetric) error {
	if _, ok := guestTrafficPlatforms[metric.Platform]; !ok || !boundedResourceID.MatchString(metric.Profile) ||
		!boundedRepository.MatchString(metric.Repo) || metric.ReceiveBytes < 0 || metric.TransmitBytes < 0 ||
		(!metric.Measured && (metric.ReceiveBytes != 0 || metric.TransmitBytes != 0)) {
		return errInvalidMetric
	}
	key := guestTrafficKey{metric.Platform, metric.Profile, metric.Repo}
	h.mu.Lock()
	defer h.mu.Unlock()
	series, ok := h.guestTraffic[key]
	if !ok {
		if len(h.guestTraffic) >= maxGuestTrafficSeries {
			return errInvalidMetric
		}
		if h.guestTraffic == nil {
			h.guestTraffic = map[guestTrafficKey]*GuestTrafficSeries{}
		}
		series = &GuestTrafficSeries{Platform: metric.Platform, Profile: metric.Profile, Repo: metric.Repo}
		h.guestTraffic[key] = series
	}
	if !metric.Measured {
		series.Unmeasured++
		return nil
	}
	series.MeasuredJobs++
	series.ReceiveBytes += metric.ReceiveBytes
	series.TransmitBytes += metric.TransmitBytes
	return nil
}

// guestTrafficSeries copies the series in a stable order; the caller holds
// h.mu for reading.
func (h *Health) guestTrafficSeries() []GuestTrafficSeries {
	series := make([]GuestTrafficSeries, 0, len(h.guestTraffic))
	for _, value := range h.guestTraffic {
		series = append(series, *value)
	}
	sort.Slice(series, func(i, j int) bool {
		left, right := series[i], series[j]
		return left.Platform+"\x00"+left.Profile+"\x00"+left.Repo < right.Platform+"\x00"+right.Profile+"\x00"+right.Repo
	})
	return series
}

func renderGuestTraffic(output *strings.Builder, series []GuestTrafficSeries) {
	if len(series) == 0 {
		return
	}
	labels := func(row GuestTrafficSeries) string {
		return fmt.Sprintf("platform=%s,profile=%s,repo=%s", prometheusLabel(row.Platform),
			prometheusLabel(row.Profile), prometheusLabel(row.Repo))
	}
	for _, counter := range []struct {
		name, help string
		value      func(GuestTrafficSeries) int64
	}{
		{"fleet_guest_network_receive_bytes_total", "Bytes guests received over their jobs, from their own interface counters.",
			func(row GuestTrafficSeries) int64 { return row.ReceiveBytes }},
		{"fleet_guest_network_transmit_bytes_total", "Bytes guests transmitted over their jobs, from their own interface counters.",
			func(row GuestTrafficSeries) int64 { return row.TransmitBytes }},
	} {
		fmt.Fprintf(output, "# HELP %s %s\n# TYPE %s counter\n", counter.name, counter.help, counter.name)
		for _, row := range series {
			fmt.Fprintf(output, "%s{%s} %d\n", counter.name, labels(row), counter.value(row))
		}
	}
	output.WriteString("# HELP fleet_guest_network_jobs_total Jobs sampled at teardown, by whether their counters could be read.\n" +
		"# TYPE fleet_guest_network_jobs_total counter\n")
	for _, row := range series {
		fmt.Fprintf(output, "fleet_guest_network_jobs_total{%s,measured=\"true\"} %d\n", labels(row), row.MeasuredJobs)
		fmt.Fprintf(output, "fleet_guest_network_jobs_total{%s,measured=\"false\"} %d\n", labels(row), row.Unmeasured)
	}
}

// TrafficReader is the daemon's read of the durable per-job samples taken at
// or after since, newest first, with whether the window held more than
// adminapi.MaxTrafficJobs.
type TrafficReader interface {
	GuestTraffic(context.Context, time.Time) ([]adminapi.TrafficJob, bool, error)
}

func trafficHandler(health *Health, reader TrafficReader) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			response.Header().Set("Allow", http.MethodGet)
			http.Error(response, "method_not_allowed", http.StatusMethodNotAllowed)
			return
		}
		seconds, err := strconv.ParseInt(request.URL.Query().Get("sinceSeconds"), 10, 64)
		window := time.Duration(seconds) * time.Second
		if err != nil || seconds <= 0 || window > adminapi.MaxTrafficWindow {
			http.Error(response, "invalid_window", http.StatusBadRequest)
			return
		}
		now := health.clock.Now().UTC()
		since := now.Add(-window)
		jobs, truncated, err := reader.GuestTraffic(request.Context(), since)
		if err != nil {
			// An unreadable store is not a node that moved no bytes.
			http.Error(response, "traffic_unavailable", http.StatusServiceUnavailable)
			return
		}
		if jobs == nil {
			jobs = []adminapi.TrafficJob{}
		}
		response.Header().Set("Content-Type", "application/json; charset=utf-8")
		response.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(response).Encode(adminapi.TrafficEnvelope{APIVersion: adminapi.APIVersion,
			Kind: adminapi.TrafficKind, GeneratedAt: now, Data: adminapi.Traffic{Since: since, Truncated: truncated,
				Totals: adminapi.SummarizeTraffic(jobs), Jobs: jobs}})
	}
}
