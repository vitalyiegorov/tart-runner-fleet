package lifecycle

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// scriptedHost is the host's view of each guest's virtual NIC. A guest that
// powers off takes its interface with it, so it is simply absent from the next
// answer, which is exactly what the host reports after ADR 0010's shutdown.
type scriptedHost struct {
	mu       sync.Mutex
	counters map[string]domain.GuestNetwork
	err      error
	block    bool
	asked    [][]string
}

func (h *scriptedHost) Read(ctx context.Context, names []string) (map[string]domain.GuestNetwork, error) {
	h.mu.Lock()
	h.asked = append(h.asked, append([]string(nil), names...))
	counters, err, block := h.counters, h.err, h.block
	h.mu.Unlock()
	if block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return counters, err
}

func (h *scriptedHost) set(counters map[string]domain.GuestNetwork, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.counters, h.err = counters, err
}

func (h *scriptedHost) setBlock(block bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.block = block
}

func (h *scriptedHost) reads() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.asked)
}

// sink stands in for the store: it keeps the samples the recorder hands it and,
// like the real store, keeps the first measured sample of an instance.
type sink struct {
	mu      sync.Mutex
	samples []operations.TrafficSample
	fail    bool
	stored  map[string]operations.TrafficSample
}

func (s *sink) record(_ context.Context, sample operations.TrafficSample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.samples = append(s.samples, sample)
	if s.fail {
		return
	}
	if s.stored == nil {
		s.stored = map[string]operations.TrafficSample{}
	}
	if prior, ok := s.stored[sample.InstanceID]; !ok || (!prior.Measured && sample.Measured) {
		s.stored[sample.InstanceID] = sample
	}
}

func (s *sink) setFail(fail bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = fail
}

func hostInstance(platform domain.Platform) operations.Instance {
	return operations.Instance{ID: "trf-linux-4x8-1abd", Repo: "budgie-at/budgie", Platform: platform,
		Profile: "linux-4x8", CreatedAt: time.Unix(100, 0).UTC()}
}

// clock is a settable clock safe for the sweeper's goroutine.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func hostRecorder(host HostCounters, c *clock, warn func(string, ...any)) (*HostTrafficRecorder, *sink) {
	s := &sink{}
	return NewHostTrafficRecorder(host, time.Second, s.record, c.now, warn), s
}

// TestTartGuestTrafficSurvivesThePowerOffWithoutAnyGuestTraffic replays #414 on
// the host's counters. The guest is read from the host while it is alive and its
// interface is gone by the time the drain records the job. The recorded sample
// is the last reading, no command ever reaches the guest, and it is never zero.
func TestTartGuestTrafficSurvivesThePowerOffWithoutAnyGuestTraffic(t *testing.T) {
	instance := hostInstance(domain.PlatformLinux)
	c := &clock{t: time.Unix(1000, 0).UTC()}
	host := &scriptedHost{}
	recorder, out := hostRecorder(host, c, nil)

	host.set(map[string]domain.GuestNetwork{instance.ID: {ReceiveBytes: 100, TransmitBytes: 10}}, nil)
	recorder.Sweep(context.Background(), []operations.Instance{instance})
	c.add(5 * time.Second)
	host.set(map[string]domain.GuestNetwork{instance.ID: {ReceiveBytes: 900, TransmitBytes: 40}}, nil)
	recorder.Sweep(context.Background(), []operations.Instance{instance})
	readAt := c.now()

	c.add(7 * time.Second)
	host.set(map[string]domain.GuestNetwork{}, nil) // the guest powered itself off
	recorder.RecordGuestTraffic(context.Background(), instance)

	if len(out.samples) != 1 {
		t.Fatalf("samples = %+v", out.samples)
	}
	got := out.samples[0]
	if !got.Measured || got.ReceiveBytes != 900 || got.TransmitBytes != 40 || got.Unmeasured != "" {
		t.Fatalf("sample = %+v, want the last reading, measured", got)
	}
	if !got.SampledAt.Equal(readAt) {
		t.Fatalf("sampledAt = %v, want the time of the reading %v", got.SampledAt, readAt)
	}
	if !got.Valid() {
		t.Fatalf("sample = %+v is not valid", got)
	}
}

