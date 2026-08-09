package daemon

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/telemetry"
)

// occupancyClock is the wall clock of the 2026-08-09 incident (issue #223), when
// trf-xl-05bbe1c83f21fcd6 had held 6 CPU and 12288 MB for seventy-five minutes
// against a forty-five minute ceiling with five jobs queued behind it.
var occupancyClock = time.Date(2026, 8, 9, 19, 22, 50, 0, time.UTC)

// incidentJob is the job the fleet would cut: the `if: always()` cleanup step
// waiting on an emulator that never booted.
var incidentJob = domain.DemandKey{Repo: "rnw-community/rnw-community", RunID: 31_325_708_527,
	Attempt: 1, JobID: 93_275_690_093}

func heldVector(age time.Duration, warned, overBudget bool) scheduler.Occupancy {
	return scheduler.Occupancy{Instance: "trf-xl-05bbe1c83f21fcd6", Profile: "xl",
		Repo: incidentJob.Repo, Demand: incidentJob,
		Resources: domain.Resources{CPU: 6, MemoryMB: 12_288, Slots: 1},
		Age:       age, Budget: 45 * time.Minute, Warned: warned, OverBudget: overBudget,
		StarvesQueuedDemand: true}
}

func occupancyReporter() (*failureReporter, *bytes.Buffer) {
	logged := &bytes.Buffer{}
	return newFailureReporter(logged, func() time.Time { return occupancyClock }), logged
}

// TestReportOccupancySaysItWhileItIsHappening is the operability half of ADR
// 0036. The incident was found by an owner asking why a release was slow, three
// quarters of an hour after the fleet could first have said so; the warning has
// to carry the vector, the duration, the ceiling and the job, so an operator
// tells a reaped job apart from a flake without opening the log a second time.
func TestReportOccupancySaysItWhileItIsHappening(t *testing.T) {
	reporter, logged := occupancyReporter()

	reporter.reportOccupancy([]scheduler.Occupancy{heldVector(75*time.Minute, true, true)})

	line := logged.String()
	for _, fragment := range []string{"instance occupancy budget exceeded", "instance=trf-xl-05bbe1c83f21fcd6",
		"profile=xl", "repo=rnw-community/rnw-community", "cpu=6", "memoryMb=12288", "held=1h15m0s",
		"budget=45m0s", "runId=31325708527", "jobId=93275690093", "queuedDemandFits=true"} {
		if !strings.Contains(line, fragment) {
			t.Fatalf("occupancy warning missing %q: %q", fragment, line)
		}
	}
}

// TestReportOccupancyStaysQuietInsideTheWarningFraction proves an ordinary long
// job does not cry wolf. Until a hold crosses three quarters of its budget there
// is nothing to say, and a fleet that said it on every tick would train
// operators to ignore the line that matters.
func TestReportOccupancyStaysQuietInsideTheWarningFraction(t *testing.T) {
	reporter, logged := occupancyReporter()

	reporter.reportOccupancy([]scheduler.Occupancy{heldVector(20*time.Minute, false, false)})

	if logged.Len() != 0 {
		t.Fatalf("a hold well inside its budget was reported: %q", logged.String())
	}
}

// TestReportOccupancyEscalatesButDoesNotRepeat pins both halves of the rate
// limit. Keyed per instance AND per state, a hold that crosses the warning
// fraction and later the ceiling produces two distinct lines rather than one per
// tick for an hour — and the escalation is never suppressed by the warning that
// preceded it.
func TestReportOccupancyEscalatesButDoesNotRepeat(t *testing.T) {
	reporter, logged := occupancyReporter()
	approaching := heldVector(40*time.Minute, true, false)

	reporter.reportOccupancy([]scheduler.Occupancy{approaching})
	reporter.reportOccupancy([]scheduler.Occupancy{approaching}) // the next tick, inside the window
	if got := strings.Count(logged.String(), "instance occupancy budget approaching"); got != 1 {
		t.Fatalf("in-window approaching lines = %d, want the rate limit to hold at 1: %q", got, logged.String())
	}

	exceeded := heldVector(75*time.Minute, true, true)
	reporter.reportOccupancy([]scheduler.Occupancy{exceeded})
	reporter.reportOccupancy([]scheduler.Occupancy{exceeded})
	if got := strings.Count(logged.String(), "instance occupancy budget exceeded"); got != 1 {
		t.Fatalf("in-window exceeded lines = %d: %q", got, logged.String())
	}
	// Two distinct lines, not one: the escalation is a new fact about the same
	// instance and the warning must not have swallowed it.
	if got := strings.Count(logged.String(), "instance occupancy budget"); got != 2 {
		t.Fatalf("total occupancy lines = %d, want the warning and the escalation: %q", got, logged.String())
	}
	// A second instance is its own key, so one noisy hold cannot silence another.
	neighbour := exceeded
	neighbour.Instance = "trf-xl-9c4d0011"
	reporter.reportOccupancy([]scheduler.Occupancy{neighbour})
	if !strings.Contains(logged.String(), "instance=trf-xl-9c4d0011") {
		t.Fatalf("a second instance was suppressed by the first: %q", logged.String())
	}
}

