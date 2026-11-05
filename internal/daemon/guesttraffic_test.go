package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/telemetry"
)

type scriptedGuest struct {
	args   []string
	output string
	err    error
	block  bool
}

func (g *scriptedGuest) Run(ctx context.Context, args ...string) ([]byte, error) {
	g.args = args
	if g.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return []byte(g.output), g.err
}

type trafficStoreFake struct {
	samples []operations.TrafficSample
	changed bool
	err     error
	records []operations.TrafficRecord
	limit   int
}

func (s *trafficStoreFake) RecordGuestTraffic(_ context.Context, sample operations.TrafficSample) (bool, error) {
	s.samples = append(s.samples, sample)
	return s.changed, s.err
}

func (s *trafficStoreFake) GuestTraffic(_ context.Context, _ time.Time, limit int) ([]operations.TrafficRecord, error) {
	s.limit = limit
	return s.records, s.err
}

type trafficMetricsFake struct {
	metrics []telemetry.GuestTrafficMetric
}

func (m *trafficMetricsFake) AddGuestTraffic(metric telemetry.GuestTrafficMetric) error {
	m.metrics = append(m.metrics, metric)
	if metric.Repo == "" {
		return errors.New("unbounded")
	}
	return nil
}

func trafficInstance(platform domain.Platform) operations.Instance {
	return operations.Instance{ID: "trf-linux-4x8-1abd", Repo: "budgie-at/budgie", Platform: platform,
		Profile: "linux-4x8", CreatedAt: time.Unix(100, 0).UTC()}
}

const linuxCounters = "Inter-|\n face |\n lo: 9 0 0 0 0 0 0 0 9 0 0 0 0 0 0 0\n eth0: 1345000000 0 0 0 0 0 0 0 21000000 0 0 0 0 0 0 0\n"

func TestGuestTrafficRecorderSamplesThroughTheGuestExecPath(t *testing.T) {
	guest := &scriptedGuest{output: linuxCounters}
	store := &trafficStoreFake{changed: true}
	metrics := &trafficMetricsFake{}
	recorder := guestTrafficRecorder{Runner: guest, Timeout: time.Second, Store: store, Metrics: metrics,
		Now: func() time.Time { return time.Unix(700, 0) }}
	recorder.RecordGuestTraffic(context.Background(), trafficInstance(domain.PlatformLinux))
	if got := strings.Join(guest.args, " "); got != "exec trf-linux-4x8-1abd cat /proc/net/dev" {
		t.Fatalf("argv = %q", got)
	}
	if len(store.samples) != 1 {
		t.Fatalf("samples = %+v", store.samples)
	}
	sample := store.samples[0]
	if !sample.Measured || sample.ReceiveBytes != 1345000000 || sample.TransmitBytes != 21000000 ||
		sample.Repo != "budgie-at/budgie" || sample.Profile != "linux-4x8" || !sample.StartedAt.Equal(time.Unix(100, 0)) ||
		!sample.SampledAt.Equal(time.Unix(700, 0)) {
		t.Fatalf("sample = %+v", sample)
	}
	if len(metrics.metrics) != 1 || metrics.metrics[0].ReceiveBytes != 1345000000 || metrics.metrics[0].Platform != "linux" {
		t.Fatalf("metrics = %+v", metrics.metrics)
	}
	store.changed = false
	recorder.RecordGuestTraffic(context.Background(), trafficInstance(domain.PlatformLinux))
	if len(metrics.metrics) != 1 {
		t.Fatal("a sample the store did not change must not be counted again")
	}
}

// TestGuestTrafficRecorderRecordsAnUnreadableSampleAsUnmeasured is AGENTS.md
// rule 4 at the producer: every failure becomes a closed reason, never zeros.
func TestGuestTrafficRecorderRecordsAnUnreadableSampleAsUnmeasured(t *testing.T) {
	for name, testCase := range map[string]struct {
		guest    *scriptedGuest
		platform domain.Platform
		id       string
		want     string
	}{
		"exec failed":      {guest: &scriptedGuest{err: errors.New("no such container")}, platform: domain.PlatformLinux, want: operations.TrafficExecFailed},
		"exec timed out":   {guest: &scriptedGuest{block: true}, platform: domain.PlatformLinux, want: operations.TrafficExecTimeout},
		"unreadable":       {guest: &scriptedGuest{output: "garbage"}, platform: domain.PlatformMacOS, want: operations.TrafficUnreadable},
		"unknown platform": {guest: &scriptedGuest{}, platform: domain.Platform(""), want: operations.TrafficNoCommand},
		"invalid name":     {guest: &scriptedGuest{}, platform: domain.PlatformLinux, id: "bad name", want: operations.TrafficNoCommand},
	} {
		t.Run(name, func(t *testing.T) {
			store := &trafficStoreFake{changed: true}
			metrics := &trafficMetricsFake{}
			instance := trafficInstance(testCase.platform)
			if testCase.id != "" {
				instance.ID = testCase.id
			}
			recorder := guestTrafficRecorder{Runner: testCase.guest, Timeout: time.Millisecond, Store: store,
				Metrics: metrics, Now: time.Now}
			recorder.RecordGuestTraffic(context.Background(), instance)
			if len(store.samples) != 1 {
				t.Fatalf("samples = %+v", store.samples)
			}
			sample := store.samples[0]
			if sample.Measured || sample.Unmeasured != testCase.want || sample.ReceiveBytes != 0 {
				t.Fatalf("sample = %+v, want unmeasured %q", sample, testCase.want)
			}
			if len(metrics.metrics) != 1 || metrics.metrics[0].Measured {
				t.Fatalf("an unmeasured job counts as unmeasured: %+v", metrics.metrics)
			}
		})
	}
}

