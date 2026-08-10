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

// TestMacOSAdmissionHonorsRepositoryCap was FINDING 2's characterization and is
// now its regression.
//
// scheduler.appendMacSpawns bounded admission by the envelope and by the
// profile's MaxActive and by nothing else: it never read Config.RepoCaps, even
// though activeRepoCounts charges a macOS instance to its repository exactly
// like a Linux one -- which is the very assumption ADR 0030's slack arithmetic
// rests on. A repository's cap therefore bounded its Linux work and not its
// macOS work. Since 2026-08-03 every macOS spawn passes the same cap the Linux
// allocator applies.
func TestMacOSAdmissionHonorsRepositoryCap(t *testing.T) {
	t.Parallel()
	config := findingConfig(func(config *scheduler.Config) {
		config.RepoCaps = map[string]int{"a/repo": 1, "b/repo": 4}
	})
	// One live Linux instance already fills a/repo's cap of one.
	live := []domain.Instance{findingInstance("linux-live", "a/repo", "small", domain.InstanceRunning)}
	// Two macOS maestro demands from the same repository. Under the cap exactly
	// zero of them may be admitted.
	first := findingDemand("a/repo", 11, 30*time.Minute, "maestro")
	second := findingDemand("a/repo", 12, 29*time.Minute, "maestro")
	if admitted := spawnedKeys(findingPlan(config, []domain.Demand{first, second}, live, scheduler.State{})); len(admitted) != 0 {
		t.Fatalf("macOS admission exceeded a/repo's cap of one: %v", admitted)
	}
	// The cap refuses that repository, never the pass: work from a repository
	// with room still takes the vector.
	elsewhere := findingDemand("b/repo", 13, 28*time.Minute, "maestro")
	admitted := spawnedKeys(findingPlan(config, []domain.Demand{first, second, elsewhere}, live, scheduler.State{}))
	if len(admitted) != 1 || !containsKey(admitted, elsewhere.Key) {
		t.Fatalf("a capped repository must not block another repository's macOS work: %v", admitted)
	}
	// And the Linux side of the same cap is unchanged.
	linux := findingDemand("a/repo", 14, 30*time.Minute, "small")
	if refused := spawnedKeys(findingPlan(config, []domain.Demand{linux}, live, scheduler.State{})); len(refused) != 0 {
		t.Fatalf("Linux admission must still honour the cap, got %v", refused)
	}
}

