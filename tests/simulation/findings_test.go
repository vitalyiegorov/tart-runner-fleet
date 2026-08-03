package simulation_test

import (
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

// The tests in this file PIN the defects the simulator found. Each one is a
// characterization, not a repair: it asserts today's behaviour so the finding
// cannot silently change shape while it waits for its own fix PR, and so the
// eventual fix arrives with a test that was already red in spirit.
//
// ADR 0031 carries the findings table these names refer to.

var findingNow = time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)

func findingProfiles() map[domain.ProfileID]domain.Profile {
	return simProfiles()
}

// findingDemand builds one demand of the given age.
func findingDemand(repo string, id int64, age time.Duration, profile domain.ProfileID) domain.Demand {
	definition := findingProfiles()[profile]
	return domain.Demand{Key: domain.DemandKey{Repo: repo, RunID: 900, Attempt: 1, JobID: id},
		CreatedAt: findingNow.Add(-age), Profile: profile, Route: definition.Route,
		Platform: definition.Platform, Event: domain.EventPullRequest}
}

// findingInstance builds one live, resource-consuming instance.
func findingInstance(id, repo string, profile domain.ProfileID, state domain.InstanceState) domain.Instance {
	definition := findingProfiles()[profile]
	return domain.Instance{ID: id, Repo: repo, Demand: domain.DemandKey{Repo: repo, RunID: 1, Attempt: 1, JobID: 1},
		Platform: definition.Platform, Profile: profile, Route: definition.Route,
		Resources: definition.Resources, State: state, Power: domain.InstancePowerRunning}
}

func findingConfig(mutate func(*scheduler.Config)) scheduler.Config {
	config := scheduler.Config{
		LinuxCapacity: domain.Resources{CPU: 10, MemoryMB: 22_528, Slots: 4},
		FairnessAge:   5 * time.Minute,
		RepoCaps:      map[string]int{"a/repo": 4, "b/repo": 4, simControlPlaneRepo: 2},
		Profiles:      findingProfiles(), MixedPlatformAdmission: true, MixedProfileCohorts: true,
	}
	if mutate != nil {
		mutate(&config)
	}
	return config
}

func findingPlan(config scheduler.Config, demands []domain.Demand, instances []domain.Instance, prior scheduler.State) scheduler.Plan {
	return scheduler.PlanTick(scheduler.Input{Now: findingNow, Config: config,
		Demands: domain.Fresh(demands, findingNow), Instances: domain.Fresh(instances, findingNow),
		Host:  domain.Fresh(domain.Host{Available: domain.Resources{CPU: 10, MemoryMB: 22_528, Slots: 4}}, findingNow),
		Prior: prior})
}

func spawnedKeys(plan scheduler.Plan) []domain.DemandKey {
	var keys []domain.DemandKey
	for _, operation := range plan.Operations {
		if operation.Kind == scheduler.OperationSpawn {
			keys = append(keys, operation.Demand)
		}
	}
	return keys
}

func containsKey(keys []domain.DemandKey, want domain.DemandKey) bool {
	for _, key := range keys {
		if key == want {
			return true
		}
	}
	return false
}

// TestFindingMacOSAdmissionIgnoresRepositoryCap pins FINDING 2.
//
// scheduler.appendMacSpawns bounds admission by the envelope and by the
// profile's MaxActive. It never reads Config.RepoCaps, even though
// activeRepoCounts charges a macOS instance to its repository exactly like a
// Linux one -- which is the very assumption ADR 0030's slack arithmetic rests
// on. A repository's cap therefore bounds its Linux work and not its macOS work.
func TestFindingMacOSAdmissionIgnoresRepositoryCap(t *testing.T) {
	t.Parallel()
	config := findingConfig(func(config *scheduler.Config) {
		config.RepoCaps = map[string]int{"a/repo": 1}
	})
	// One live Linux instance already fills a/repo's cap of one.
	live := []domain.Instance{findingInstance("linux-live", "a/repo", "small", domain.InstanceRunning)}
	// Two macOS maestro demands from the same repository. Under the cap exactly
	// zero of them may be admitted.
	first := findingDemand("a/repo", 11, 30*time.Minute, "maestro")
	second := findingDemand("a/repo", 12, 29*time.Minute, "maestro")

	plan := findingPlan(config, []domain.Demand{first, second}, live, scheduler.State{})
	admitted := spawnedKeys(plan)
	if len(admitted) == 0 {
		t.Fatalf("FINDING 2 no longer reproduces: macOS admission now respects the repository cap: %#v", plan)
	}
	if !containsKey(admitted, first.Key) {
		t.Fatalf("finding changed shape; admitted %v", admitted)
	}
	// The characterization: the repository cap of one is exceeded by macOS work
	// while the identical Linux request would have been refused.
	linux := findingDemand("a/repo", 13, 30*time.Minute, "small")
	if refused := spawnedKeys(findingPlan(config, []domain.Demand{linux}, live, scheduler.State{})); len(refused) != 0 {
		t.Fatalf("Linux admission must still honour the cap, got %v", refused)
	}
}

