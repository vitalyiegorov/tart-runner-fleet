package daemon

import (
	"context"
	"errors"
	"strings"
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
	if recorder := (dependencies{}).guestTraffic(&trafficStoreFake{}, &trafficMetricsFake{}, cfg, nil); recorder != nil {
		t.Fatalf("no exec runner must wire no recorder, got %#v", recorder)
	}
	podmanless := linuxPlatform()
	if recorder := (dependencies{guestExec: podmanless.guestExec}).guestTraffic(&trafficStoreFake{},
		&trafficMetricsFake{}, cfg, nil); recorder != nil {
		t.Fatalf("a Linux node with no container backend has no guest: %#v", recorder)
	}
	cfg.Executor.Backend = config.ExecutorPodman
	if recorder := (dependencies{guestExec: podmanless.guestExec, now: time.Now}).guestTraffic(&trafficStoreFake{},
		&trafficMetricsFake{}, cfg, nil); recorder == nil {
		t.Fatal("a podman node samples its containers")
	}
	if recorder := (dependencies{guestExec: applePlatform().guestExec, now: time.Now}).guestTraffic(&trafficStoreFake{},
		&trafficMetricsFake{}, cfg, nil); recorder == nil {
		t.Fatal("a Tart node samples its VMs")
	}
}
