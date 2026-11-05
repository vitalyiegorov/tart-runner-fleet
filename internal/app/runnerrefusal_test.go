package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

var refusalNow = time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)

func refusalDemand(id int64, platform domain.Platform) domain.Demand {
	return domain.Demand{Key: domain.DemandKey{Repo: "o/r", RunID: 1, Attempt: 1, JobID: id}, Platform: platform}
}

func TestOnlyAJobStartedAfterTheVerdictClearsIt(t *testing.T) {
	refusals := &RunnerRefusals{Now: func() time.Time { return refusalNow }}
	refusals.Record(domain.PlatformLinux, "2.336.0", "trf-a")
	refusals.Record(domain.PlatformLinux, "2.336.0", "trf-b")
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

func TestAVerdictNeedsTwoGuestsWithinTheWindowAndAgreeing(t *testing.T) {
	clock := refusalNow
	refusals := &RunnerRefusals{Now: func() time.Time { return clock }}
	refusals.Record(domain.PlatformLinux, "2.336.0", "trf-a")
	refusals.Record(domain.PlatformLinux, "2.335.0", "trf-b")
	if len(refusals.Observe(nil)) != 0 {
		t.Fatal("two guests naming different versions are not agreeing")
	}
	clock = clock.Add(domain.RunnerRefusalWitnessWindow + time.Second)
	refusals.Record(domain.PlatformLinux, "2.335.0", "trf-c")
	if len(refusals.Observe(nil)) != 0 {
		t.Fatal("a witness past the window is not the same event")
	}
	refusals.Record(domain.PlatformMacOS, "2.335.0", "trf-d")
	if len(refusals.Observe(nil)) != 0 {
		t.Fatal("a witness on another platform says nothing about this image")
	}
	refusals.Record(domain.PlatformLinux, "2.335.0", "trf-e")
	standing := refusals.Observe(nil)
	if len(standing) != 1 || standing[domain.PlatformLinux].Instance != "trf-e" {
		t.Fatalf("got %+v", standing)
	}
	clock = clock.Add(time.Minute)
	refusals.Record(domain.PlatformLinux, "2.335.0", "trf-f")
	if at := refusals.Observe(nil)[domain.PlatformLinux].At; !at.Equal(clock) {
		t.Fatalf("one probe renews a standing verdict: %s", at)
	}
}

func TestAStartedJobClearsALoneWitness(t *testing.T) {
	refusals := &RunnerRefusals{Now: func() time.Time { return refusalNow }}
	refusals.Record(domain.PlatformLinux, "2.336.0", "trf-a")
	refusals.Observe([]domain.Instance{{Platform: domain.PlatformLinux, State: domain.InstanceRunning, RunningSince: refusalNow.Add(time.Minute)}})
	refusals.Record(domain.PlatformLinux, "2.336.0", "trf-b")
	if len(refusals.Observe(nil)) != 0 {
		t.Fatal("a job that started after the first reading disproved it")
	}
}

type jobEvidenceFake struct {
	took bool
	err  error
}

func (f jobEvidenceFake) InstanceTookJob(context.Context, string) (bool, error) { return f.took, f.err }

const refusalLine = "An error occurred: Runner version v2.336.0 is deprecated and cannot receive messages.\n"

// A guest that ever took a job is not even asked: its log is the job's to write.
func TestAGuestThatTookAJobIsNotAskedAndCountsForNothing(t *testing.T) {
	for name, test := range map[string]struct {
		evidence jobEvidenceFake
		output   string
		err      error
		reads    int
		witness  bool
	}{
		"took a job":          {evidence: jobEvidenceFake{took: true}, output: refusalLine},
		"evidence unread":     {evidence: jobEvidenceFake{err: errors.New("locked")}, output: refusalLine},
		"never took one":      {output: refusalLine, reads: 1, witness: true},
		"no such line":        {err: errors.New("exit status 1"), reads: 1},
		"says something else": {output: "An error occurred: something else\n", reads: 1},
	} {
		t.Run(name, func(t *testing.T) {
			refusals := &RunnerRefusals{Now: func() time.Time { return refusalNow }}
			reads := 0
			reader := RunnerRefusalReader{Evidence: test.evidence, Refusals: refusals,
				Read: func(context.Context, string) ([]byte, error) { reads++; return []byte(test.output), test.err }}
			instance := operations.Instance{ID: "trf-a", Platform: domain.PlatformLinux}
			reader.ReadRunnerRefusal(context.Background(), instance)
			if reads != test.reads {
				t.Fatalf("reads = %d, want %d", reads, test.reads)
			}
			instance.ID = "trf-b"
			reader.ReadRunnerRefusal(context.Background(), instance)
			if verdict := len(refusals.Observe(nil)) == 1; verdict != test.witness {
				t.Fatalf("verdict = %v, want %v", verdict, test.witness)
			}
		})
	}
}

func TestANilRefusalTrackerHoldsNothing(t *testing.T) {
	var refusals *RunnerRefusals
	refusals.Record(domain.PlatformLinux, "2.336.0", "trf-a")
	refusals.Record(domain.PlatformLinux, "2.336.0", "trf-b")
	if refusals.Observe(nil) != nil {
		t.Fatal("a node that reads no guest has no verdict")
	}
}

func TestARefusedImageWithholdsItsWholeQueueAndOffersItOnceDue(t *testing.T) {
	refused := map[domain.Platform]domain.RunnerRefusal{domain.PlatformLinux: {Version: "2.336.0", At: refusalNow}}
	queue := []domain.Demand{refusalDemand(1, domain.PlatformLinux), refusalDemand(2, domain.PlatformLinux), refusalDemand(3, domain.PlatformMacOS)}
	due := refusalNow.Add(domain.RunnerRefusalProbeInterval)

	withheld := map[domain.DemandKey]bool{}
	if probes := withholdRefused(queue, refused, map[domain.Platform]bool{}, withheld, refusalNow.Add(time.Minute)); len(probes) != 0 {
		t.Fatalf("inside the interval nothing probes: %+v", probes)
	}
	if !withheld[queue[0].Key] || !withheld[queue[1].Key] || withheld[queue[2].Key] {
		t.Fatalf("the whole Linux queue waits and only it: %v", withheld)
	}
	withheld = map[domain.DemandKey]bool{}
	if probes := withholdRefused(queue, refused, map[domain.Platform]bool{}, withheld, due); len(probes) != 2 {
		t.Fatalf("once due every Linux demand is a candidate: %+v", probes)
	}
	withheld = map[domain.DemandKey]bool{}
	if probes := withholdRefused(queue, refused, map[domain.Platform]bool{domain.PlatformLinux: true}, withheld, due); len(probes) != 0 {
		t.Fatalf("a guest already answering needs no second probe: %+v", probes)
	}
	withheld = map[domain.DemandKey]bool{queue[0].Key: true}
	if probes := withholdRefused(queue, refused, map[domain.Platform]bool{}, withheld, due); len(probes) != 1 || probes[0].Key != queue[1].Key {
		t.Fatalf("a demand already withheld is no candidate: %+v", probes)
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