// TestFindingControlPlaneOvertakesAgedStandardWork pins FINDING 3.
//
// ADR 0004 states the lane rule as "control-plane work can bypass only YOUNG
// standard work; aged global FIFO ... remains absolute", and priorityOrder
// honours it. exactSelect then re-ranks the same candidates by scheduling class
// alone (betterAdmission's band coverage) and aging is not a band there, so
// inside a backfill a young control-plane demand outranks an aged standard one.
func TestFindingControlPlaneOvertakesAgedStandardWork(t *testing.T) {
	t.Parallel()
	config := findingConfig(func(config *scheduler.Config) {
		config.RepoSchedulingClasses = map[string]domain.SchedulingClass{simControlPlaneRepo: domain.SchedulingControlPlane}
		config.LinuxCapacity = domain.Resources{CPU: 4, MemoryMB: 8_192, Slots: 4}
	})
	// The aged head is an xl that cannot fit the four free cores, so it reserves
	// and ADR 0017 opens the residual to backfill.
	head := findingDemand("a/repo", 21, 60*time.Minute, "xl")
	agedStandard := findingDemand("b/repo", 22, 30*time.Minute, "large")
	youngControlPlane := findingDemand(simControlPlaneRepo, 23, time.Minute, "large")

	plan := scheduler.PlanTick(scheduler.Input{Now: findingNow, Config: config,
		Demands:   domain.Fresh([]domain.Demand{head, agedStandard, youngControlPlane}, findingNow),
		Instances: domain.Fresh([]domain.Instance(nil), findingNow),
		Host:      domain.Fresh(domain.Host{Available: domain.Resources{CPU: 4, MemoryMB: 8_192, Slots: 4}}, findingNow)})
	admitted := spawnedKeys(plan)
	if containsKey(admitted, agedStandard.Key) {
		t.Fatalf("FINDING 3 no longer reproduces: aged standard work now keeps its place: %v", admitted)
	}
	if !containsKey(admitted, youngControlPlane.Key) {
		t.Fatalf("finding changed shape; admitted %v", admitted)
	}
	if plan.Next.Reservation == nil || plan.Next.Reservation.Demand != head.Key {
		t.Fatalf("the aged head must still hold the reservation: %#v", plan.Next.Reservation)
	}
}

// TestFindingCountMaximizationOvertakesAgedWork pins FINDING 5.
//
// exactSelect searches for the admission with the largest COUNT inside a
// scheduling band. throughputOrder's own comment promises that
// shortest-resource-first applies only to young work "because aging is applied
// before this function", but exactSelect receives the aged and young candidates
// in one slice, so two younger small demands still outrank one aged large.
func TestFindingCountMaximizationOvertakesAgedWork(t *testing.T) {
	t.Parallel()
	config := findingConfig(func(config *scheduler.Config) {
		config.LinuxCapacity = domain.Resources{CPU: 4, MemoryMB: 8_192, Slots: 4}
	})
	head := findingDemand("a/repo", 31, 60*time.Minute, "xl")
	agedLarge := findingDemand("b/repo", 32, 40*time.Minute, "large")
	youngSmallA := findingDemand("a/repo", 33, time.Minute, "small")
	youngSmallB := findingDemand("b/repo", 34, time.Minute, "small")

	plan := scheduler.PlanTick(scheduler.Input{Now: findingNow, Config: config,
		Demands:   domain.Fresh([]domain.Demand{head, agedLarge, youngSmallA, youngSmallB}, findingNow),
		Instances: domain.Fresh([]domain.Instance(nil), findingNow),
		Host:      domain.Fresh(domain.Host{Available: domain.Resources{CPU: 4, MemoryMB: 8_192, Slots: 4}}, findingNow)})
	admitted := spawnedKeys(plan)
	if containsKey(admitted, agedLarge.Key) {
		t.Fatalf("FINDING 5 no longer reproduces: the aged large kept its place: %v", admitted)
	}
	if !containsKey(admitted, youngSmallA.Key) || !containsKey(admitted, youngSmallB.Key) {
		t.Fatalf("finding changed shape; admitted %v", admitted)
	}
}

// TestFindingCrossPlatformResidualArbitration pins FINDING 4, which is the hole
// ADR 0030 names in its own "Not addressed here": the residual is arbitrated one
// platform at a time -- planLinux runs, then the macOS remainder -- so a younger
// Linux demand takes a vector an older macOS demand was entitled to under the
// cross-platform aged FIFO of ADR 0012.
func TestFindingCrossPlatformResidualArbitration(t *testing.T) {
	t.Parallel()
	config := findingConfig(nil)
	// An aged macOS builder heads the global FIFO and cannot fit beside the live
	// xl, so ADR 0017 opens the residual. Four cores remain.
	live := []domain.Instance{findingInstance("xl-live", "a/repo", "xl", domain.InstanceRunning)}
	head := findingDemand("a/repo", 41, 90*time.Minute, "builder")
	agedMaestro := findingDemand("b/repo", 42, 60*time.Minute, "maestro")
	youngLarge := findingDemand("b/repo", 43, time.Minute, "large")

	plan := scheduler.PlanTick(scheduler.Input{Now: findingNow, Config: config,
		Demands:   domain.Fresh([]domain.Demand{head, agedMaestro, youngLarge}, findingNow),
		Instances: domain.Fresh(live, findingNow),
		Host:      domain.Fresh(domain.Host{Available: domain.Resources{CPU: 4, MemoryMB: 10_240, Slots: 3}}, findingNow)})
	admitted := spawnedKeys(plan)
	if containsKey(admitted, agedMaestro.Key) {
		t.Fatalf("FINDING 4 no longer reproduces: the aged macOS demand won the residual: %v", admitted)
	}
	if !containsKey(admitted, youngLarge.Key) {
		t.Fatalf("finding changed shape; admitted %v", admitted)
	}
}
