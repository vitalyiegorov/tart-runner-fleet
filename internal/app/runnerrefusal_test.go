package app

import (
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

var refusalNow = time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)

func TestOnlyAJobStartedAfterTheReadingClearsIt(t *testing.T) {
	refusals := &RunnerRefusals{Now: func() time.Time { return refusalNow }}
	refusals.Record(domain.PlatformLinux, "2.336.0", "trf-a")
	already := domain.Instance{Platform: domain.PlatformLinux, State: domain.InstanceRunning, RunningSince: refusalNow.Add(-time.Minute)}
	if standing := refusals.Observe([]domain.Instance{already}); len(standing) != 1 {
		t.Fatal("a job running since before the reading says nothing about it")
	}
	other := domain.Instance{Platform: domain.PlatformMacOS, State: domain.InstanceRunning, RunningSince: refusalNow.Add(time.Minute)}
	if standing := refusals.Observe([]domain.Instance{other}); len(standing) != 1 {
		t.Fatal("a macOS job says nothing about the Linux image")
	}
	started := domain.Instance{Platform: domain.PlatformLinux, State: domain.InstanceRunning, RunningSince: refusalNow.Add(time.Minute)}
	if standing := refusals.Observe([]domain.Instance{started}); len(standing) != 0 {
		t.Fatal("a job that started after the reading contradicts it")
	}
}

func TestANilRefusalTrackerReadsNothing(t *testing.T) {
	var refusals *RunnerRefusals
	refusals.Record(domain.PlatformLinux, "2.336.0", "trf-a")
	if refusals.Observe(nil) != nil {
		t.Fatal("a node that reads no guest has no reading")
	}
}
