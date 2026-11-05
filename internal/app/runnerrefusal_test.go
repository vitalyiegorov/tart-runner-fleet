package app

import (
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

var refusalNow = time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)

func refusalDemand(id int64, platform domain.Platform) domain.Demand {
	return domain.Demand{Key: domain.DemandKey{Repo: "o/r", RunID: 1, Attempt: 1, JobID: id}, Platform: platform}
}

func TestOnlyAJobStartedAfterTheVerdictClearsIt(t *testing.T) {
	refusals := &RunnerRefusals{Now: func() time.Time { return refusalNow }}
	refusals.Record(domain.PlatformLinux, "2.336.0", "trf-a")
	already := domain.Instance{Platform: domain.PlatformLinux, State: domain.InstanceRunning, RunningSince: refusalNow.Add(-time.Minute)}
	if standing := refusals.Observe([]domain.Instance{already}); len(standing) != 1 {
		t.Fatal("a job running since before the cutoff says nothing about the image today")
	}
	other := domain.Instance{Platform: domain.PlatformMacOS, State: domain.InstanceRunning, RunningSince: refusalNow.Add(time.Minute)}
	if standing := refusals.Observe([]domain.Instance{other}); len(standing) != 1 {
		t.Fatal("a macOS job says nothing about the Linux image")
	}
	started := domain.Instance{Platform: domain.PlatformLinux, State: domain.InstanceRunning, RunningSince: refusalNow.Add(time.Minute)}
	if standing := refusals.Observe([]domain.Instance{started}); len(standing) != 0 {
		t.Fatal("a job that started after the verdict disproves it")
	}
}

func TestANilRefusalTrackerHoldsNothing(t *testing.T) {
	var refusals *RunnerRefusals
	refusals.Record(domain.PlatformLinux, "2.336.0", "trf-a")
	if refusals.Observe(nil) != nil {
		t.Fatal("a node that reads no guest has no verdict")
	}
}

func TestARefusedImageWithholdsItsQueueAndProbesOnceAfterTheInterval(t *testing.T) {
	refused := map[domain.Platform]domain.RunnerRefusal{domain.PlatformLinux: {Version: "2.336.0", At: refusalNow}}
	queue := []domain.Demand{refusalDemand(1, domain.PlatformLinux), refusalDemand(2, domain.PlatformLinux), refusalDemand(3, domain.PlatformMacOS)}

	withheld := map[domain.DemandKey]bool{}
	withholdRefused(queue, refused, map[domain.Platform]bool{}, withheld, refusalNow.Add(time.Minute))
	if !withheld[queue[0].Key] || !withheld[queue[1].Key] || withheld[queue[2].Key] {
		t.Fatalf("inside the interval the whole Linux queue waits and only it: %v", withheld)
	}

	withheld = map[domain.DemandKey]bool{}
	withholdRefused(queue, refused, map[domain.Platform]bool{}, withheld, refusalNow.Add(domain.RunnerRefusalProbeInterval))
	if withheld[queue[0].Key] || !withheld[queue[1].Key] {
		t.Fatalf("after the interval exactly one demand probes: %v", withheld)
	}

	withheld = map[domain.DemandKey]bool{}
	withholdRefused(queue, refused, map[domain.Platform]bool{domain.PlatformLinux: true}, withheld, refusalNow.Add(domain.RunnerRefusalProbeInterval))
	if !withheld[queue[0].Key] || !withheld[queue[1].Key] {
		t.Fatalf("a guest already answering the question needs no second probe: %v", withheld)
	}
}

func TestOnlyALiveGuestWithoutAJobCountsAsAProbe(t *testing.T) {
	key := domain.DemandKey{Repo: "o/r", RunID: 1, Attempt: 1, JobID: 1}
	probing := probeInFlight([]domain.Instance{
		{Demand: key, Platform: domain.PlatformLinux, State: domain.InstanceAssigned},
		{Demand: key, Platform: domain.PlatformMacOS, State: domain.InstanceRunning},
	})
	if !probing[domain.PlatformLinux] || probing[domain.PlatformMacOS] {
		t.Fatalf("got %v", probing)
	}
}