func TestGuestTrafficRecorderSurvivesAFailingStore(t *testing.T) {
	store := &trafficStoreFake{err: errors.New("locked")}
	metrics := &trafficMetricsFake{}
	warned := 0
	recorder := guestTrafficRecorder{Runner: &scriptedGuest{output: linuxCounters}, Timeout: time.Second, Store: store,
		Metrics: metrics, Now: time.Now, Warn: func(string, ...any) { warned++ }}
	recorder.RecordGuestTraffic(context.Background(), trafficInstance(domain.PlatformLinux))
	instance := trafficInstance(domain.PlatformLinux)
	instance.Repo = ""
	store.err, store.changed = nil, true
	recorder.RecordGuestTraffic(context.Background(), instance)
	if warned != 2 || len(metrics.metrics) != 1 {
		t.Fatalf("both a store failure and a refused metric are warned once each: warned=%d metrics=%+v", warned, metrics.metrics)
	}
	recorder.Warn = nil
	recorder.RecordGuestTraffic(context.Background(), instance)
}

func TestGuestTrafficReaderTruncatesAndNeverZeroesUnmeasuredJobs(t *testing.T) {
	records := make([]operations.TrafficRecord, adminapi.MaxTrafficJobs+1)
	for index := range records {
		records[index] = operations.TrafficRecord{TrafficSample: operations.TrafficSample{InstanceID: "trf-a",
			Repo: "o/r", Platform: domain.PlatformLinux, Profile: "p", Measured: index == 0, ReceiveBytes: 5, TransmitBytes: 6,
			Unmeasured: map[bool]string{true: "", false: operations.TrafficExecFailed}[index == 0]},
			WorkflowRef: "o/r/.github/workflows/ci.yml@refs/heads/main", JobName: "build", RunID: 9, ScaleSetID: 3}
	}
	store := &trafficStoreFake{records: records}
	jobs, truncated, err := guestTrafficReader{Store: store}.GuestTraffic(context.Background(), time.Unix(0, 0))
	if err != nil || !truncated || len(jobs) != adminapi.MaxTrafficJobs || store.limit != adminapi.MaxTrafficJobs+1 {
		t.Fatalf("jobs=%d truncated=%t err=%v limit=%d", len(jobs), truncated, err, store.limit)
	}
	if first := jobs[0]; !first.Measured || *first.ReceiveBytes != 5 || *first.TransmitBytes != 6 || first.Job != "build" ||
		first.WorkflowRef == "" || first.RunID != 9 || first.ScaleSetID != 3 || first.Platform != "linux" {
		t.Fatalf("measured job = %+v", first)
	}
	if second := jobs[1]; second.ReceiveBytes != nil || second.TransmitBytes != nil || second.Unmeasured == "" {
		t.Fatalf("an unmeasured job must carry no bytes: %+v", second)
	}
	store.err = errors.New("closed")
	if jobs, _, err := (guestTrafficReader{Store: store}).GuestTraffic(context.Background(), time.Unix(0, 0)); err == nil || jobs != nil {
		t.Fatalf("an unreadable store is an error, never an empty answer: %+v %v", jobs, err)
	}
}

func TestGuestTrafficIsWiredOnlyWhereAGuestCanBeExecuted(t *testing.T) {
	cfg := config.Default()
	if _, ok := (dependencies{}).guestTraffic(&trafficStoreFake{}, &trafficMetricsFake{}, cfg, nil); ok {
		t.Fatal("no exec runner must wire no recorder")
	}
	podmanless := linuxPlatform()
	if _, ok := (dependencies{guestExec: podmanless.guestExec}).guestTraffic(&trafficStoreFake{},
		&trafficMetricsFake{}, cfg, nil); ok {
		t.Fatal("a Linux node with no container backend has no guest")
	}
	cfg.Executor.Backend = config.ExecutorPodman
	if _, ok := (dependencies{guestExec: podmanless.guestExec, now: time.Now}).guestTraffic(&trafficStoreFake{},
		&trafficMetricsFake{}, cfg, nil); !ok {
		t.Fatal("a podman node samples its containers")
	}
	if _, ok := (dependencies{guestExec: applePlatform().guestExec, now: time.Now}).guestTraffic(&trafficStoreFake{},
		&trafficMetricsFake{}, cfg, nil); !ok {
		t.Fatal("a Tart node samples its VMs")
	}
}

