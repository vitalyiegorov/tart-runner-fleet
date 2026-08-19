package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

// glitched is the instance of issue #246: Running, executing a job, and reported
// by the backend as powered off. PowerRetracted is the durable trace of its own
// drain having contradicted that reading and sent it back to Running.
func glitched(retracted bool) domain.Instance {
	return domain.Instance{ID: "trf-large-f7d1fac9141ad580", Repo: lostJob.Repo, Demand: lostJob,
		Platform: domain.PlatformLinux, Profile: "large", Route: "tiered",
		Resources: domain.Resources{CPU: 4, MemoryMB: 8_192, Slots: 1},
		State:     domain.InstanceRunning, Power: domain.InstancePowerStopped,
		PowerRun:       domain.ObservationRun{Refusals: 3, RefusedSince: deathClock.Add(-time.Minute), LastProbe: deathClock},
		PowerRetracted: retracted}
}

// unreadable is the 2026-08-18/19 instance as the fleet now sees it: the backend
// could not read its power, so nothing is planned and the reason is named.
func unreadable(reason domain.PowerReadReason) domain.Instance {
	instance := glitched(false)
	instance.Power = domain.InstancePowerUnknown
	instance.PowerUnreadable = domain.PowerReadFailure{Reason: reason, Latency: 1234 * time.Millisecond}
	return instance
}

func recoveryDrain(mutate func(*scheduler.Operation)) scheduler.Operation {
	operation := scheduler.Operation{Kind: scheduler.OperationDrain, Instance: "trf-large-f7d1fac9141ad580",
		Profile: "large", Recovery: true}
	if mutate != nil {
		mutate(&operation)
	}
	return operation
}

// The gap this closes, stated exactly: of six recovery causes only two have ever
// produced a log line, so two hundred and one destructive decisions across two
// nights had to be attributed by re-deriving content-addressed operation
// identities out of the durable ledger. Every cause names itself now.
func TestEveryRecoveryCauseNamesItself(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*scheduler.Operation)
		cause  string
	}{
		{name: "stopped recovery", cause: "vm powered off"},
		{name: "confirmed inactive", cause: "runner confirmed inactive",
			mutate: func(o *scheduler.Operation) { o.ConfirmedInactive = true }},
		{name: "stalled assignment", cause: "assignment never started",
			mutate: func(o *scheduler.Operation) { o.StalledAssignment = true }},
		{name: "lingering runner", cause: "runner idle past its deadline",
			mutate: func(o *scheduler.Operation) { o.LingeringRunner = true }},
		{name: "guest unresponsive", cause: "guest stopped answering",
			mutate: func(o *scheduler.Operation) { o.GuestUnresponsive = true }},
		{name: "occupancy budget", cause: "occupancy budget exceeded",
			mutate: func(o *scheduler.Operation) { o.OccupancyExceeded = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			reporter, logged := silenceReporter()
			reporter.reportRecovery(recoveryDrain(test.mutate))
			for _, fragment := range []string{"instance recovery drain planned",
				"instance=trf-large-f7d1fac9141ad580", "cause=\"" + test.cause + "\""} {
				if !strings.Contains(logged.String(), fragment) {
					t.Fatalf("recovery log missing %q: %q", fragment, logged.String())
				}
			}
		})
	}
}

// An ordinary drain is not a recovery and says nothing here. The completion path
// is the fleet working, and a line per finished job would bury the six that mean
// the fleet is destroying something.
func TestAnOrdinaryDrainIsNotReportedAsARecovery(t *testing.T) {
	reporter, logged := silenceReporter()
	reporter.reportRecovery(recoveryDrain(func(o *scheduler.Operation) { o.Recovery = false }))
	if logged.Len() != 0 {
		t.Fatalf("an event drain logged a recovery: %q", logged.String())
	}
}

// A storm is never suppressed. Eighty-six identical decisions in nine minutes is
// exactly the artifact an operator needs, and the eighty-sixth line is the one
// that names the problem.
func TestARecoveryStormIsNeverRateLimited(t *testing.T) {
	reporter, logged := silenceReporter()
	for range 5 {
		reporter.reportRecovery(recoveryDrain(nil))
	}
	if lines := strings.Count(logged.String(), "instance recovery drain planned"); lines != 5 {
		t.Fatalf("a storm of five recoveries produced %d lines", lines)
	}
}

