package domain

import (
	"testing"
	"time"
)

var corroborationNow = time.Date(2026, 8, 18, 4, 23, 1, 0, time.UTC)

// corroborated is a run of `stopped` readings that satisfies both halves of
// PowerCorroboration as of corroborationNow.
func corroborated() ObservationRun {
	return ObservationRun{Refusals: PowerCorroboration.ConsecutiveRefusals,
		RefusedSince: corroborationNow.Add(-PowerCorroboration.Window), LastProbe: corroborationNow}
}

// On 2026-08-18 a single `tart list` reading that said a running VM was powered
// off planned a destructive drain; the drain's own re-read of the same source,
// two seconds later, contradicted it and aborted. That repeated eighty-six times
// in nine minutes, and a hundred and fifteen times the night before (issue #246).
//
// `tart` reports a VM whose configuration it cannot open as `"Running": false`:
// its running() swallows the error and answers "not running". One such claim must
// not be a fact the fleet acts on.
func TestOneUncorroboratedStoppedReadingIsNotAFact(t *testing.T) {
	for _, test := range []struct {
		name string
		run  ObservationRun
	}{
		{name: "a first reading", run: ObservationRun{Refusals: 1, RefusedSince: corroborationNow, LastProbe: corroborationNow}},
		{name: "enough readings, too short a window", run: ObservationRun{
			Refusals:     PowerCorroboration.ConsecutiveRefusals,
			RefusedSince: corroborationNow.Add(-PowerCorroboration.Window + time.Second), LastProbe: corroborationNow}},
		{name: "a long enough window, too few readings", run: ObservationRun{
			Refusals:     PowerCorroboration.ConsecutiveRefusals - 1,
			RefusedSince: corroborationNow.Add(-time.Hour), LastProbe: corroborationNow}},
		{name: "no run recorded at all", run: ObservationRun{}},
		{name: "a run that began in the future", run: ObservationRun{
			Refusals:     PowerCorroboration.ConsecutiveRefusals,
			RefusedSince: corroborationNow.Add(time.Hour), LastProbe: corroborationNow}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := CorroboratedPower(InstancePowerStopped, InstanceRunning, test.run, false, corroborationNow)
			if got != InstancePowerUnknown {
				t.Fatalf("%s must classify as unknown, not %q", test.name, got)
			}
		})
	}
}

// The bound delays a reclaim; it never withholds one. A backend that has said the
// same thing three times over forty-five seconds is stating a fact.
func TestACorroboratedStoppedReadingSurvivesClassification(t *testing.T) {
	if got := CorroboratedPower(InstancePowerStopped, InstanceRunning, corroborated(), false, corroborationNow); got != InstancePowerStopped {
		t.Fatalf("a corroborated reading must survive as stopped; got %q", got)
	}
}

// Only `stopped` is ever downgraded. Running is not a destructive premise, and
// absent has its own gate with its own evidence — neither waits on a run.
func TestOnlyAStoppedReadingIsEverWithheld(t *testing.T) {
	for _, power := range []InstancePower{InstancePowerRunning, InstancePowerAbsent, InstancePowerUnknown} {
		if got := CorroboratedPower(power, InstanceRunning, ObservationRun{}, false, corroborationNow); got != power {
			t.Fatalf("power %q must pass through unchanged; got %q", power, got)
		}
	}
}

// The run is half the damping. A drain that aborts returns the instance to
// Running, and the first reading that agrees clears the run outright — so the
// premise must be rebuilt from nothing before it can act again.
func TestAnAgreeingReadingClearsTheRun(t *testing.T) {
	run := PowerCorroboration.Observe(corroborated(), PowerSignal(InstancePowerRunning), corroborationNow)
	if run.Refusals != 0 || !run.RefusedSince.IsZero() {
		t.Fatalf("a running reading must clear the run outright; got %#v", run)
	}
	if got := CorroboratedPower(InstancePowerStopped, InstanceRunning, run, false, corroborationNow); got != InstancePowerUnknown {
		t.Fatalf("a cleared run must not corroborate the next stopped reading; got %q", got)
	}
}

// An unread power state established nothing, so it may neither extend a run nor
// start one. Absence is a different fact and must not accumulate here either.
func TestAnUnreadPowerStateNeitherAccumulatesNorSurvives(t *testing.T) {
	for _, power := range []InstancePower{InstancePowerUnknown, InstancePowerAbsent} {
		if signal := PowerSignal(power); signal != GuestLivenessUnknown {
			t.Fatalf("power %q must fold to an unknown signal; got %q", power, signal)
		}
		run := PowerCorroboration.Observe(corroborated(), PowerSignal(power), corroborationNow)
		if run.Refusals != 0 || !run.RefusedSince.IsZero() {
			t.Fatalf("power %q must clear the run rather than extend it; got %#v", power, run)
		}
	}
}

// The other half of the damping. Once the drain's own re-read has sent a stopped
// recovery back to Running, the same reading must hold for far longer before it
// may act again — otherwise a persistently wrong enumeration re-derives the
// identical operation as fast as the run refills, which is the storm itself.
func TestARetractedPremiseMustHoldForLonger(t *testing.T) {
	run := corroborated()
	if CorroboratedPower(InstancePowerStopped, InstanceRunning, run, true, corroborationNow) != InstancePowerUnknown {
		t.Fatal("a retracted premise must not be corroborated by the ordinary bound")
	}
	held := PowerCorroboration.Window * PowerRetractedFactor
	run.RefusedSince = corroborationNow.Add(-held)
	if got := CorroboratedPower(InstancePowerStopped, InstanceRunning, run, true, corroborationNow); got != InstancePowerStopped {
		t.Fatalf("a retracted premise held for %s must be corroborated; got %q", held, got)
	}
}

// The escalated bound raises the window and nothing else. The count is already
// the glitch bound; doubling it too would only delay a genuinely stopped VM's
// reclaim without excluding anything the window does not.
func TestTheRetractedBoundRaisesOnlyTheWindow(t *testing.T) {
	ordinary, retracted := powerBound(false), powerBound(true)
	if retracted.ConsecutiveRefusals != ordinary.ConsecutiveRefusals {
		t.Fatalf("the count must be unchanged: %d vs %d", retracted.ConsecutiveRefusals, ordinary.ConsecutiveRefusals)
	}
	if retracted.Window != ordinary.Window*PowerRetractedFactor {
		t.Fatalf("the window must be scaled by %d: %s vs %s", PowerRetractedFactor, retracted.Window, ordinary.Window)
	}
}

// A stopped reading for an instance the fleet has already ordered to stop is
// corroborated by that order. Requiring a second source there would delay
// releasing the vector of a VM everyone agrees is finished, on every teardown,
// which is the capacity turnaround the fleet runs on — and it would buy nothing:
// the surprise this bound exists for is a stopped reading nobody asked for.
func TestAnInstanceTheFleetIsTearingDownNeedsNoCorroboration(t *testing.T) {
	for _, state := range []InstanceState{InstanceDraining, InstanceDeregistering, InstanceStopping} {
		if got := CorroboratedPower(InstancePowerStopped, state, ObservationRun{}, false, corroborationNow); got != InstancePowerStopped {
			t.Fatalf("state %q must not wait on a run; got %q", state, got)
		}
	}
	for _, state := range []InstanceState{InstanceAssigned, InstanceRunning, InstanceOnlineIdle} {
		if got := CorroboratedPower(InstancePowerStopped, state, ObservationRun{}, false, corroborationNow); got != InstancePowerUnknown {
			t.Fatalf("state %q must wait on a run; got %q", state, got)
		}
	}
}