// poweringGuest is a Tart guest as ADR 0010 makes it: it answers while its
// runner is alive, and the moment the ephemeral runner exits it powers itself
// off, after which an exec hangs (mid-shutdown) or is refused (stopped).
type poweringGuest struct {
	output string
	off    bool
	hang   bool
}

func (g *poweringGuest) Run(ctx context.Context, _ ...string) ([]byte, error) {
	switch {
	case !g.off:
		return []byte(g.output), nil
	case g.hang:
		<-ctx.Done()
		return nil, ctx.Err()
	default:
		return nil, errors.New(`VM is not running`)
	}
}

func countersOf(received, transmitted int) string {
	return fmt.Sprintf("Inter-|\n face |\n lo: 9 0 0 0 0 0 0 0 9 0 0 0 0 0 0 0\n eth0: %d 0 0 0 0 0 0 0 %d 0 0 0 0 0 0 0\n",
		received, transmitted)
}

// TestGuestTrafficSurvivesTheGuestPoweringOffBeforeTheDrainSamples reproduces
// the 2026-11-05 finding: Tart nodes measured ~3% of jobs because the guest has
// already powered itself off when the drain reaches its first stop, so the one
// end-of-job read times out or is refused. A reading taken while the job ran
// must stand in for it.
func TestGuestTrafficSurvivesTheGuestPoweringOffBeforeTheDrainSamples(t *testing.T) {
	for name, hang := range map[string]bool{"mid-shutdown exec hangs": true, "stopped guest refuses": false} {
		t.Run(name, func(t *testing.T) {
			guest := &poweringGuest{output: countersOf(100, 10), hang: hang}
			store := &trafficStoreFake{changed: true}
			metrics := &trafficMetricsFake{}
			clock := time.Unix(1000, 0).UTC()
			recorder := guestTrafficRecorder{Runner: guest, Timeout: time.Millisecond, Store: store, Metrics: metrics,
				Now: func() time.Time { return clock }, Last: newGuestReadings()}
			instance := trafficInstance(domain.PlatformLinux)
			instance.State = operations.StateRunning
			recorder.Sweep(context.Background(), []operations.Instance{instance})
			guest.output, clock = countersOf(500, 50), clock.Add(15*time.Second)
			recorder.Sweep(context.Background(), []operations.Instance{instance})
			guest.off, clock = true, clock.Add(10*time.Second)
			recorder.RecordGuestTraffic(context.Background(), instance)
			if len(store.samples) != 1 {
				t.Fatalf("samples = %+v", store.samples)
			}
			sample := store.samples[0]
			if !sample.Measured || sample.ReceiveBytes != 500 || sample.TransmitBytes != 50 ||
				!sample.SampledAt.Equal(time.Unix(1015, 0)) {
				t.Fatalf("sample = %+v; want the last reading taken while the guest was alive", sample)
			}
			if len(metrics.metrics) != 1 || !metrics.metrics[0].Measured || metrics.metrics[0].ReceiveBytes != 500 {
				t.Fatalf("metrics = %+v", metrics.metrics)
			}
		})
	}
}

func TestTrafficOrNilKeepsTheDrainsNilCheckHonest(t *testing.T) {
	if trafficOrNil(guestTrafficRecorder{}, false) != nil {
		t.Fatal("a node with no guest must hand the drain an untyped nil")
	}
	if trafficOrNil(guestTrafficRecorder{}, true) == nil {
		t.Fatal("a node with a guest hands the drain its recorder")
	}
}

// TestGuestTrafficEndOfJobReadingWinsOverAnEarlierOne keeps the sweep a fallback:
// when the guest still answers at the end, that reading is the job's.
func TestGuestTrafficEndOfJobReadingWinsOverAnEarlierOne(t *testing.T) {
	guest := &poweringGuest{output: countersOf(100, 10)}
	store := &trafficStoreFake{changed: true}
	recorder := guestTrafficRecorder{Runner: guest, Timeout: time.Second, Store: store, Metrics: &trafficMetricsFake{},
		Now: func() time.Time { return time.Unix(2000, 0) }, Last: newGuestReadings()}
	instance := trafficInstance(domain.PlatformLinux)
	instance.State = operations.StateRunning
	recorder.Sweep(context.Background(), []operations.Instance{instance})
	guest.output = countersOf(900, 90)
	recorder.RecordGuestTraffic(context.Background(), instance)
	if got := store.samples[0]; !got.Measured || got.ReceiveBytes != 900 {
		t.Fatalf("sample = %+v; want the end-of-job reading", got)
	}
	if _, kept := recorder.Last.get(instance.ID); kept {
		t.Fatal("a recorded instance's reading must be forgotten")
	}
}

