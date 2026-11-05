package app

import (
	"context"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

var imageHoldNow = time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)

const imageHoldGeneration = "base@2.336.0"

func imageHoldKey(id int64) domain.DemandKey {
	return domain.DemandKey{Repo: "owner/repo", RunID: 8, Attempt: 1, JobID: id}
}

// imageHoldEngine is one node with demands 1..jobs queued on one image, and whatever
// history the case describes. Ten slots, so a launch limit is the policy's, never the
// host's.
func imageHoldEngine(jobs int64, exits map[domain.EmptyExitKey]domain.EmptyExits, instances []domain.Instance) Engine {
	engine := withheldEngine(imageHoldNow, exits, nil)
	engine.Images = map[domain.Platform]string{domain.PlatformLinux: imageHoldGeneration}
	engine.Inventory = fakeInventory{instances: domain.Fresh(instances, imageHoldNow),
		host: domain.Fresh(domain.Host{Available: domain.Resources{CPU: 40, MemoryMB: 80_000, Slots: 10},
			Pressure: domain.HostPressure{FreeDiskGB: 200, AdmissionAllowed: true}}, imageHoldNow)}
	engine.Config.LinuxCapacity = domain.Resources{CPU: 40, MemoryMB: 80_000, Slots: 10}
	engine.Config.RepoCaps = map[string]int{"owner/repo": 10}
	store := engine.Store.(emptyExitStoreFake).tickStore
	store.statistics.Available = int(jobs)
	store.records = nil
	for id := int64(1); id <= jobs; id++ {
		store.records = append(store.records, operations.DemandRecord{Status: operations.DemandJobAvailable, RunnerRequestID: id,
			Owner: "owner", Repository: "repo", WorkflowRunID: 8, QueueTime: imageHoldNow.Add(-time.Hour - time.Duration(id)*time.Minute)})
	}
	return engine
}

// spentOn is `each` never-assigned exits on every demand 1..jobs, all on the current
// generation, the last of them and the last launch at the given ages.
func spentOn(jobs int64, each int, exitAge, launchAge time.Duration) map[domain.EmptyExitKey]domain.EmptyExits {
	exits := map[domain.EmptyExitKey]domain.EmptyExits{}
	for id := int64(1); id <= jobs; id++ {
		exits[domain.EmptyExitKey{Demand: imageHoldKey(id), Image: imageHoldGeneration}] = domain.EmptyExits{Count: each,
			Unassigned: each, Last: imageHoldNow.Add(-exitAge), Launched: imageHoldNow.Add(-launchAge)}
	}
	return exits
}

