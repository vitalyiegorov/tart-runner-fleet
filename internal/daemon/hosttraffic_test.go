package daemon

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/telemetry"
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

func hostRecorder(host hostCounters, clock *time.Time) (hostTrafficRecorder, *trafficStoreFake, *trafficMetricsFake) {
	store, metrics := &trafficStoreFake{changed: true}, &trafficMetricsFake{}
	return hostTrafficRecorder{Host: host, Timeout: time.Second, Store: store, Metrics: metrics,
		Now: func() time.Time { return *clock }, Last: newGuestReadings()}, store, metrics
}

// TestTartGuestTrafficSurvivesThePowerOffWithoutAnyGuestTraffic replays #414 on
// the host's counters. The guest is read from the host while it is alive and its
// interface is gone by the time the drain records the job. The recorded sample
// is the last reading, no command ever reaches the guest, and it is never zero.
func TestTartGuestTrafficSurvivesThePowerOffWithoutAnyGuestTraffic(t *testing.T) {
	instance := trafficInstance(domain.PlatformLinux)
	clock := time.Unix(1000, 0).UTC()
	host := &scriptedHost{}
	recorder, store, metrics := hostRecorder(host, &clock)

	host.set(map[string]domain.GuestNetwork{instance.ID: {ReceiveBytes: 100, TransmitBytes: 10}}, nil)
	recorder.Sweep(context.Background(), []operations.Instance{instance})
	clock = clock.Add(5 * time.Second)
	host.set(map[string]domain.GuestNetwork{instance.ID: {ReceiveBytes: 900, TransmitBytes: 40}}, nil)
	recorder.Sweep(context.Background(), []operations.Instance{instance})
	readAt := clock

	clock = clock.Add(7 * time.Second)
	host.set(map[string]domain.GuestNetwork{}, nil) // the guest powered itself off
	recorder.RecordGuestTraffic(context.Background(), instance)

	if len(store.samples) != 1 {
		t.Fatalf("samples = %+v", store.samples)
	}
	got := store.samples[0]
	if !got.Measured || got.ReceiveBytes != 900 || got.TransmitBytes != 40 || got.Unmeasured != "" {
		t.Fatalf("sample = %+v, want the last reading, measured", got)
	}
	if !got.SampledAt.Equal(readAt) {
		t.Fatalf("sampledAt = %v, want the time of the reading %v", got.SampledAt, readAt)
	}
	if !got.Valid() || len(metrics.metrics) != 1 || metrics.metrics[0].ReceiveBytes != 900 {
		t.Fatalf("valid=%v metrics=%+v", got.Valid(), metrics.metrics)
	}
	if _, kept := recorder.Last.get(instance.ID); kept {
		t.Fatal("a recorded instance's reading must be forgotten")
	}
}

// TestUnreadGuestTrafficStaysUnmeasuredNeverZero: a job shorter than the first
// sweep, or a daemon restarted mid-job, has no reading. That is an unobserved
// guest, not one that moved nothing (AGENTS.md rule 4).
func TestUnreadGuestTrafficStaysUnmeasuredNeverZero(t *testing.T) {
	clock := time.Unix(2000, 0).UTC()
	recorder, store, metrics := hostRecorder(&scriptedHost{}, &clock)
	recorder.RecordGuestTraffic(context.Background(), trafficInstance(domain.PlatformLinux))
	got := store.samples[0]
	if got.Measured || got.Unmeasured != operations.TrafficNoReading || got.ReceiveBytes != 0 || got.TransmitBytes != 0 {
		t.Fatalf("sample = %+v", got)
	}
	if !got.Valid() || len(metrics.metrics) != 1 || metrics.metrics[0].Measured {
		t.Fatalf("valid=%v metrics=%+v", got.Valid(), metrics.metrics)
	}
}

func TestHostTrafficSweepIsBestEffortAndBounded(t *testing.T) {
	instance := trafficInstance(domain.PlatformMacOS)
	clock := time.Unix(3000, 0).UTC()
	host := &scriptedHost{}
	recorder, store, _ := hostRecorder(host, &clock)

	host.set(map[string]domain.GuestNetwork{instance.ID: {ReceiveBytes: 5, TransmitBytes: 6}}, nil)
	recorder.Sweep(context.Background(), []operations.Instance{instance})

	// An unreadable host keeps the previous reading: it is not evidence the guest moved nothing.
	host.set(nil, errors.New("netstat failed"))
	recorder.Sweep(context.Background(), []operations.Instance{instance})
	if last, ok := recorder.Last.get(instance.ID); !ok || last.receive != 5 {
		t.Fatalf("a failed read must keep the previous reading, got %+v %v", last, ok)
	}

	// A host command that hangs is bounded by the deadline and never blocks the caller for good.
	host.mu.Lock()
	host.block = true
	host.mu.Unlock()
	recorder.Timeout = 20 * time.Millisecond
	done := make(chan struct{})
	go func() { recorder.Sweep(context.Background(), []operations.Instance{instance}); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a hung host read must be bounded by the recorder's deadline")
	}
	if len(store.samples) != 0 {
		t.Fatal("a sweep never touches the store")
	}

	// Instances no longer live are dropped, so the map is bounded by the live set.
	host.mu.Lock()
	host.block = false
	host.mu.Unlock()
	recorder.Sweep(context.Background(), nil)
	if _, ok := recorder.Last.get(instance.ID); ok {
		t.Fatal("a reading for an instance that is no longer live must be dropped")
	}
}

