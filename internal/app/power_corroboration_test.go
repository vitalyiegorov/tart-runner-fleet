package app

import (
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

var corroboratorEpoch = time.Date(2026, 8, 18, 4, 22, 59, 0, time.UTC)

// stoppedReading is one live instance the backend enumerates as powered off.
func stoppedReading(id string) domain.Instance {
	return domain.Instance{ID: id, State: domain.InstanceRunning, Power: domain.InstancePowerStopped}
}

// tick folds one round of readings at the given offset from the epoch and
// returns what the corroborator lets through.
func tick(c *PowerCorroborator, offset time.Duration, instances ...domain.Instance) []domain.Instance {
	return c.Observe(instances, corroboratorEpoch.Add(offset))
}

// The production storm, at the layer that produced it. `tart list` said the VM
// was off on essentially every tick for nine minutes while the drain's own re-read
// said it was running; the fleet acted on the first reading and every one after
// it (issue #246). Here it must act on none of them until the bound is met.
func TestAStoppedReadingIsWithheldUntilItIsCorroborated(t *testing.T) {
	corroborator := &PowerCorroborator{}
	poll := 5 * time.Second
	var released int
	for reading := range 12 {
		out := tick(corroborator, time.Duration(reading)*poll, stoppedReading("trf-large-f7d1fac9141ad580"))
		if out[0].Power == domain.InstancePowerStopped {
			released++
			continue
		}
		if out[0].Power != domain.InstancePowerUnknown {
			t.Fatalf("reading %d: an uncorroborated stopped reading must be unknown, got %q", reading, out[0].Power)
		}
	}
	// Both halves of the bound must be met, and at a five-second poll the window is
	// what binds: three readings arrive in ten seconds, the window needs forty-five.
	if firstRelease := 12 - released; firstRelease*int(poll.Seconds()) < int(domain.PowerCorroboration.Window.Seconds()) {
		t.Fatalf("the reading was acted on after %d polls, inside the %s window",
			firstRelease, domain.PowerCorroboration.Window)
	}
	if released == 0 {
		t.Fatal("a reading that held for the whole bound must eventually be acted on")
	}
}

// The run is per instance. A sibling misreported in the same enumeration must not
// corroborate anything about a healthy one — the production storms hit exactly one
// instance while its siblings in the same `tart list` output read correctly.
func TestRunsAreKeptPerInstance(t *testing.T) {
	corroborator := &PowerCorroborator{}
	healthy := domain.Instance{ID: "healthy", State: domain.InstanceRunning, Power: domain.InstancePowerRunning}
	for reading := range 4 {
		out := tick(corroborator, time.Duration(reading)*30*time.Second, stoppedReading("glitched"), healthy)
		if out[1].Power != domain.InstancePowerRunning || out[1].PowerRun.Refusals != 0 {
			t.Fatalf("reading %d: the healthy instance must be untouched, got %#v", reading, out[1])
		}
	}
	out := tick(corroborator, 2*time.Minute, stoppedReading("glitched"), healthy)
	if out[0].Power != domain.InstancePowerStopped {
		t.Fatalf("the glitched instance's own run must still corroborate; got %q", out[0].Power)
	}
}

// An instance the corroborator no longer sees is forgotten, so a recycled name
// cannot inherit a run about a machine that no longer exists.
func TestAnAbsentInstanceIsForgotten(t *testing.T) {
	corroborator := &PowerCorroborator{}
	for reading := range 4 {
		tick(corroborator, time.Duration(reading)*30*time.Second, stoppedReading("gone"))
	}
	tick(corroborator, 2*time.Minute) // the instance is no longer live
	out := tick(corroborator, 3*time.Minute, stoppedReading("gone"))
	if out[0].Power != domain.InstancePowerUnknown || out[0].PowerRun.Refusals != 1 {
		t.Fatalf("a forgotten instance must start a fresh run; got %#v", out[0])
	}
}

// A reading that agrees the VM is running clears the run outright, which is what
// makes an aborted recovery self-limiting: the premise must be rebuilt from
// nothing rather than resumed where it left off.
func TestAnAgreeingReadingResetsTheRun(t *testing.T) {
	corroborator := &PowerCorroborator{}
	for reading := range 3 {
		tick(corroborator, time.Duration(reading)*30*time.Second, stoppedReading("flapping"))
	}
	running := domain.Instance{ID: "flapping", State: domain.InstanceRunning, Power: domain.InstancePowerRunning}
	tick(corroborator, 90*time.Second, running)
	out := tick(corroborator, 2*time.Minute, stoppedReading("flapping"))
	if out[0].Power != domain.InstancePowerUnknown || out[0].PowerRun.Refusals != 1 {
		t.Fatalf("an agreeing reading must reset the run to nothing; got %#v", out[0])
	}
}

// A nil corroborator is the fail-closed default: it withholds nothing because it
// accumulates nothing, and it must never be the thing that authorizes a reclaim.
func TestANilCorroboratorAccumulatesNothing(t *testing.T) {
	var corroborator *PowerCorroborator
	out := corroborator.Observe([]domain.Instance{stoppedReading("unwired")}, corroboratorEpoch)
	if len(out) != 1 || out[0].PowerRun.Refusals != 0 {
		t.Fatalf("a nil corroborator must pass instances through untouched; got %#v", out)
	}
}