// TestAFailedStoreWriteDoesNotLoseTheReadingTheDrainRetries replays the drain
// retry: the first write fails, the guest has powered off by the retry, and the
// retry must still store the measured reading, not an unmeasured sample.
func TestAFailedStoreWriteDoesNotLoseTheReadingTheDrainRetries(t *testing.T) {
	instance := hostInstance(domain.PlatformLinux)
	c := &clock{t: time.Unix(1500, 0).UTC()}
	host := &scriptedHost{}
	recorder, out := hostRecorder(host, c, nil)

	host.set(map[string]domain.GuestNetwork{instance.ID: {ReceiveBytes: 700, TransmitBytes: 30}}, nil)
	recorder.Sweep(context.Background(), []operations.Instance{instance})

	out.setFail(true)
	recorder.RecordGuestTraffic(context.Background(), instance)
	if len(out.stored) != 0 {
		t.Fatalf("stored = %+v, the failed write must store nothing", out.stored)
	}

	// The retry finds the guest powered off: the host no longer shows it, and a
	// sweep that saw it gone keeps the reading.
	out.setFail(false)
	host.set(map[string]domain.GuestNetwork{}, nil)
	recorder.Sweep(context.Background(), []operations.Instance{instance})
	recorder.RecordGuestTraffic(context.Background(), instance)
	stored, ok := out.stored[instance.ID]
	if !ok || !stored.Measured || stored.ReceiveBytes != 700 || stored.TransmitBytes != 30 {
		t.Fatalf("stored = %+v %v, want the measured reading", stored, ok)
	}

	// Recording the same job again cannot change what is stored.
	recorder.RecordGuestTraffic(context.Background(), instance)
	if again := out.stored[instance.ID]; again != stored {
		t.Fatalf("a repeated record changed the stored sample: %+v then %+v", stored, again)
	}
}

// TestUnreadGuestTrafficStaysUnmeasuredNeverZero: a job shorter than the first
// sweep, or a daemon restarted mid-job, has no reading. That is an unobserved
// guest, not one that moved nothing (AGENTS.md rule 4).
func TestUnreadGuestTrafficStaysUnmeasuredNeverZero(t *testing.T) {
	c := &clock{t: time.Unix(2000, 0).UTC()}
	recorder, out := hostRecorder(&scriptedHost{}, c, nil)
	recorder.RecordGuestTraffic(context.Background(), hostInstance(domain.PlatformLinux))
	got := out.samples[0]
	if got.Measured || got.Unmeasured != operations.TrafficNoReading || got.ReceiveBytes != 0 || got.TransmitBytes != 0 {
		t.Fatalf("sample = %+v", got)
	}
	if !got.Valid() {
		t.Fatalf("sample = %+v is not valid", got)
	}
}

func TestHostTrafficSweepIsBestEffortAndBounded(t *testing.T) {
	instance := hostInstance(domain.PlatformMacOS)
	c := &clock{t: time.Unix(3000, 0).UTC()}
	host := &scriptedHost{}
	recorder, out := hostRecorder(host, c, nil)

	host.set(map[string]domain.GuestNetwork{instance.ID: {ReceiveBytes: 5, TransmitBytes: 6}}, nil)
	recorder.Sweep(context.Background(), []operations.Instance{instance})

	// An unreadable host keeps the previous reading: it is not evidence the guest moved nothing.
	host.set(nil, errors.New("netstat failed"))
	recorder.Sweep(context.Background(), []operations.Instance{instance})
	if last, ok := recorder.last.get(instance.ID); !ok || last.receive != 5 {
		t.Fatalf("a failed read must keep the previous reading, got %+v %v", last, ok)
	}

	// A host command that hangs is bounded by the deadline and never blocks the caller for good.
	host.setBlock(true)
	recorder.timeout = 20 * time.Millisecond
	done := make(chan struct{})
	go func() { recorder.Sweep(context.Background(), []operations.Instance{instance}); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a hung host read must be bounded by the recorder's deadline")
	}
	if len(out.samples) != 0 {
		t.Fatal("a sweep never records a sample")
	}

	// Instances no longer live are dropped, so the map is bounded by the live set.
	host.setBlock(false)
	recorder.Sweep(context.Background(), nil)
	if _, ok := recorder.last.get(instance.ID); ok {
		t.Fatal("a reading for an instance that is no longer live must be dropped")
	}
}

func TestHostTrafficSweepAsksOnceForEveryLiveGuest(t *testing.T) {
	c := &clock{t: time.Unix(4000, 0).UTC()}
	host := &scriptedHost{}
	recorder, _ := hostRecorder(host, c, nil)
	live := []operations.Instance{{ID: "trf-linux-4x8-aa", State: operations.StateRunning},
		{ID: "trf-linux-4x8-bb", State: operations.StateDraining}}
	recorder.Sweep(context.Background(), live)
	if len(host.asked) != 1 || len(host.asked[0]) != 2 {
		t.Fatalf("asked = %v, want one host read for both guests (a draining guest may still be up)", host.asked)
	}
	recorder.Sweep(context.Background(), nil)
	if len(host.asked) != 1 {
		t.Fatalf("no live guest, no host read: %v", host.asked)
	}
}

// safeWarn collects warnings from the sweeper's goroutine.
type safeWarn struct {
	mu       sync.Mutex
	messages []string
}