func imageHoldTick(t *testing.T, engine Engine) TickResult {
	t.Helper()
	result, err := engine.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func imageRow(result TickResult) (EmptyExitBackoff, bool) {
	for _, backoff := range result.EmptyExitBackoffs {
		if backoff.Demand == (domain.DemandKey{}) {
			return backoff, true
		}
	}
	return EmptyExitBackoff{}, false
}

// The bound that a per-demand backoff cannot give: every queued job on the image is
// held, though none of them has yet earned a hold of its own.
func TestManyDemandsExitingEmptyHoldTheirImage(t *testing.T) {
	result := imageHoldTick(t, imageHoldEngine(4, spentOn(4, 1, time.Minute, 2*time.Minute), nil))
	if spawned := spawnKeys(result.Plan); len(spawned) != 0 {
		t.Fatalf("four empty exits across four demands and nothing was held: %+v", spawned)
	}
	row, held := imageRow(result)
	if !held || row.Image != imageHoldGeneration || row.Exits != 4 {
		t.Fatalf("the hold must be named: %+v", result.EmptyExitBackoffs)
	}
}

// Fewer than that is ordinary, and one job's exits say nothing about the image: a job
// no runner will take must not hold back the younger one a runner would.
func TestAThresholdAndTwoDemandsAreNeededToHoldAnImage(t *testing.T) {
	few := imageHoldTick(t, imageHoldEngine(4, spentOn(3, 1, time.Minute, time.Minute), nil))
	if len(spawnKeys(few.Plan)) != 4 {
		t.Fatalf("three exits are ordinary: %+v", few.Plan.Operations)
	}
	if _, held := imageRow(few); held {
		t.Fatalf("three exits named a hold: %+v", few.EmptyExitBackoffs)
	}
	one := imageHoldTick(t, imageHoldEngine(2, spentOn(1, 6, time.Minute, time.Minute), nil))
	spawned := spawnKeys(one.Plan)
	if len(spawned) != 1 || spawned[0] != imageHoldKey(2) {
		t.Fatalf("the younger job must run, and the spent one wait out its own hold: %+v", spawned)
	}
}

// One assignment on the generation clears it at once, however many exits came before.
func TestOneAssignmentClearsTheImageHoldAtOnce(t *testing.T) {
	exits := spentOn(4, 3, time.Minute, time.Minute)
	for key, spent := range exits {
		spent.Unassigned = 0
		exits[key] = spent
	}
	result := imageHoldTick(t, imageHoldEngine(5, exits, nil))
	if _, held := imageRow(result); held {
		t.Fatalf("an assignment must clear the hold: %+v", result.EmptyExitBackoffs)
	}
	spawned := spawnKeys(result.Plan)
	if len(spawned) != 1 || spawned[0] != imageHoldKey(5) {
		t.Fatalf("all that still waits is each demand's own hold: %+v", spawned)
	}
}

// Exits on another generation are not this one's: a swap starts clean.
func TestAnotherGenerationsExitsDoNotHoldThisOne(t *testing.T) {
	exits := map[domain.EmptyExitKey]domain.EmptyExits{}
	for key, spent := range spentOn(4, 1, time.Minute, time.Minute) {
		key.Image = "base@old"
		exits[key] = spent
	}
	result := imageHoldTick(t, imageHoldEngine(4, exits, nil))
	if _, held := imageRow(result); held || len(spawnKeys(result.Plan)) != 4 {
		t.Fatalf("got %+v %+v", result.EmptyExitBackoffs, result.Plan.Operations)
	}
}

// Exits on a job no longer queued are the ordinary race: another runner took it, or
// it was cancelled. They are absent from the sum, so they cannot hold anything.
func TestExitsOnAJobThatIsNoLongerQueuedDoNotCount(t *testing.T) {
	result := imageHoldTick(t, imageHoldEngine(1, spentOn(6, 1, time.Minute, time.Minute), nil))
	if _, held := imageRow(result); held {
		t.Fatalf("five of six exits were on jobs that left the queue: %+v", result.EmptyExitBackoffs)
	}
}

// Once due, one guest launches: the retry is the probe. The launch itself is what
// spends the interval, whatever becomes of the guest, so a read that fails or a guest
// that never reports cannot let a second one through a minute later.
func TestARetryLaunchesOneGuestAndSpendsTheInterval(t *testing.T) {
	due := imageHoldTick(t, imageHoldEngine(4, spentOn(4, 2, time.Hour, time.Hour), nil))
	if spawned := spawnKeys(due.Plan); len(spawned) != 1 {
		t.Fatalf("one retry: %+v", spawned)
	}
	spent := imageHoldTick(t, imageHoldEngine(4, spentOn(4, 2, time.Hour, time.Minute), nil))
	if spawned := spawnKeys(spent.Plan); len(spawned) != 0 {
		t.Fatalf("a launch a minute ago spent the interval: %+v", spawned)
	}
}

// A retry goes to the job that has cost the least, so one that keeps failing cannot
// take every retry from a job a runner would take.
func TestARetryPrefersTheJobThatHasCostTheLeast(t *testing.T) {
	exits := spentOn(3, 2, time.Hour, time.Hour)
	result := imageHoldTick(t, imageHoldEngine(4, exits, nil))
	spawned := spawnKeys(result.Plan)
	if len(spawned) != 1 || spawned[0] != imageHoldKey(4) {
		t.Fatalf("the job with no exits goes first: %+v", spawned)
	}
}

// A stranded instance suppresses nothing, because no state counts as answering: only
// time does. It keeps its own demand out of the queue and nothing else.
func TestAStrandedPlannedInstanceDoesNotSuppressLaunches(t *testing.T) {
	stranded := func(state domain.InstanceState) []domain.Instance {
		instance := incarnation(state)
		instance.Demand = imageHoldKey(1)
		return []domain.Instance{instance}
	}
	for _, state := range []domain.InstanceState{domain.InstancePlanned, domain.InstanceCloning, domain.InstanceDraining, domain.InstanceStopping} {
		t.Run(string(state), func(t *testing.T) {
			result := imageHoldTick(t, imageHoldEngine(4, spentOn(4, 2, time.Hour, time.Hour), stranded(state)))
			spawned := spawnKeys(result.Plan)
			if len(spawned) != 1 || spawned[0] == imageHoldKey(1) {
				t.Fatalf("a stranded %s must not suppress the retry: %+v", state, spawned)
			}
		})
	}
	// The same at the head of the failure reported at 0b1bda5: readings on file, and a
	// Planned row that has stood for hours.
	engine := imageHoldEngine(2, nil, stranded(domain.InstancePlanned))
	engine.Refusals = &RunnerRefusals{Now: func() time.Time { return imageHoldNow.Add(-time.Hour) }}
	engine.Refusals.Record(domain.PlatformLinux, "2.336.0", "trf-a")
	engine.Refusals.Record(domain.PlatformLinux, "2.336.0", "trf-b")
	if spawned := spawnKeys(imageHoldTick(t, engine).Plan); len(spawned) != 1 || spawned[0] != imageHoldKey(2) {
		t.Fatalf("a stranded Planned row suppressed launches: %+v", spawned)
	}
}

// The log is display-only. It can be written by a job, so whatever it says, however
// often, from however many guests, the plan is the plan it would have been.
func TestAForgedLogLineChangesNoLaunchDecision(t *testing.T) {
	clean := imageHoldTick(t, imageHoldEngine(4, nil, nil))
	engine := imageHoldEngine(4, nil, nil)
	engine.Refusals = &RunnerRefusals{Now: func() time.Time { return imageHoldNow }}
	for _, guest := range []string{"trf-a", "trf-b", "trf-c", "trf-d"} {
		engine.Refusals.Record(domain.PlatformLinux, "2.336.0", guest)
	}
	forged := imageHoldTick(t, engine)
	if clean.Plan.ID != forged.Plan.ID || len(spawnKeys(forged.Plan)) != 4 {
		t.Fatalf("a log line moved the plan: %+v", forged.Plan.Operations)
	}
	// Nor does it become a reason to hold on a node that has no other evidence.
	if _, held := imageRow(forged); held || len(forged.EmptyExitBackoffs) != 0 {
		t.Fatalf("a log line became a hold: %+v", forged.EmptyExitBackoffs)
	}
}

// A retry belongs to work the scheduler can start. A job wider than the node's Linux cap
// will ever allow, which has cost the least and is met first, must not hold the launch
// from a job behind it that fits.
func TestARetryGoesToWorkThatCanStart(t *testing.T) {
	exits := spentOn(3, 2, time.Hour, time.Hour)
	engine := imageHoldEngine(3, exits, nil)
	small := engine.Config.Profiles["small"]
	big := domain.Profile{ID: "big", Route: "big", Platform: domain.PlatformLinux, Resources: domain.Resources{CPU: 64, MemoryMB: 4096, Slots: 1}}
	engine.Config.Profiles = map[domain.ProfileID]domain.Profile{"small": small, "big": big}
	engine.Config.RepoCaps = map[string]int{"owner/big": 2, "owner/repo": 10}
	engine.Bindings = []Binding{
		{ScaleSetID: 1, Profile: big, Targets: []string{"owner/big"}},
		{ScaleSetID: 2, Profile: small, Targets: []string{"owner/repo"}},
	}
	store := engine.Store.(emptyExitStoreFake).tickStore
	store.statistics.Available = 4
	store.records = append(store.records, operations.DemandRecord{Status: operations.DemandJobAvailable, RunnerRequestID: 99,
		Owner: "owner", Repository: "big", WorkflowRunID: 8, QueueTime: imageHoldNow.Add(-3 * time.Hour)})
	result := imageHoldTick(t, engine)
	spawned := spawnKeys(result.Plan)
	if len(spawned) != 1 || spawned[0].Repo != "owner/repo" {
		t.Fatalf("one retry, of work that fits: %+v", spawned)
	}
	if len(result.Demands) != 1 || result.Demands[0].Key != spawned[0] {
		t.Fatalf("the plan's queue must be what it planned: %+v", result.Demands)
	}
}

// A tick that could start nothing spends nothing: only a launch dates the hold, so the
// next tick asks again.
func TestARetryThatCannotStartIsNotSpent(t *testing.T) {
	engine := imageHoldEngine(4, spentOn(4, 2, time.Hour, time.Hour), nil)
	ready := engine.Inventory
	engine.Inventory = fakeInventory{instances: domain.Fresh[[]domain.Instance](nil, imageHoldNow),
		host: domain.Fresh(domain.Host{Pressure: domain.HostPressure{FreeDiskGB: 200}}, imageHoldNow)}
	if spawned := spawnKeys(imageHoldTick(t, engine).Plan); len(spawned) != 0 {
		t.Fatalf("a node refusing admission launched: %+v", spawned)
	}
	engine.Inventory = ready
	if spawned := spawnKeys(imageHoldTick(t, engine).Plan); len(spawned) != 1 {
		t.Fatalf("the retry was spent on a tick that started nothing: %+v", spawned)
	}
}
