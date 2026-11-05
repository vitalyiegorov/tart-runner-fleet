package daemon

import (
	"context"
	"sync"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/executor"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/lifecycle"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/telemetry"
)

// guestTrafficTimeout bounds every counter read. It is short on purpose:
// teardown waits for the end-of-job one, and a guest slow to print its counters
// is recorded as unmeasured rather than allowed to hold its vector (ADR 0080).
const guestTrafficTimeout = 5 * time.Second

// guestTrafficInterval is how often a live guest's counters are read while its
// job runs. A Tart guest powers itself off when its runner exits (ADR 0010), so
// the end-of-job read usually finds it gone; the last reading from this sweep
// stands in for it. It bounds how much of a job's final traffic can be missed.
const guestTrafficInterval = 15 * time.Second

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
	// Last holds each live guest's most recent successful reading. Nil keeps no
	// readings, so the end-of-job read stands alone.
	Last *guestReadings
}

// reading is one successful counter read.
type reading struct {
	receive, transmit int64
	at                time.Time
}

// guestReadings is the in-memory, best-effort store of the latest reading per
// instance. It is not persisted: a daemon restart forgets it, and the job is then
// recorded as it was before it existed.
type guestReadings struct {
	mu   sync.Mutex
	byID map[string]reading
}

func newGuestReadings() *guestReadings { return &guestReadings{byID: map[string]reading{}} }

func (g *guestReadings) put(id string, r reading) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.byID[id] = r
}

func (g *guestReadings) get(id string) (reading, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	r, ok := g.byID[id]
	return r, ok
}

func (g *guestReadings) forget(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.byID, id)
}

// retain drops every reading whose instance is not in live, so the map is
// bounded by the live instances.
func (g *guestReadings) retain(live map[string]bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for id := range g.byID {
		if !live[id] {
			delete(g.byID, id)
		}
	}
}

func (r guestTrafficRecorder) RecordGuestTraffic(ctx context.Context, instance operations.Instance) {
	sample := r.sample(ctx, instance)
	if r.Last != nil {
		if last, ok := r.Last.get(instance.ID); ok && !sample.Measured {
			sample.Unmeasured, sample.Measured = "", true
			sample.ReceiveBytes, sample.TransmitBytes, sample.SampledAt = last.receive, last.transmit, last.at
		}
		r.Last.forget(instance.ID)
	}
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

// Sweep reads every live guest whose job may still be running, concurrently,
// and keeps each successful reading. A failed read keeps the previous one: the
// guest may be mid-shutdown, which is exactly what the kept reading is for.
// Readings of instances no longer live are dropped.
func (r guestTrafficRecorder) Sweep(ctx context.Context, live []operations.Instance) {
	if r.Last == nil {
		return
	}
	ids := make(map[string]bool, len(live))
	var wg sync.WaitGroup
	for _, instance := range live {
		ids[instance.ID] = true
		if !guestMayBeRunning(instance.State) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if sample := r.sample(ctx, instance); sample.Measured {
				r.Last.put(instance.ID, reading{receive: sample.ReceiveBytes, transmit: sample.TransmitBytes, at: sample.SampledAt})
			}
		}()
	}
	wg.Wait()
	r.Last.retain(ids)
}

// guestMayBeRunning is the states in which a guest is up and its runner has not
// been proved finished. Once a drain starts the guest may be powering off, and
// reading it would only wait out the deadline.
func guestMayBeRunning(state operations.State) bool {
	return state == operations.StateOnlineIdle || state == operations.StateAssigned || state == operations.StateRunning
}

// guestTrafficSweeper runs Sweep on a tick. It is best-effort and independent of
// the drain: a read that fails or hangs is bounded by the recorder's deadline and
// delays only the next sweep, never a drain.
type guestTrafficSweeper struct {
	Source interface {
		LiveInstances(context.Context) ([]operations.Instance, error)
	}
	Recorder guestTrafficRecorder
	Tick     <-chan time.Time
}

func (s guestTrafficSweeper) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.Tick:
			if live, err := s.Source.LiveInstances(ctx); err != nil {
				s.Recorder.warn("guest traffic sweep could not list live instances", "error", err)
			} else {
				s.Recorder.Sweep(ctx, live)
			}
		}
	}
}

func (r guestTrafficRecorder) warn(message string, args ...any) {
	if r.Warn != nil {
		r.Warn(message, args...)
	}
}

// guestTraffic builds the node's traffic recorder, or false on a node with no
// guest to exec into.
func (d dependencies) guestTraffic(store guestTrafficStore, metrics guestTrafficMetrics, cfg config.Config,
	warn func(string, ...any)) (guestTrafficRecorder, bool) {
	if d.guestExec == nil {
		return guestTrafficRecorder{}, false
	}
	runner := d.guestExec(cfg)
	if runner == nil {
		return guestTrafficRecorder{}, false
	}
	return guestTrafficRecorder{Runner: runner, Timeout: guestTrafficTimeout, Store: store, Metrics: metrics,
		Now: d.now, Warn: warn, Last: newGuestReadings()}, true
}

// trafficOrNil is the drain's recorder, or an untyped interface nil so the
// drain's `Traffic != nil` check sees a node with no guest to exec into.
func trafficOrNil(recorder guestTrafficRecorder, ok bool) lifecycle.GuestTrafficRecorder {
	if !ok {
		return nil
	}
	return recorder
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