func (w *safeWarn) warn(message string, _ ...any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.messages = append(w.messages, message)
}

func (w *safeWarn) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.messages)
}

type swappableSource struct {
	mu   sync.Mutex
	live []operations.Instance
	err  error
}

func (s *swappableSource) set(live []operations.Instance, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.live, s.err = live, err
}

func (s *swappableSource) LiveInstances(context.Context) ([]operations.Instance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.live, s.err
}

// TestHostTrafficSweeperSweepsOnEachTickAndStopsWithItsContext hands the sweeper
// one tick at a time. A tick is unbuffered, so a send returns once the sweeper
// has received it, not once the sweep is done; the test therefore waits for each
// sweep's own observable effect (a host read, a warning) before moving on.
func TestHostTrafficSweeperSweepsOnEachTickAndStopsWithItsContext(t *testing.T) {
	instance := hostInstance(domain.PlatformLinux)
	c := &clock{t: time.Unix(6000, 0).UTC()}
	host := &scriptedHost{counters: map[string]domain.GuestNetwork{instance.ID: {ReceiveBytes: 1, TransmitBytes: 2}}}
	warned := &safeWarn{}
	recorder, _ := hostRecorder(host, c, warned.warn)
	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan struct{})
	source := &swappableSource{live: []operations.Instance{instance}}
	go func() { HostTrafficSweeper{Source: source, Recorder: recorder, Tick: ticks}.Run(ctx); close(stopped) }()

	ticks <- c.now()
	waitFor(t, "the first sweep to read the host", func() bool { return host.reads() == 1 })
	if _, ok := recorder.last.get(instance.ID); !ok {
		t.Fatal("the first tick must have kept a reading")
	}

	source.set(nil, errors.New("store closed"))
	ticks <- c.now()
	waitFor(t, "the failing list to be warned about", func() bool { return warned.count() == 1 })
	if host.reads() != 1 {
		t.Fatal("a sweep that could not list the live guests must not read the host")
	}

	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the sweeper must stop with its context")
	}
}

// TestAnUnreadableHostIsWarnedAboutOnceNotEveryInterval: a host whose counters
// stay unreadable would otherwise log every five seconds, and one whose
// counters come back would never say so. It drives Sweep directly, which is
// synchronous, so nothing here races with a sweeper goroutine.
func TestAnUnreadableHostIsWarnedAboutOnceNotEveryInterval(t *testing.T) {
	instance := hostInstance(domain.PlatformLinux)
	c := &clock{t: time.Unix(7000, 0).UTC()}
	host := &scriptedHost{}
	warned := &safeWarn{}
	recorder, _ := hostRecorder(host, c, warned.warn)
	live := []operations.Instance{instance}

	host.set(nil, errors.New("netstat failed"))
	for range 3 {
		recorder.Sweep(context.Background(), live)
	}
	if warned.count() != 1 {
		t.Fatalf("warnings = %v, want exactly one for a persistent failure", warned.messages)
	}
	host.set(map[string]domain.GuestNetwork{instance.ID: {ReceiveBytes: 1}}, nil)
	recorder.Sweep(context.Background(), live)
	recorder.Sweep(context.Background(), live)
	if warned.count() != 2 {
		t.Fatalf("warnings = %v, want one more when the host is readable again", warned.messages)
	}
	recorder.warn = nil
	host.set(nil, errors.New("again"))
	recorder.Sweep(context.Background(), live) // a nil warn must be safe
}

// TestAStaleReadingKeepsItsOwnAgeWhileTheHostIsUnreadable: traffic after the
// last successful read is omitted, with no fixed bound while reads fail, and the
// stored SampledAt is what shows how old the reading was.
func TestAStaleReadingKeepsItsOwnAgeWhileTheHostIsUnreadable(t *testing.T) {
	instance := hostInstance(domain.PlatformLinux)
	c := &clock{t: time.Unix(8000, 0).UTC()}
	host := &scriptedHost{}
	recorder, out := hostRecorder(host, c, nil)
	host.set(map[string]domain.GuestNetwork{instance.ID: {ReceiveBytes: 50, TransmitBytes: 5}}, nil)
	recorder.Sweep(context.Background(), []operations.Instance{instance})
	readAt := c.now()

	host.set(nil, errors.New("netstat failed"))
	for range 12 {
		c.add(HostTrafficInterval)
		recorder.Sweep(context.Background(), []operations.Instance{instance})
	}
	recorder.RecordGuestTraffic(context.Background(), instance)
	got := out.samples[0]
	if !got.Measured || got.ReceiveBytes != 50 || !got.SampledAt.Equal(readAt) {
		t.Fatalf("sample = %+v, want the old reading with its own time %v", got, readAt)
	}
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}