// TestReportOccupancyReclaimNamesTheJobItCuts exists because a reaped job fails
// on GitHub with a lost-communication error, which is indistinguishable from a
// flake. Something has to say which job was cut, on whose behalf, and how long
// it had held the host — and it is never rate limited, because each reclaim is a
// distinct destructive decision and suppressing the second would hide it.
func TestReportOccupancyReclaimNamesTheJobItCuts(t *testing.T) {
	reporter, logged := occupancyReporter()
	held := heldVector(75*time.Minute, true, true)
	reclaim := scheduler.Operation{Kind: scheduler.OperationDrain, Instance: held.Instance, Profile: held.Profile,
		Demand: incidentJob, Recovery: true, OccupancyExceeded: true}

	reporter.reportOccupancyReclaim(reclaim, []scheduler.Occupancy{held})

	line := logged.String()
	for _, fragment := range []string{"instance reclaimed for exceeding its occupancy budget",
		"instance=trf-xl-05bbe1c83f21fcd6", "profile=xl", "repo=rnw-community/rnw-community",
		"runId=31325708527", "jobId=93275690093", "attempt=1", "held=1h15m0s", "budget=45m0s",
		"lost-communication failure on GitHub"} {
		if !strings.Contains(line, fragment) {
			t.Fatalf("reclaim line missing %q: %q", fragment, line)
		}
	}
	reporter.reportOccupancyReclaim(reclaim, []scheduler.Occupancy{held})
	if got := strings.Count(logged.String(), "instance reclaimed for exceeding"); got != 2 {
		t.Fatalf("reclaim lines = %d, want every destructive decision on the record", got)
	}
}

// TestReportOccupancyReclaimIgnoresEveryOtherReclaim keeps the line honest. A
// drain for a lingering runner or a stalled assignment has a different premise
// and its own reporting; announcing it as an occupancy reap would attribute a
// cut job to a budget that never fired.
func TestReportOccupancyReclaimIgnoresEveryOtherReclaim(t *testing.T) {
	reporter, logged := occupancyReporter()
	held := heldVector(75*time.Minute, true, true)

	for _, operation := range []scheduler.Operation{
		{Kind: scheduler.OperationDrain, Instance: held.Instance, Demand: incidentJob, Recovery: true, LingeringRunner: true},
		{Kind: scheduler.OperationDrain, Instance: held.Instance, Demand: incidentJob, Recovery: true, StalledAssignment: true},
		{Kind: scheduler.OperationSpawn, Instance: held.Instance, Demand: incidentJob},
	} {
		reporter.reportOccupancyReclaim(operation, []scheduler.Occupancy{held})
	}
	if logged.Len() != 0 {
		t.Fatalf("a reclaim with another premise was reported as an occupancy reap: %q", logged.String())
	}
}

// TestReportOccupancyReclaimOfAnInstanceItCannotMeasure proves the line survives
// a hold the projection no longer carries — a VM that released between the plan
// and the report. Zero is published as zero rather than the line being dropped:
// the destructive decision is the fact worth recording, and the durations are
// supporting evidence.
func TestReportOccupancyReclaimOfAnInstanceItCannotMeasure(t *testing.T) {
	reporter, logged := occupancyReporter()
	reclaim := scheduler.Operation{Kind: scheduler.OperationDrain, Instance: "trf-xl-05bbe1c83f21fcd6",
		Profile: "xl", Demand: incidentJob, Recovery: true, OccupancyExceeded: true}

	reporter.reportOccupancyReclaim(reclaim, nil)

	if !strings.Contains(logged.String(), "held=0s") || !strings.Contains(logged.String(), "jobId=93275690093") {
		t.Fatalf("an unmeasurable hold dropped the reclaim record: %q", logged.String())
	}
}

// occupancyTicker builds the engine ticker over the incident's profile so the
// metric, the warning and the reap all read the same pure projection.
func occupancyTicker(t *testing.T, reporter *failureReporter) (engineTicker, *telemetry.Health) {
	t.Helper()
	health, err := telemetry.NewHealth(wallClock{}, telemetry.HealthConfig{Profiles: []string{"xl"},
		CriticalObservations: []string{"scheduler"}})
	if err != nil {
		t.Fatal(err)
	}
	profile := domain.Profile{ID: "xl", Platform: domain.PlatformLinux, Route: "tiered",
		Resources: domain.Resources{CPU: 6, MemoryMB: 12_288, Slots: 1}, OccupancyBudget: 45 * time.Minute}
	engine := app.Engine{Config: scheduler.Config{Profiles: map[domain.ProfileID]domain.Profile{"xl": profile}}}
	return engineTicker{engine: engine, health: health, reporter: reporter}, health
}