// TestGuestTrafficSweepIsBoundedAndBestEffort covers what a sweep reads, what it
// keeps, and that a failed read never replaces a good one nor blocks past its
// deadline.
func TestGuestTrafficSweepIsBoundedAndBestEffort(t *testing.T) {
	guest := &poweringGuest{output: countersOf(100, 10)}
	reads := &readCounter{inner: guest}
	recorder := guestTrafficRecorder{Runner: reads, Timeout: time.Millisecond, Store: &trafficStoreFake{},
		Metrics: &trafficMetricsFake{}, Now: func() time.Time { return time.Unix(3000, 0) }, Last: newGuestReadings()}
	instances := map[operations.State]operations.Instance{}
	for index, state := range []operations.State{operations.StateOnlineIdle, operations.StateAssigned, operations.StateRunning,
		operations.StateDraining, operations.StateStopping, operations.StateBooting} {
		instance := trafficInstance(domain.PlatformLinux)
		instance.ID, instance.State = fmt.Sprintf("trf-linux-4x8-%04x", index), state
		instances[state] = instance
	}
	live := make([]operations.Instance, 0, len(instances))
	for _, instance := range instances {
		live = append(live, instance)
	}
	recorder.Sweep(context.Background(), live)
	if reads.count() != 3 {
		t.Fatalf("only guests that may still be running are read, got %d reads", reads.count())
	}
	for _, state := range []operations.State{operations.StateOnlineIdle, operations.StateAssigned, operations.StateRunning} {
		if _, ok := recorder.Last.get(instances[state].ID); !ok {
			t.Fatalf("no reading kept for %s", state)
		}
	}
	guest.off, guest.hang = true, true
	recorder.Sweep(context.Background(), live)
	if last, ok := recorder.Last.get(instances[operations.StateRunning].ID); !ok || last.receive != 100 {
		t.Fatalf("a failed read must keep the previous reading, got %+v %t", last, ok)
	}
	recorder.Sweep(context.Background(), live[:0])
	if _, ok := recorder.Last.get(instances[operations.StateRunning].ID); ok {
		t.Fatal("a reading of an instance that is no longer live must be dropped")
	}
	(guestTrafficRecorder{Runner: guest}).Sweep(context.Background(), live)
}

type readCounter struct {
	inner *poweringGuest
	mu    sync.Mutex
	n     int
}

func (r *readCounter) Run(ctx context.Context, args ...string) ([]byte, error) {
	r.mu.Lock()
	r.n++
	r.mu.Unlock()
	return r.inner.Run(ctx, args...)
}

func (r *readCounter) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n
}

type liveSource struct {
	live []operations.Instance
	err  error
}

func (s liveSource) LiveInstances(context.Context) ([]operations.Instance, error) {
	return s.live, s.err
}

func TestGuestTrafficSweeperSweepsOnEachTickAndStopsWithItsContext(t *testing.T) {
	guest := &poweringGuest{output: countersOf(7, 1)}
	var warned []string
	recorder := guestTrafficRecorder{Runner: guest, Timeout: time.Second, Store: &trafficStoreFake{},
		Metrics: &trafficMetricsFake{}, Now: time.Now, Last: newGuestReadings(),
		Warn: func(message string, _ ...any) { warned = append(warned, message) }}
	instance := trafficInstance(domain.PlatformLinux)
	instance.State = operations.StateRunning
	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	source := &swappableSource{source: liveSource{live: []operations.Instance{instance}}}
	go func() {
		guestTrafficSweeper{Source: source, Recorder: recorder, Tick: ticks}.Run(ctx)
		close(done)
	}()
	ticks <- time.Now()
	source.set(liveSource{err: errors.New("locked")})
	ticks <- time.Now()
	ticks <- time.Now() // received only once the previous tick's sweep has finished
	cancel()
	<-done
	if last, ok := recorder.Last.get(instance.ID); !ok || last.receive != 7 {
		t.Fatalf("the first tick must have kept a reading, got %+v %t", last, ok)
	}
	if len(warned) != 2 {
		t.Fatalf("an unlistable inventory is warned and skipped, got %v", warned)
	}
}

type swappableSource struct {
	mu     sync.Mutex
	source liveSource
}

func (s *swappableSource) set(source liveSource) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.source = source
}

func (s *swappableSource) LiveInstances(ctx context.Context) ([]operations.Instance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.source.LiveInstances(ctx)
}