func TestHostTrafficSweepAsksOnceForEveryLiveGuest(t *testing.T) {
	clock := time.Unix(4000, 0).UTC()
	host := &scriptedHost{}
	recorder, _, _ := hostRecorder(host, &clock)
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

func TestHostTrafficRecorderSurvivesAFailingStore(t *testing.T) {
	clock := time.Unix(5000, 0).UTC()
	recorder, store, _ := hostRecorder(&scriptedHost{}, &clock)
	var warned []string
	recorder.Warn = func(message string, _ ...any) { warned = append(warned, message) }
	store.err = errors.New("disk full")
	recorder.RecordGuestTraffic(context.Background(), trafficInstance(domain.PlatformLinux))
	store.err, store.changed = nil, false
	recorder.RecordGuestTraffic(context.Background(), trafficInstance(domain.PlatformLinux))
	store.changed = true
	recorder.Metrics = &failingMetrics{}
	recorder.RecordGuestTraffic(context.Background(), trafficInstance(domain.PlatformLinux))
	if len(warned) != 2 {
		t.Fatalf("warnings = %v, want one for the store and one for the metrics", warned)
	}
	recorder.Warn = nil
	store.err = errors.New("again")
	recorder.RecordGuestTraffic(context.Background(), trafficInstance(domain.PlatformLinux))
}

type failingMetrics struct{}

func (failingMetrics) AddGuestTraffic(_ telemetry.GuestTrafficMetric) error {
	return errors.New("closed")
}

func TestHostTrafficSweeperSweepsOnEachTickAndStopsWithItsContext(t *testing.T) {
	instance := trafficInstance(domain.PlatformLinux)
	clock := time.Unix(6000, 0).UTC()
	host := &scriptedHost{counters: map[string]domain.GuestNetwork{instance.ID: {ReceiveBytes: 1, TransmitBytes: 2}}}
	recorder, _, _ := hostRecorder(host, &clock)
	var warned []string
	recorder.Warn = func(message string, _ ...any) { warned = append(warned, message) }
	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	source := &swappableSource{live: []operations.Instance{instance}}
	go func() { hostTrafficSweeper{Source: source, Recorder: recorder, Tick: ticks}.Run(ctx); close(stopped) }()

	ticks <- clock
	source.set(nil, errors.New("store closed"))
	ticks <- clock
	ticks <- clock // a third send returns only after the second sweep finished
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the sweeper must stop with its context")
	}
	if _, ok := recorder.Last.get(instance.ID); !ok {
		t.Fatal("the first tick must have kept a reading")
	}
	if len(warned) == 0 {
		t.Fatal("a failing live-instance list must be warned about, not fatal")
	}
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

func TestGuestTrafficIsReadFromTheHostOnTartAndByExecElsewhere(t *testing.T) {
	cfg := config.Default()
	store, metrics := &trafficStoreFake{}, &trafficMetricsFake{}

	tartNode := dependencies{guestExec: applePlatform().guestExec, hostTraffic: applePlatform().hostTraffic, now: time.Now}
	recorder, sweeper := tartNode.guestTraffic(store, metrics, cfg, nil)
	if recorder == nil || sweeper == nil {
		t.Fatal("a Tart node records from host counters and sweeps them")
	}
	if _, ok := recorder.(hostTrafficRecorder); !ok {
		t.Fatalf("a Tart node must not exec into its guests for traffic, got %T", recorder)
	}

	podman := linuxPlatform()
	cfg.Executor.Backend = config.ExecutorPodman
	recorder, sweeper = (dependencies{guestExec: podman.guestExec, hostTraffic: podman.hostTraffic, now: time.Now}).
		guestTraffic(store, metrics, cfg, nil)
	if _, ok := recorder.(guestTrafficRecorder); !ok || sweeper != nil {
		t.Fatalf("a podman node keeps its end-of-job exec and has nothing to sweep: %T %v", recorder, sweeper)
	}

	cfg.Executor.Backend = ""
	if recorder, sweeper = (dependencies{guestExec: podman.guestExec, hostTraffic: podman.hostTraffic}).
		guestTraffic(store, metrics, cfg, nil); recorder != nil || sweeper != nil {
		t.Fatalf("a node with no guest records nothing: %v %v", recorder, sweeper)
	}
	if recorder, sweeper = (dependencies{}).guestTraffic(store, metrics, cfg, nil); recorder != nil || sweeper != nil {
		t.Fatalf("no wiring, no recorder: %v %v", recorder, sweeper)
	}
}

// TestAnUnreadableHostIsWarnedAboutOnceNotEveryInterval: a host whose counters
// stay unreadable would otherwise log every five seconds, and one whose
// counters come back would never say so.
func TestAnUnreadableHostIsWarnedAboutOnceNotEveryInterval(t *testing.T) {
	instance := trafficInstance(domain.PlatformLinux)
	clock := time.Unix(7000, 0).UTC()
	host := &scriptedHost{}
	recorder, _, _ := hostRecorder(host, &clock)
	var warned []string
	recorder.Warn = func(message string, _ ...any) { warned = append(warned, message) }
	live := []operations.Instance{instance}

	host.set(nil, errors.New("netstat failed"))
	for range 3 {
		recorder.Sweep(context.Background(), live)
	}
	if len(warned) != 1 {
		t.Fatalf("warnings = %v, want exactly one for a persistent failure", warned)
	}
	host.set(map[string]domain.GuestNetwork{instance.ID: {ReceiveBytes: 1}}, nil)
	recorder.Sweep(context.Background(), live)
	recorder.Sweep(context.Background(), live)
	if len(warned) != 2 {
		t.Fatalf("warnings = %v, want one more when the host is readable again", warned)
	}
	recorder.Warn = nil
	host.set(nil, errors.New("again"))
	recorder.Sweep(context.Background(), live) // a nil Warn must be safe
}