func occupancyTickResult(reclaimed bool) app.TickResult {
	held := domain.Instance{ID: "trf-xl-05bbe1c83f21fcd6", Repo: incidentJob.Repo, Demand: incidentJob,
		Platform: domain.PlatformLinux, Profile: "xl", Route: "tiered",
		Resources: domain.Resources{CPU: 6, MemoryMB: 12_288, Slots: 1},
		State:     domain.InstanceRunning, Power: domain.InstancePowerRunning,
		RunningSince: occupancyClock.Add(-75 * time.Minute), OccupiedSince: occupancyClock.Add(-75 * time.Minute)}
	// The starved queue: work whose vector fits inside the one being held.
	queued := []domain.Demand{{Key: domain.DemandKey{Repo: "sudoku-repo/builder", RunID: 7, Attempt: 1, JobID: 9},
		Profile: "xl", Platform: domain.PlatformLinux, Route: "tiered", CreatedAt: occupancyClock.Add(-80 * time.Minute)}}
	result := app.TickResult{At: occupancyClock, Instances: []domain.Instance{held}, Demands: queued}
	if reclaimed {
		result.Plan = scheduler.Plan{Operations: []scheduler.Operation{{Kind: scheduler.OperationDrain,
			Instance: held.ID, Profile: "xl", Demand: incidentJob, Recovery: true, OccupancyExceeded: true}}}
	}
	return result
}

// TestRecordOccupancyPublishesAndSpeaks proves the tick fans the one projection
// out to both surfaces: the metric an alert scrapes and the log line an operator
// reads, plus the record of the job the plan is about to cut. They come from the
// same scheduler.Occupancies call as the reap itself, so they cannot disagree
// about a hold.
func TestRecordOccupancyPublishesAndSpeaks(t *testing.T) {
	reporter, logged := occupancyReporter()
	ticker, health := occupancyTicker(t, reporter)

	ticker.recordOccupancy(occupancyTickResult(true))

	published := health.Snapshot().Occupancy
	if len(published) != 1 {
		t.Fatalf("published occupancy = %#v, want the held vector", published)
	}
	want := telemetry.OccupancyMetric{Instance: "trf-xl-05bbe1c83f21fcd6", Profile: "xl", Repo: incidentJob.Repo,
		CPU: 6, MemoryMiB: 12_288, Age: 75 * time.Minute, Budget: 45 * time.Minute,
		Warned: true, OverBudget: true, StarvesQueuedDemand: true}
	if published[0] != want {
		t.Fatalf("published metric = %#v, want %#v", published[0], want)
	}
	if health.Occupancy().OK {
		t.Fatalf("the starved vector did not reach the health verdict: %#v", health.Occupancy())
	}
	for _, fragment := range []string{"instance occupancy budget exceeded",
		"instance reclaimed for exceeding its occupancy budget"} {
		if !strings.Contains(logged.String(), fragment) {
			t.Fatalf("tick log missing %q: %q", fragment, logged.String())
		}
	}
}

// TestRecordOccupancyWithoutAReporterStillPublishes keeps the warning optional
// and the metric mandatory. An observe-mode harness has no reporter, and it must
// still measure every hold rather than becoming a fleet that sees nothing.
func TestRecordOccupancyWithoutAReporterStillPublishes(t *testing.T) {
	ticker, health := occupancyTicker(t, nil)

	ticker.recordOccupancy(occupancyTickResult(true))

	if published := health.Snapshot().Occupancy; len(published) != 1 || !published[0].OverBudget {
		t.Fatalf("a reporterless tick published %#v", published)
	}
}

// TestRecordOccupancyOfAFleetHoldingNothing proves the quiet path stays quiet:
// no rows, no warning, and a health verdict that is OK because nothing is held
// rather than because nothing was measured.
func TestRecordOccupancyOfAFleetHoldingNothing(t *testing.T) {
	reporter, logged := occupancyReporter()
	ticker, health := occupancyTicker(t, reporter)

	ticker.recordOccupancy(app.TickResult{At: occupancyClock})

	if published := health.Snapshot().Occupancy; len(published) != 0 {
		t.Fatalf("an empty fleet published %#v", published)
	}
	if !health.Occupancy().OK || logged.Len() != 0 {
		t.Fatalf("an empty fleet reported an incident: %#v %q", health.Occupancy(), logged.String())
	}
}
