package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/lifecycle"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

type fixedHost map[string]domain.GuestNetwork

func (h fixedHost) Read(context.Context, []string) (map[string]domain.GuestNetwork, error) {
	return h, nil
}

func TestGuestTrafficIsReadFromTheHostOnTartAndByExecElsewhere(t *testing.T) {
	cfg := config.Default()
	store, metrics := &trafficStoreFake{}, &trafficMetricsFake{}

	tartNode := dependencies{guestExec: applePlatform().guestExec, hostTraffic: applePlatform().hostTraffic, now: time.Now}
	recorder, sweeper := tartNode.guestTraffic(store, metrics, cfg, nil)
	if recorder == nil || sweeper == nil {
		t.Fatal("a Tart node records from host counters and sweeps them")
	}
	if _, ok := recorder.(*lifecycle.HostTrafficRecorder); !ok {
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

// TestAWiredHostRecorderStoresTheReadingOnTheDrainRetry replays a store write
// that fails while the drain is recording: the retry, with the guest already
// gone, must store the measured reading through the wired store and metrics, and
// a nil warn must not matter.
func TestAWiredHostRecorderStoresTheReadingOnTheDrainRetry(t *testing.T) {
	instance := trafficInstance(domain.PlatformLinux)
	store, metrics := &trafficStoreFake{changed: true}, &trafficMetricsFake{}
	node := dependencies{hostTraffic: func(config.Config) lifecycle.HostCounters {
		return fixedHost{instance.ID: {ReceiveBytes: 900, TransmitBytes: 40}}
	}, now: time.Now}
	recorder, sweeper := node.guestTraffic(store, metrics, config.Default(), nil)
	sweeper.Sweep(context.Background(), []operations.Instance{instance})

	store.err = errors.New("disk full")
	recorder.RecordGuestTraffic(context.Background(), instance)
	store.err = nil
	recorder.RecordGuestTraffic(context.Background(), instance)

	last := store.samples[len(store.samples)-1]
	if len(store.samples) != 2 || !last.Measured || last.ReceiveBytes != 900 || last.TransmitBytes != 40 {
		t.Fatalf("samples = %+v, want the retry to store the measured reading", store.samples)
	}
	if len(metrics.metrics) != 1 || !metrics.metrics[0].Measured {
		t.Fatalf("metrics = %+v, want the one counted sample to be measured", metrics.metrics)
	}
}
