package daemon

import (
	"context"
	"sync"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// hostTrafficInterval is how often the host's counters for the live guests are
// read. A read is three short host commands for every guest at once and never
// touches a guest, so it can be frequent. The interval bounds how much of a
// job's final traffic can be missed: a Tart guest powers itself off when its
// runner exits (ADR 0010) and its virtual NIC goes with it.
const hostTrafficInterval = 5 * time.Second

// hostCounters is the host's own view of what each named guest's virtual NIC has
// carried. A guest it cannot see is absent from the answer, never zero.
type hostCounters interface {
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

// transition records whether the host could be read and reports a change, so a
// host that stays unreadable is warned about once, not every few seconds.
func (g *guestReadings) transition(unreadable bool) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	changed := g.unreadable != unreadable
	g.unreadable = unreadable
	return changed
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

// retain drops every reading whose instance is not live, so the map is bounded
// by the live instances.
func (g *guestReadings) retain(live map[string]bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for id := range g.byID {
		if !live[id] {
			delete(g.byID, id)
		}
	}
}

// hostTrafficRecorder is lifecycle.GuestTrafficRecorder for a Tart node. A Tart
// guest is gone before the drain can ask it anything, and asking a guest over
// `tart exec` is the vsock traffic that traps Virtualization.framework (issue
// #478). So the recorder asks nothing: it records the last reading the sweeper
// took from the host while the guest was alive. Counters only grow, so that is a
// lower bound that misses at most the final interval. With no reading the job is
// unmeasured, never zero (AGENTS.md rule 4).
//
// RecordGuestTraffic reads memory and the store only. It runs no command, so it
// cannot hold a drain.
type hostTrafficRecorder struct {
	Host    hostCounters
	Timeout time.Duration
	Store   guestTrafficStore
	Metrics guestTrafficMetrics
	Now     func() time.Time
	Warn    func(string, ...any)
	Last    *guestReadings
}

func (r hostTrafficRecorder) RecordGuestTraffic(ctx context.Context, instance operations.Instance) {
	sample := operations.TrafficSample{InstanceID: instance.ID, Repo: instance.Repo, Platform: instance.Platform,
		Profile: instance.Profile, StartedAt: instance.CreatedAt, SampledAt: r.Now().UTC(),
		Unmeasured: operations.TrafficNoReading}
	if last, ok := r.Last.get(instance.ID); ok {
		sample.Unmeasured, sample.Measured = "", true
		sample.ReceiveBytes, sample.TransmitBytes, sample.SampledAt = last.receive, last.transmit, last.at
	}
	r.Last.forget(instance.ID)
	storeGuestTraffic(ctx, r.Store, r.Metrics, r.warn, sample)
}

// Sweep reads the host's counters for every live guest, once, and keeps each
// reading it got. A host that cannot be read, or a guest it no longer shows,
// keeps the previous reading: a guest mid-shutdown is exactly what the kept
// reading is for. Readings of instances no longer live are dropped. It touches
// neither the store nor the metrics, and its one read is bounded by Timeout.
func (r hostTrafficRecorder) Sweep(ctx context.Context, live []operations.Instance) {
	ids := make(map[string]bool, len(live))
	names := make([]string, 0, len(live))
	for _, instance := range live {
		ids[instance.ID] = true
		names = append(names, instance.ID)
	}
	r.Last.retain(ids)
	if len(names) == 0 {
		return
	}
	attempt, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	counters, err := r.Host.Read(attempt, names)
	if r.Last.transition(err != nil) {
		if err != nil {
			r.warn("guest traffic cannot be read from the host; jobs are unmeasured until it can", "error", err)
		} else {
			r.warn("guest traffic is readable from the host again")
		}
	}
	if err != nil {
		return
	}
	at := r.Now().UTC()
	for id, counted := range counters {
		r.Last.put(id, reading{receive: counted.ReceiveBytes, transmit: counted.TransmitBytes, at: at})
	}
}

func (r hostTrafficRecorder) warn(message string, args ...any) {
	if r.Warn != nil {
		r.Warn(message, args...)
	}
}

// hostTrafficSweeper runs Sweep on a tick. It is best-effort and independent of
// the drain: a read that fails or hangs is bounded by the recorder's deadline and
// delays only the next sweep.
type hostTrafficSweeper struct {
	Source interface {
		LiveInstances(context.Context) ([]operations.Instance, error)
	}
	Recorder hostTrafficRecorder
	Tick     <-chan time.Time
}

func (s hostTrafficSweeper) Run(ctx context.Context) {
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
