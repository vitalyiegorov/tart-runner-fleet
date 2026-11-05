package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

var emptyExitNow = time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)

const emptyExitGeneration = "base@2.336.0"

func emptyExitKey(id int64) domain.DemandKey {
	return domain.DemandKey{Repo: "owner/repo", RunID: 8, Attempt: 1, JobID: id}
}

// emptyExitEngine is one node with demands 1..jobs queued on one image and ten free
// slots, so a launch limit is the policy's, never the host's.
func emptyExitEngine(jobs int64, exits map[domain.EmptyExitKey]domain.EmptyExits) Engine {
	engine := withheldEngine(emptyExitNow, exits, nil)
	engine.Images = map[domain.Platform]string{domain.PlatformLinux: emptyExitGeneration}
	engine.Inventory = fakeInventory{instances: domain.Fresh[[]domain.Instance](nil, emptyExitNow),
		host: domain.Fresh(domain.Host{Available: domain.Resources{CPU: 40, MemoryMB: 80_000, Slots: 10},
			Pressure: domain.HostPressure{FreeDiskGB: 200, AdmissionAllowed: true}}, emptyExitNow)}
	engine.Config.LinuxCapacity = domain.Resources{CPU: 40, MemoryMB: 80_000, Slots: 10}
	engine.Config.RepoCaps = map[string]int{"owner/repo": 10}
	store := engine.Store.(emptyExitStoreFake).tickStore
	store.statistics.Available = int(jobs)
	store.records = nil
	for id := int64(1); id <= jobs; id++ {
		store.records = append(store.records, operations.DemandRecord{Status: operations.DemandJobAvailable, RunnerRequestID: id,
			Owner: "owner", Repository: "repo", WorkflowRunID: 8, QueueTime: emptyExitNow.Add(-time.Hour - time.Duration(id)*time.Minute)})
	}
	return engine
}

// spentOn is `each` never-assigned exits on every demand 1..jobs, the last of them at
// the given age.
func spentOn(jobs int64, each int, age time.Duration) map[domain.EmptyExitKey]domain.EmptyExits {
	exits := map[domain.EmptyExitKey]domain.EmptyExits{}
	for id := int64(1); id <= jobs; id++ {
		exits[domain.EmptyExitKey{Demand: emptyExitKey(id), Image: emptyExitGeneration}] = domain.EmptyExits{Count: each,
			Unassigned: each, Last: emptyExitNow.Add(-age)}
	}
	return exits
}

