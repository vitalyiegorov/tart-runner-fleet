package daemon

import (
	"context"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/executor"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/lifecycle"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/telemetry"
)

// guestTrafficTimeout bounds the one end-of-job counter read. It is short on
// purpose: teardown waits for it, and a guest slow to print its counters is
// recorded as unmeasured rather than allowed to hold its vector (ADR 0080).
const guestTrafficTimeout = 5 * time.Second

type guestTrafficStore interface {
	RecordGuestTraffic(context.Context, operations.TrafficSample) (bool, error)
	GuestTraffic(context.Context, time.Time, int) ([]operations.TrafficRecord, error)
}

type guestTrafficMetrics interface {
	AddGuestTraffic(telemetry.GuestTrafficMetric) error
}

// guestTrafficRecorder is lifecycle.GuestTrafficRecorder: it reads the guest's
// own interface counters through the same `exec <instance>` vector readiness
// and liveness use, on Tart and podman alike, and stores the result.
type guestTrafficRecorder struct {
	Runner  executor.CommandRunner
	Timeout time.Duration
	Store   guestTrafficStore
	Metrics guestTrafficMetrics
	Now     func() time.Time
	Warn    func(string, ...any)
}

func (r guestTrafficRecorder) RecordGuestTraffic(ctx context.Context, instance operations.Instance) {
	sample := r.sample(ctx, instance)
	changed, err := r.Store.RecordGuestTraffic(ctx, sample)
	if err != nil {
		r.warn("guest traffic sample was not stored", "instance", instance.ID, "error", err)
		return
	}
	if !changed {
		return
	}
	if err := r.Metrics.AddGuestTraffic(telemetry.GuestTrafficMetric{Platform: string(sample.Platform),
		Profile: string(sample.Profile), Repo: sample.Repo, Measured: sample.Measured,
		ReceiveBytes: sample.ReceiveBytes, TransmitBytes: sample.TransmitBytes}); err != nil {
		r.warn("guest traffic sample was not counted", "instance", instance.ID, "error", err)
	}
}

func (r guestTrafficRecorder) sample(ctx context.Context, instance operations.Instance) operations.TrafficSample {
	sample := operations.TrafficSample{InstanceID: instance.ID, Repo: instance.Repo, Platform: instance.Platform,
		Profile: instance.Profile, StartedAt: instance.CreatedAt, SampledAt: r.Now().UTC()}
	command := domain.GuestNetworkCommand(instance.Platform)
	if command == nil || domain.ValidateInstanceName(instance.ID) != nil {
		sample.Unmeasured = operations.TrafficNoCommand
		return sample
	}
	attempt, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	output, err := r.Runner.Run(attempt, append([]string{"exec", instance.ID}, command...)...)
	switch {
	case err != nil && attempt.Err() != nil:
		sample.Unmeasured = operations.TrafficExecTimeout
		return sample
	case err != nil:
		sample.Unmeasured = operations.TrafficExecFailed
		return sample
	}
	counters, err := domain.ParseGuestNetwork(instance.Platform, output)
	if err != nil {
		sample.Unmeasured = operations.TrafficUnreadable
		return sample
	}
	sample.Measured, sample.ReceiveBytes, sample.TransmitBytes = true, counters.ReceiveBytes, counters.TransmitBytes
	return sample
}

func (r guestTrafficRecorder) warn(message string, args ...any) {
	if r.Warn != nil {
		r.Warn(message, args...)
	}
}

// guestTraffic builds the drain's traffic recorder, or nil on a node with no
// guest to exec into. The nil is an untyped interface nil, so the drain's
// `Traffic != nil` check sees it.
func (d dependencies) guestTraffic(store guestTrafficStore, metrics guestTrafficMetrics, cfg config.Config,
	warn func(string, ...any)) lifecycle.GuestTrafficRecorder {
	if d.guestExec == nil {
		return nil
	}
	runner := d.guestExec(cfg)
	if runner == nil {
		return nil
	}
	return guestTrafficRecorder{Runner: runner, Timeout: guestTrafficTimeout, Store: store, Metrics: metrics,
		Now: d.now, Warn: warn}
}

// guestTrafficReader is telemetry.TrafficReader over the store.
type guestTrafficReader struct{ Store guestTrafficStore }

func (r guestTrafficReader) GuestTraffic(ctx context.Context, since time.Time) ([]adminapi.TrafficJob, bool, error) {
	records, err := r.Store.GuestTraffic(ctx, since, adminapi.MaxTrafficJobs+1)
	if err != nil {
		return nil, false, err
	}
	truncated := len(records) > adminapi.MaxTrafficJobs
	if truncated {
		records = records[:adminapi.MaxTrafficJobs]
	}
	jobs := make([]adminapi.TrafficJob, 0, len(records))
	for _, record := range records {
		job := adminapi.TrafficJob{Instance: record.InstanceID, Repo: record.Repo, Platform: string(record.Platform),
			Profile: string(record.Profile), ScaleSetID: record.ScaleSetID, RunID: record.RunID,
			WorkflowRef: record.WorkflowRef, Job: record.JobName, StartedAt: record.StartedAt, SampledAt: record.SampledAt,
			Measured: record.Measured, Unmeasured: record.Unmeasured}
		if record.Measured {
			receive, transmit := record.ReceiveBytes, record.TransmitBytes
			job.ReceiveBytes, job.TransmitBytes = &receive, &transmit
		}
		jobs = append(jobs, job)
	}
	return jobs, truncated, nil
}