// TestAgedStandardWorkOutranksYoungControlPlane was FINDING 3's
// characterization and is now its regression.
//
// ADR 0004 orders scheduling as aged global FIFO, then young control-plane, then
// young standard, and priorityOrder honoured it. exactSelect then re-ranked the
// same candidates by scheduling class alone (betterAdmission's band coverage)
// and aging was not a band there, so inside a backfill a young control-plane
// demand outranked an aged standard one. Since 2026-08-03 aging is the first
// band, so the band vector and priorityOrder state the same rule.
func TestAgedStandardWorkOutranksYoungControlPlane(t *testing.T) {
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
	if !containsKey(admitted, agedStandard.Key) || containsKey(admitted, youngControlPlane.Key) {
		t.Fatalf("aged standard work must take the residual ahead of young control-plane work: %v", admitted)
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
// before this function", but exactSelect receives every aged candidate in one
// slice, so two smaller aged demands still outrank one older aged large.
//
// FINDING 3's repair narrowed this finding's SHAPE and this test moved with it:
// aging is now exactSelect's first band, so young work can no longer maximize
// count ahead of aged work at all. What remains is count maximization INSIDE the
// aged band, where global FIFO is still the written rule and the largest count
// still wins.
func TestFindingCountMaximizationOvertakesAgedWork(t *testing.T) {
	t.Parallel()
	config := findingConfig(func(config *scheduler.Config) {
		config.LinuxCapacity = domain.Resources{CPU: 4, MemoryMB: 8_192, Slots: 4}
	})
	head := findingDemand("a/repo", 31, 60*time.Minute, "xl")
	agedLarge := findingDemand("b/repo", 32, 40*time.Minute, "large")
	agedSmallA := findingDemand("a/repo", 33, 20*time.Minute, "small")
	agedSmallB := findingDemand("b/repo", 34, 19*time.Minute, "small")

	plan := scheduler.PlanTick(scheduler.Input{Now: findingNow, Config: config,
		Demands:   domain.Fresh([]domain.Demand{head, agedLarge, agedSmallA, agedSmallB}, findingNow),
		Instances: domain.Fresh([]domain.Instance(nil), findingNow),
		Host:      domain.Fresh(domain.Host{Available: domain.Resources{CPU: 4, MemoryMB: 8_192, Slots: 4}}, findingNow)})
	admitted := spawnedKeys(plan)
	if containsKey(admitted, agedLarge.Key) {
		t.Fatalf("FINDING 5 no longer reproduces: the older aged large kept its place: %v", admitted)
	}
	if !containsKey(admitted, agedSmallA.Key) || !containsKey(admitted, agedSmallB.Key) {
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

// TestFinding7AReservedHeadHeldByARepositoryCapLendsItsVector was FINDING 7's
// characterization and is now its REGRESSION.
//
// Seed 67 of the container-node arm stalled admission for more than LivenessK
// ticks in this state: `c/repo` at its cap of two, the aged reserved head a
// `c/repo` `xl` whose vector the host could hand over immediately, and the
// demand waiting behind it from a repository with no live instance at all. The
// mechanism is `safeBackfill`'s remainder subtraction, and ADR 0038 makes the
// head lend the vector it cannot use to work it still outranks.
//
// Three things about this test are corrections to how the finding was pinned
// while it was open, and each one is a lesson rather than a detail.
//
// It is a ONE-TICK PlanTick rather than a trace pin. The original comment said a
// direct PlanTick over the same instances and demands ADMITS the waiting work,
// so the wedge could only be a cross-tick composition. That was wrong: it is a
// property of one tick's inputs, in both directions of the prior state. A trace
// pin also only holds while `overrun_job` is armed, because the wedge lasts
// exactly as long as the cap holder runs -- so the trace pin was hostage to a
// generator feature, and this one is not.
//
// It asserts the ADMISSION rather than the signature. While the finding was open
// it asserted that the wedge kept its documented shape; its own comment
// anticipated flipping to the admission once the fix landed, and this is that
// flip. `sigReservedHeadHeldByARepositoryCap` is consequently NOT in
// `knownFinding` any more: a wedge of this shape fails the sweep like any other.
//
// And the reason it needed restoring at all is worth carrying: the pin was
// retired because no seed reproduced it, and no seed reproduced it because an
// oracle refinement had stopped the harness from being able to see it.
func TestFinding7AReservedHeadHeldByARepositoryCapLendsItsVector(t *testing.T) {
	t.Parallel()
	cfg := containerNodeWorld()
	head := oracleDemand(cfg, "c/repo", 9, 13*time.Minute, "xl")
	waiting := oracleDemand(cfg, "a/repo", 10, 9*time.Minute+30*time.Second, "large")
	host := domain.Fresh(domain.Host{Available: issue226Available(),
		Capacity: domain.Resources{CPU: 12, MemoryMB: 30_720, Slots: 4}}, oracleNow)

	plan := scheduler.PlanTick(scheduler.Input{Now: oracleNow, Config: cfg.Scheduler,
		Demands:   domain.Fresh([]domain.Demand{waiting, head}, oracleNow),
		Instances: domain.Fresh(issue226Live(cfg), oracleNow), Host: host})

	admitted := spawnedKeys(plan)
	if len(admitted) != 1 || admitted[0] != waiting.Key {
		t.Fatalf("FINDING 7 has regressed: a head its repository cap holds out must lend the vector "+
			"it cannot use to the `large` it outranks; admitted %v", admitted)
	}
	if plan.Next.Reservation == nil || plan.Next.Reservation.Demand != head.Key {
		t.Fatalf("FINDING 7's fix must not cost the head its place in line: %#v", plan.Next.Reservation)
	}
}

// TestFinding7SweepReportsNothingOnItsOwnSeed is the other half of the
// regression, and the half the one-tick test cannot give: the whole seed-67
// history of the container-node arm, with `overrun_job` armed, must now report
// NOTHING at all -- not a tolerated signature, not an unsignatured wedge.
//
// It is the test that would have caught the retirement being wrong. While the
// defect was live this seed reported a wedge under
// `sigReservedHeadHeldByARepositoryCap`; after the oracle was blinded it
// reported nothing, which looked identical to fixed. It is only identical from
// the harness's side, which is why the ADMISSION above is the primary assertion
// and this is the confirmation.
func TestFinding7SweepReportsNothingOnItsOwnSeed(t *testing.T) {
	t.Parallel()
	cfg := containerNodeWorld()
	w := newWorld(t, cfg, generateTrace(67, 200, cfg))
	defer w.close()

	if findings := w.run(); len(findings) > 0 {
		t.Fatalf("seed 67 of the container-node arm must be clean under ADR 0038: %s", findings[0])
	}
	if _, met := w.known[sigReservedHeadHeldByARepositoryCap+string(findingWedge)]; met {
		t.Fatal("FINDING 7 is fixed, so its signature must never be MET again — if this fires, the " +
			"cap-held head is sterilizing the residual once more")
	}
}