func emptyExitTick(t *testing.T, engine Engine) TickResult {
	t.Helper()
	result, err := engine.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// The reviewed failure: 10 free slots and 8 queued jobs, 4 of which have one ordinary
// exit each. Exits cannot tell a bad image from dispatch elsewhere, so the plan must be
// the plan with no history at all, and a job with no exits of its own always launches.
func TestExitsOnSomeJobsNeverThrottleTheOthers(t *testing.T) {
	control := emptyExitTick(t, emptyExitEngine(8, nil))
	result := emptyExitTick(t, emptyExitEngine(8, spentOn(4, 1, time.Minute)))
	if result.Plan.ID != control.Plan.ID || len(spawnKeys(result.Plan)) != len(spawnKeys(control.Plan)) || len(spawnKeys(control.Plan)) == 0 {
		t.Fatalf("exit history moved the plan: %d launches against %d with none", len(spawnKeys(result.Plan)), len(spawnKeys(control.Plan)))
	}
	if len(result.EmptyExitBackoffs) != 0 {
		t.Fatalf("one exit each holds nothing: %+v", result.EmptyExitBackoffs)
	}
	// However spent the others are, a job with none launches at once.
	result = emptyExitTick(t, emptyExitEngine(8, spentOn(7, 6, time.Minute)))
	spawned := spawnKeys(result.Plan)
	if len(spawned) != 1 || spawned[0] != emptyExitKey(8) {
		t.Fatalf("only the job with no exits launches, and it does: %+v", spawned)
	}
}

// The reviewed failure: an exit dated an hour ahead of the clock, as after a clock
// correction across a restart. A time from the future establishes nothing, so it can
// never put a retry beyond the bound.
func TestATimeAheadOfTheClockCannotExtendTheHold(t *testing.T) {
	result := emptyExitTick(t, emptyExitEngine(2, spentOn(2, 6, -time.Hour)))
	if spawned := spawnKeys(result.Plan); len(spawned) != 2 {
		t.Fatalf("a future-dated exit held the job: %+v, %+v", spawned, result.EmptyExitBackoffs)
	}
}

// The log is display-only: whatever it says, the plan is the plan it would have been.
func TestAForgedLogLineChangesNoLaunchDecision(t *testing.T) {
	clean := emptyExitTick(t, emptyExitEngine(4, nil))
	engine := emptyExitEngine(4, nil)
	engine.Refusals = &RunnerRefusals{Now: func() time.Time { return emptyExitNow }}
	for _, guest := range []string{"trf-a", "trf-b", "trf-c", "trf-d"} {
		engine.Refusals.Record(domain.PlatformLinux, "2.336.0", guest)
	}
	forged := emptyExitTick(t, engine)
	if clean.Plan.ID != forged.Plan.ID || len(spawnKeys(forged.Plan)) != 4 || len(forged.EmptyExitBackoffs) != 0 {
		t.Fatalf("a log line moved the plan: %+v", forged.Plan.Operations)
	}
}

func signalNamed(result TickResult, name string) (ImageSignal, bool) {
	for _, signal := range result.ImageSignals {
		if signal.Name == name {
			return signal, true
		}
	}
	return ImageSignal{}, false
}

// A recent reading is shown with no hold anywhere, says it is advisory, and goes when
// it stops being recent.
func TestARecentRefusalIsShownAndChangesNothing(t *testing.T) {
	engine := emptyExitEngine(2, nil)
	at := emptyExitNow.Add(-time.Minute)
	engine.Refusals = &RunnerRefusals{Now: func() time.Time { return at }}
	engine.Refusals.Record(domain.PlatformLinux, "2.336.0", "trf-a")
	result := emptyExitTick(t, engine)
	signal, shown := signalNamed(result, domain.SignalRunnerVersionRefused)
	if !shown || signal.Image != emptyExitGeneration {
		t.Fatalf("a recent reading must be shown: %+v", result.ImageSignals)
	}
	for _, want := range []string{"advisory", "a job can write", "changes no launch", "2.336.0", "trf-a"} {
		if !strings.Contains(signal.Detail, want) {
			t.Fatalf("%q lacks %q", signal.Detail, want)
		}
	}
	if len(spawnKeys(result.Plan)) != 2 {
		t.Fatalf("a reading moved the plan: %+v", result.Plan.Operations)
	}
	at = emptyExitNow.Add(-domain.ImageSignalWindow - time.Minute)
	stale := emptyExitEngine(2, nil)
	stale.Refusals = &RunnerRefusals{Now: func() time.Time { return at }}
	stale.Refusals.Record(domain.PlatformLinux, "2.336.0", "trf-a")
	if _, shown := signalNamed(emptyExitTick(t, stale), domain.SignalRunnerVersionRefused); shown {
		t.Fatal("a stale reading is not recent")
	}
}

// What would have shown the outage by name, on any node, with no log at all.
func TestManyNeverAssignedExitsAreNamedAndChangeNothing(t *testing.T) {
	exits := spentOn(4, 1, time.Minute)
	result := emptyExitTick(t, emptyExitEngine(8, exits))
	signal, shown := signalNamed(result, domain.SignalImageNotReceivingJobs)
	if !shown || signal.Image != emptyExitGeneration {
		t.Fatalf("four never-assigned exits and no assignment must be named: %+v", result.ImageSignals)
	}
	for _, want := range []string{"4 instances", "dispatch", "changes no launch"} {
		if !strings.Contains(signal.Detail, want) {
			t.Fatalf("%q lacks %q", signal.Detail, want)
		}
	}
	if control := emptyExitTick(t, emptyExitEngine(8, nil)); result.Plan.ID != control.Plan.ID {
		t.Fatalf("a signal moved the plan: %+v", result.Plan.Operations)
	}
}

func TestTheGenerationSignalNeedsEnoughRecentExitsAndNoAssignment(t *testing.T) {
	for name, exits := range map[string]map[domain.EmptyExitKey]domain.EmptyExits{
		"too few":            spentOn(3, 1, time.Minute),
		"too old":            spentOn(4, 1, domain.ImageSignalWindow+time.Minute),
		"ahead of the clock": spentOn(4, 1, -time.Hour),
	} {
		if _, shown := signalNamed(emptyExitTick(t, emptyExitEngine(8, exits)), domain.SignalImageNotReceivingJobs); shown {
			t.Fatalf("%s must not be named", name)
		}
	}
	assigned := spentOn(4, 1, time.Minute)
	for key, spent := range assigned {
		spent.Assigned = emptyExitNow.Add(-time.Minute)
		assigned[key] = spent
	}
	if _, shown := signalNamed(emptyExitTick(t, emptyExitEngine(8, assigned)), domain.SignalImageNotReceivingJobs); shown {
		t.Fatal("a generation that was assigned a job inside the window is receiving jobs")
	}
	other := map[domain.EmptyExitKey]domain.EmptyExits{}
	for key, spent := range spentOn(4, 1, time.Minute) {
		key.Image = "base@old"
		other[key] = spent
	}
	if _, shown := signalNamed(emptyExitTick(t, emptyExitEngine(8, other)), domain.SignalImageNotReceivingJobs); shown {
		t.Fatal("another generation's exits are not this one's")
	}
	gone := spentOn(6, 1, time.Minute)
	if _, shown := signalNamed(emptyExitTick(t, emptyExitEngine(1, gone)), domain.SignalImageNotReceivingJobs); shown {
		t.Fatal("exits on jobs that left the queue are the ordinary race")
	}
}