// The abort had never been recorded anywhere, and it is the most interesting
// event in the ladder: the fleet catching itself about to destroy a live runner.
func TestARetractedPremiseIsNamedOnceAndCarriesTheNewBound(t *testing.T) {
	reporter, logged := silenceReporter()

	reporter.reportRetractedPremise([]domain.Instance{glitched(true)})
	reporter.reportRetractedPremise([]domain.Instance{glitched(true)})

	for _, fragment := range []string{"instance power premise retracted by its own drain",
		"instance=trf-large-f7d1fac9141ad580", "stoppedReadings=3", "requiredWindow=6m0s"} {
		if !strings.Contains(logged.String(), fragment) {
			t.Fatalf("retraction log missing %q: %q", fragment, logged.String())
		}
	}
	if lines := strings.Count(logged.String(), "power premise retracted"); lines != 1 {
		t.Fatalf("the retraction persists on the row for the instance's life and must be "+
			"reported once, not per tick; got %d lines", lines)
	}
}

// Only a retracted, live instance is reported. A healthy one has nothing to say,
// and a torn-down one is no longer a fact about the fleet.
func TestOnlyALiveRetractedInstanceIsReported(t *testing.T) {
	dead := glitched(true)
	dead.State = domain.InstanceDeleted
	for _, instance := range []domain.Instance{glitched(false), dead} {
		reporter, logged := silenceReporter()
		reporter.reportRetractedPremise([]domain.Instance{instance})
		if logged.Len() != 0 {
			t.Fatalf("instance %#v was reported: %q", instance, logged.String())
		}
	}
}

// The tick path says both halves, and a reporterless observe-mode ticker says
// neither without failing.
func TestRecordRecoveriesSpeaksAndToleratesNoReporter(t *testing.T) {
	reporter, logged := silenceReporter()
	ticker, _ := silenceTicker(t, reporter)
	result := app.TickResult{At: deathClock, Instances: []domain.Instance{glitched(true)},
		Plan: scheduler.Plan{Operations: []scheduler.Operation{recoveryDrain(nil)}}}

	ticker.recordRecoveries(result)

	for _, fragment := range []string{"instance recovery drain planned",
		"instance power premise retracted by its own drain"} {
		if !strings.Contains(logged.String(), fragment) {
			t.Fatalf("tick log missing %q: %q", fragment, logged.String())
		}
	}
	quiet, _ := silenceTicker(t, nil)
	quiet.recordRecoveries(result)
}

// TestAnUnreadablePowerStateIsNamedWithItsErrnoClassAndLatency is the whole of
// what issue #246 could not do. Five reproduction attempts failed to identify the
// trigger, because `tart`'s running() swallows the error and the fleet was never
// told there had been one; two nightlies then died with the answer sitting
// unrecorded at the bottom of the stack. The next occurrence answers for itself.
func TestAnUnreadablePowerStateIsNamedWithItsErrnoClassAndLatency(t *testing.T) {
	reporter, logged := silenceReporter()

	reporter.reportUnreadablePower([]domain.Instance{unreadable(domain.PowerReadDescriptors)})
	reporter.reportUnreadablePower([]domain.Instance{unreadable(domain.PowerReadDescriptors)})

	for _, fragment := range []string{"instance power state unreadable",
		"instance=trf-large-f7d1fac9141ad580", "reason=descriptor_exhaustion", "readLatency=1.234s"} {
		if !strings.Contains(logged.String(), fragment) {
			t.Fatalf("unreadable-power log missing %q: %q", fragment, logged.String())
		}
	}
	if lines := strings.Count(logged.String(), "instance power state unreadable"); lines != 1 {
		t.Fatalf("a condition that persists for minutes must be reported once per reason, got %d", lines)
	}
	// A reason that CHANGES is itself the finding, so it is not suppressed.
	reporter.reportUnreadablePower([]domain.Instance{unreadable(domain.PowerReadPermission)})
	if !strings.Contains(logged.String(), "reason=permission_denied") {
		t.Fatalf("a new failure class was suppressed as a repeat: %q", logged.String())
	}
}

// Nothing is said about an instance whose power WAS read, or about one that is
// no longer a fact about the fleet.
func TestOnlyALiveUnreadableInstanceIsReported(t *testing.T) {
	dead := unreadable(domain.PowerReadIO)
	dead.State = domain.InstanceDeleted
	for _, instance := range []domain.Instance{glitched(false), dead} {
		reporter, logged := silenceReporter()
		reporter.reportUnreadablePower([]domain.Instance{instance})
		if logged.Len() != 0 {
			t.Fatalf("instance %#v was reported: %q", instance, logged.String())
		}
	}
}
