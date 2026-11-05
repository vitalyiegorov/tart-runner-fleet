package lifecycle

import (
	"context"
	"sync"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// HostTrafficInterval is how often the host's counters for the live guests are
// read. A read is three short host commands for every guest at once and never
// touches a guest, so it can be frequent. Traffic a guest moves after the last
// successful read is omitted, and normally that is up to one interval: a Tart
// guest powers itself off when its runner exits (ADR 0010) and its virtual NIC
// goes with it. While host reads fail there is no fixed bound (ADR 0080).
const HostTrafficInterval = 5 * time.Second

// HostCounters is the host's own view of what each named guest's virtual NIC has
// carried. A guest it cannot see is absent from the answer, never zero.
type HostCounters interface {
	Read(context.Context, []string) (map[string]domain.GuestNetwork, error)
}

// reading is one successful host read of a guest.
type reading struct {
	receive, transmit int64
	at                time.Time
}

// guestReadings is the in-memory, best-effort store of the latest reading per
// instance. It is not persisted: a daemon restart forgets it, and a job that had
// one is then recorded as unmeasured.
type guestReadings struct {
	mu         sync.Mutex
	byID       map[string]reading
	unreadable bool
}

func newGuestReadings() *guestReadings { return &guestReadings{byID: map[string]reading{}} }

// transition records whether the host could be read and reports a change, so a
// host that stays unreadable is warned about once, not every few seconds.
func (g *guestReadings) transition(unreadable bool) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	changed := g.unreadable != unreadable
	g.unreadable = unreadable
	return changed
}

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

// retain drops every reading whose instance is not live, so the map is bounded
// by the live instances. It is the only thing that removes a reading: a reading
// whose job has been recorded stays until its instance is gone, so a drain that
// retries still finds it.
func (g *guestReadings) retain(live map[string]bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for id := range g.byID {
		if !live[id] {
			delete(g.byID, id)
		}
	}
}

// HostTrafficRecorder is GuestTrafficRecorder for a Tart node. A Tart guest is
// gone before the drain can ask it anything, and asking a guest over `tart exec`
// is the vsock traffic that traps Virtualization.framework (issue #478). So the
// recorder asks nothing: it records the last reading Sweep took from the host
// while the guest was alive. Counters only grow, so that reading is a lower
// bound. Traffic after the last successful reading is omitted: normally up to
// one HostTrafficInterval, with no fixed bound while host reads fail. The
// stored SampledAt is the reading's time, so it shows how old the reading was.
// With no reading the job is unmeasured, never zero (AGENTS.md rule 4).
//
// RecordGuestTraffic reads memory and hands the sample to Record. It runs no
// command, so it cannot hold a drain. A drain may call it more than once for an
// instance; the reading is kept so every call sees it, and Record must be
// idempotent (the store keeps the first measured sample and counts it once).
type HostTrafficRecorder struct {
	host    HostCounters
	timeout time.Duration
	record  func(context.Context, operations.TrafficSample)
	now     func() time.Time
	warn    func(string, ...any)
	last    *guestReadings
}

// NewHostTrafficRecorder builds the recorder. record stores and counts one
// sample and reports nothing; warn may be nil. timeout bounds each host read.
func NewHostTrafficRecorder(host HostCounters, timeout time.Duration,
	record func(context.Context, operations.TrafficSample), now func() time.Time,
	warn func(string, ...any)) *HostTrafficRecorder {
	return &HostTrafficRecorder{host: host, timeout: timeout, record: record, now: now, warn: warn,
		last: newGuestReadings()}
}

func (r *HostTrafficRecorder) RecordGuestTraffic(ctx context.Context, instance operations.Instance) {
	sample := operations.TrafficSample{InstanceID: instance.ID, Repo: instance.Repo, Platform: instance.Platform,
		Profile: instance.Profile, StartedAt: instance.CreatedAt, SampledAt: r.now().UTC(),
		Unmeasured: operations.TrafficNoReading}
	if last, ok := r.last.get(instance.ID); ok {
		sample.Unmeasured, sample.Measured = "", true
		sample.ReceiveBytes, sample.TransmitBytes, sample.SampledAt = last.receive, last.transmit, last.at
	}
	r.record(ctx, sample)
}

// Sweep reads the host's counters for every live guest, once, and keeps each
// reading it got. A host that cannot be read, or a guest it no longer shows,
// keeps the previous reading: a guest mid-shutdown is exactly what the kept
// reading is for. Readings of instances no longer live are dropped. It touches
// neither the store nor the metrics, and its one read is bounded by the timeout.
func (r *HostTrafficRecorder) Sweep(ctx context.Context, live []operations.Instance) {
	ids := make(map[string]bool, len(live))
	names := make([]string, 0, len(live))
	for _, instance := range live {
		ids[instance.ID] = true
		names = append(names, instance.ID)
	}
	r.last.retain(ids)
	if len(names) == 0 {
		return
	}
	attempt, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	counters, err := r.host.Read(attempt, names)
	if r.last.transition(err != nil) {
		if err != nil {
			r.warnf("guest traffic cannot be read from the host; jobs are unmeasured until it can", "error", err)
		} else {
			r.warnf("guest traffic is readable from the host again")
		}
	}
	if err != nil {
		return
	}
	at := r.now().UTC()
	for id, counted := range counters {
		r.last.put(id, reading{receive: counted.ReceiveBytes, transmit: counted.TransmitBytes, at: at})
	}
}

func (r *HostTrafficRecorder) warnf(message string, args ...any) {
	if r.warn != nil {
		r.warn(message, args...)
	}
}

// LiveInstanceSource lists the instances whose guests may still be up.
type LiveInstanceSource interface {
	LiveInstances(context.Context) ([]operations.Instance, error)
}

// HostTrafficSweeper runs Sweep on a tick. It is best-effort and independent of
// the drain: a read that fails or hangs is bounded by the recorder's deadline and
// delays only the next sweep.
type HostTrafficSweeper struct {
	Source   LiveInstanceSource
	Recorder *HostTrafficRecorder
	Tick     <-chan time.Time
}

func (s HostTrafficSweeper) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.Tick:
			if live, err := s.Source.LiveInstances(ctx); err != nil {
				s.Recorder.warnf("guest traffic sweep could not list live instances", "error", err)
			} else {
				s.Recorder.Sweep(ctx, live)
			}
		}
	}
}
