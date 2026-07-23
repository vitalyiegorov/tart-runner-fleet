package telemetry

import (
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newTestHealth(t *testing.T) (*Health, *fakeClock) {
	t.Helper()
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0).UTC()}
	health, err := NewHealth(clock, HealthConfig{
		ReadyTickTTL:           10 * time.Second,
		LiveTickTTL:            time.Minute,
		CriticalObservationTTL: 20 * time.Second,
		Profiles:               []string{"builder", "linux-small"},
		CriticalObservations:   []string{"github", "host", "tart"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return health, clock
}

func TestHealthSeparatesReadinessAndLiveness(t *testing.T) {
	health, clock := newTestHealth(t)
	if !health.Live().OK || health.Ready().OK {
		t.Fatalf("startup live=%+v ready=%+v", health.Live(), health.Ready())
	}
	health.RecordTick(true)
	for _, name := range []string{"github", "host", "tart"} {
		if err := health.RecordObservation(name, ObservationFresh); err != nil {
			t.Fatal(err)
		}
	}
	if !health.Ready().OK {
		t.Fatalf("ready=%+v", health.Ready())
	}

	if err := health.RecordObservation("github", ObservationUnavailable); err != nil {
		t.Fatal(err)
	}
	if health.Ready().OK || !health.Live().OK {
		t.Fatalf("unavailable live=%+v ready=%+v", health.Live(), health.Ready())
	}
	if got := strings.Join(health.Ready().Reasons, ","); got != "critical_observation_unavailable" {
		t.Fatalf("reasons=%q", got)
	}

	if err := health.RecordObservation("github", ObservationFresh); err != nil {
		t.Fatal(err)
	}
	clock.Advance(11 * time.Second)
	if health.Ready().OK || !health.Live().OK {
		t.Fatalf("expired tick live=%+v ready=%+v", health.Live(), health.Ready())
	}
	clock.Advance(50 * time.Second)
	if health.Live().OK {
		t.Fatalf("expired loop live=%+v", health.Live())
	}
}

func TestRecordObservationDetailCarriesBoundedDiagnostic(t *testing.T) {
	health, _ := newTestHealth(t)
	if err := health.RecordObservationDetail("host", ObservationStale, "host stale: host probe is stale"); err != nil {
		t.Fatal(err)
	}
	if got := health.Snapshot().Observations["host"]; got.Freshness != ObservationStale || got.Detail != "host stale: host probe is stale" {
		t.Fatalf("detailed observation = %#v", got)
	}
	// The plain recorder leaves the detail empty rather than retaining a prior one.
	if err := health.RecordObservation("host", ObservationFresh); err != nil {
		t.Fatal(err)
	}
	if got := health.Snapshot().Observations["host"]; got.Detail != "" {
		t.Fatalf("detail not cleared: %#v", got)
	}
	if err := health.RecordObservationDetail("host", ObservationFreshness("bogus"), "x"); err != errInvalidObservation {
		t.Fatalf("invalid freshness err = %v", err)
	}
	if err := health.RecordObservationDetail("missing", ObservationFresh, "x"); err != errUnknownObservation {
		t.Fatalf("unknown observation err = %v", err)
	}
}

func TestHealthDetectsStaleAndExpiredObservations(t *testing.T) {
	health, clock := newTestHealth(t)
	health.RecordTick(true)
	for _, name := range []string{"github", "host", "tart"} {
		if err := health.RecordObservation(name, ObservationFresh); err != nil {
			t.Fatal(err)
		}
	}
	if err := health.RecordObservation("host", ObservationStale); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(health.Ready().Reasons, ","); got != "critical_observation_stale" {
		t.Fatalf("reasons=%q", got)
	}
	if err := health.RecordObservation("host", ObservationFresh); err != nil {
		t.Fatal(err)
	}
	clock.Advance(21 * time.Second)
	result := health.Ready()
	if result.OK || !contains(result.Reasons, "critical_observation_expired") || !contains(result.Reasons, "successful_tick_expired") {
		t.Fatalf("ready=%+v", result)
	}
}

func TestHealthSnapshotMetricsAndDefensiveCopies(t *testing.T) {
	health, clock := newTestHealth(t)
	health.RecordTick(false)
	clock.Advance(time.Second)
	health.RecordTick(true)
	if err := health.SetMode(ModeMixed); err != nil {
		t.Fatal(err)
	}
	if err := health.SetQueue("linux-small", 7, clock.Now().Add(-45*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := health.SetInstances("linux-small", 3, 6, 12_288); err != nil {
		t.Fatal(err)
	}
	health.SetOperations(4, 2)
	if err := health.SetHostPressure(HostPressureMetric{AvailableMemoryMiB: 8192, FreeDiskGiB: 200, SwapUsedMiB: 32,
		SwapOuts: 4, CPUIdlePercent: 60, LoadAverage: 3, AdmissionAllowed: true, AdmissionReason: "capacity available"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"github", "host", "tart"} {
		if err := health.RecordObservation(name, ObservationFresh); err != nil {
			t.Fatal(err)
		}
	}

	snapshot := health.Snapshot()
	snapshot.Queues["linux-small"] = QueueMetrics{}
	snapshot.Instances["linux-small"] = InstanceMetrics{}
	snapshot.Observations["github"] = ObservationMetric{}
	second := health.Snapshot()
	if second.Queues["linux-small"].Count != 7 || second.Instances["linux-small"].MemoryMiB != 12_288 || second.Observations["github"].Freshness != ObservationFresh {
		t.Fatalf("snapshot aliases state: %+v", second)
	}
	if second.LastSuccessfulTick != clock.Now() || second.Mode != ModeMixed || second.OperationRetries != 4 || second.DeadOperations != 2 ||
		second.HostPressure.FreeDiskGiB != 200 || !second.HostPressure.AdmissionAllowed {
		t.Fatalf("snapshot=%+v", second)
	}
}

func TestQueueSLOIsIndependentFromAuthorityReadiness(t *testing.T) {
	health, clock := newTestHealth(t)
	if err := health.SetQueue("builder", 0, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if queue := health.QueueHealth(); !queue.OK {
		t.Fatalf("idle queue health = %+v", queue)
	}
	health.RecordTick(true)
	for _, name := range []string{"github", "host", "tart"} {
		if err := health.RecordObservation(name, ObservationFresh); err != nil {
			t.Fatal(err)
		}
	}
	if err := health.SetQueue("linux-small", 1, clock.Now().Add(-11*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if ready := health.Ready(); !ready.OK {
		t.Fatalf("queue backlog disabled authority readiness = %+v", ready)
	}
	if queue := health.QueueHealth(); queue.OK || !contains(queue.Reasons, "queue_slo_breached") || contains(queue.Reasons, "queue_incident") {
		t.Fatalf("queue SLO = %+v", queue)
	}
	if err := health.SetQueue("linux-small", 1, clock.Now().Add(-31*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if queue := health.QueueHealth(); queue.OK || !contains(queue.Reasons, "queue_incident") {
		t.Fatalf("queue incident = %+v", queue)
	}
}

func TestHealthRejectsInvalidUpdatesWithoutEchoingValues(t *testing.T) {
	health, clock := newTestHealth(t)
	secret := "ghp_do-not-leak"
	cases := []error{
		health.RecordObservation(secret, ObservationFresh),
		health.RecordObservation("github", ObservationFreshness("bogus")),
		health.SetQueue(secret, 1, clock.Now()),
		health.SetQueue("linux-small", -1, clock.Now()),
		health.SetQueue("linux-small", 1, clock.Now().Add(time.Second)),
		health.SetInstances(secret, 1, 1, 1),
		health.SetInstances("linux-small", -1, 1, 1),
		health.SetOperations(-1, 0),
		health.SetHostPressure(HostPressureMetric{AvailableMemoryMiB: -1}),
		health.SetHostPressure(HostPressureMetric{AdmissionReason: secret}),
		health.SetMode(Mode(secret)),
	}
	for _, err := range cases {
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Fatalf("unsafe error: %v", err)
		}
	}
}

func TestHealthConcurrentReadersAndWriters(t *testing.T) {
	health, clock := newTestHealth(t)
	var wg sync.WaitGroup
	for worker := 0; worker < 24; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 250; i++ {
				health.RecordTick(i%2 == 0)
				_ = health.RecordObservation("github", ObservationFresh)
				_ = health.SetQueue("linux-small", i, clock.Now())
				_ = health.SetInstances("linux-small", i, i*2, i*1024)
				health.SetOperations(i, i/2)
				_ = health.SetMode(ModeLinux)
				_ = health.Snapshot()
				_ = health.Live()
				_ = health.Ready()
			}
		}()
	}
	wg.Wait()
}

func TestNewHealthValidationAndDefaults(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1, 0)}
	if _, err := NewHealth(nil, HealthConfig{}); err == nil {
		t.Fatal("nil clock accepted")
	}
	invalid := []HealthConfig{
		{ReadyTickTTL: -1},
		{ReadyTickTTL: time.Minute, LiveTickTTL: time.Second},
		{CriticalObservationTTL: -1},
		{QueueSLO: -1},
		{QueueIncidentSLO: -1},
		{QueueSLO: time.Hour, QueueIncidentSLO: time.Minute},
		{Profiles: []string{"x", "x"}},
		{CriticalObservations: []string{"x", "x"}},
		{Profiles: []string{""}},
	}
	for _, cfg := range invalid {
		if _, err := NewHealth(clock, cfg); err == nil {
			t.Fatalf("accepted %+v", cfg)
		}
	}
	health, err := NewHealth(clock, HealthConfig{})
	if err != nil || !health.Live().OK || health.Ready().OK {
		t.Fatalf("default health=%v err=%v", health, err)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
