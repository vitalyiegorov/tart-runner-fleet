package scheduler

import (
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// The 2026-08-09 production incident (issue #223). Instance
// trf-xl-05bbe1c83f21fcd6 held 6 CPU and 12288 MB -- sixty percent of the Mac
// mini's ten-core envelope -- from 18:07:28Z to 19:22:50Z for job 93275690093
// of rnw-community/rnw-community. The job had FAILED at 18:09:12Z, a hundred
// seconds in; what ran for the remaining seventy-three minutes was an
// `if: always()` cleanup step waiting on an emulator that never booted.
//
// GitHub reported the job in_progress the whole time, and it genuinely was, so
// every existing reclaim gate was correct to leave it alone: the VM was powered
// on, the runner was busy, and the bound demand carried an active job. Nothing
// in the fleet measured how long one instance had held its vector, so nothing
// could see that three maestro jobs and two store-release builds were queued
// behind a dead job on the same host with four cores idle.
//
// These tests pin the missing bound in both directions: the budget must reclaim
// the vector when it is exceeded, and it must never touch a healthy job inside
// it.

// occupancyProfiles is the incident's shape: a 6 CPU / 12288 MB profile with a
// wall-clock ceiling on how long one of its instances may hold that vector.
func occupancyProfiles(budget time.Duration) map[domain.ProfileID]domain.Profile {
	profiles := testConfig().Profiles
	profiles["xl"] = domain.Profile{ID: "xl", Platform: domain.PlatformLinux, Route: "tiered",
		Resources: domain.Resources{CPU: 6, MemoryMB: 12_288, Slots: 1}, OccupancyBudget: budget}
	return profiles
}

// occupant is the incident's instance: powered on, running, executing a job
// GitHub still calls in_progress, holding its vector since `held` ago.
func occupant(profile domain.ProfileID, profiles map[domain.ProfileID]domain.Profile, held time.Duration) domain.Instance {
	config := profiles[profile]
	return domain.Instance{
		ID: "trf-xl-05bbe1c83f21fcd6", Repo: "rnw-community/rnw-community",
		Demand:    domain.DemandKey{Repo: "rnw-community/rnw-community", RunID: 31_325_708_527, Attempt: 1, JobID: 93_275_690_093},
		Platform:  config.Platform,
		Profile:   profile,
		Route:     config.Route,
		Resources: config.Resources,
		State:     domain.InstanceRunning,
		Power:     domain.InstancePowerRunning,
		// The bound demand carries an ACTIVE job, so the lingering-runner gate
		// cannot fire. That is the whole point: this is a busy runner.
		JobInactive:   false,
		RunningSince:  testNow.Add(-held),
		OccupiedSince: testNow.Add(-held),
	}
}

func occupancyInput(instances []domain.Instance, demands []domain.Demand, budget time.Duration) Input {
	in := input(demands, instances, State{})
	in.Config.Profiles = occupancyProfiles(budget)
	return in
}

func drainOf(plan Plan, id string) (Operation, bool) {
	for _, operation := range plan.Operations {
		if operation.Kind == OperationDrain && operation.Instance == id {
			return operation, true
		}
	}
	return Operation{}, false
}

func TestOccupancyBudgetReclaimsAVectorHeldPastItsCeiling(t *testing.T) {
	const budget = 45 * time.Minute
	held := occupant("xl", occupancyProfiles(budget), 75*time.Minute)
	// The starved queue of the incident: maestro jobs and the owner's store
	// release, all of which fit inside the vector the dead job is holding.
	queued := []domain.Demand{
		demand("rnw-repo/maestro", 1, 85*time.Minute, "medium"),
		demand("sudoku-repo/builder", 2, 80*time.Minute, "medium"),
	}

	plan := PlanTick(occupancyInput([]domain.Instance{held}, queued, budget))

	operation, ok := drainOf(plan, held.ID)
	if !ok {
		t.Fatalf("an instance %s past its %s occupancy budget must be drained; plan operations: %#v",
			75*time.Minute, budget, plan.Operations)
	}
	if !operation.Recovery || !operation.OccupancyExceeded {
		t.Fatalf("the drain must name the occupancy budget as its cause; got %#v", operation)
	}
	if operation.Demand != held.Demand {
		t.Fatalf("the drain must name the job it cuts (%v); got %v", held.Demand, operation.Demand)
	}
	// The cause is part of the content-addressed identity, so a budget reap and a
	// lingering-runner reap of the same instance are distinct attempts.
	lingering := Operation{Kind: OperationDrain, Instance: held.ID, Profile: held.Profile, Route: held.Route,
		Demand: held.Demand, Recovery: true, LingeringRunner: true}
	if operation.ID == stableID("op", lingering) {
		t.Fatalf("the occupancy cause must change the operation identity; got %s", operation.ID)
	}
}

func TestOccupancyBudgetNeverCutsAHealthyJobInsideIt(t *testing.T) {
	const budget = 45 * time.Minute
	// Budget minus one second. A job legitimately running for forty-four minutes
	// and fifty-nine seconds -- a macOS builder doing an App Store archive is
	// exactly this -- must not be touched.
	for _, held := range []time.Duration{budget - time.Second, budget / 2, time.Second} {
		instance := occupant("xl", occupancyProfiles(budget), held)
		plan := PlanTick(occupancyInput([]domain.Instance{instance}, []domain.Demand{demand("b/repo", 3, time.Hour, "medium")}, budget))
		if _, drained := drainOf(plan, instance.ID); drained {
			t.Fatalf("an instance %s into a %s budget must not be drained; plan operations: %#v", held, budget, plan.Operations)
		}
	}
}

func TestOccupancyBudgetIsFailClosedWithoutEvidenceOrACeiling(t *testing.T) {
	const budget = 45 * time.Minute
	for _, test := range []struct {
		name    string
		budget  time.Duration
		mutate  func(*domain.Instance)
		profile domain.ProfileID
	}{
		{name: "no configured ceiling", budget: 0, mutate: func(*domain.Instance) {}, profile: "xl"},
		{name: "unknown occupancy start", budget: budget, profile: "xl",
			mutate: func(i *domain.Instance) { i.OccupiedSince = time.Time{} }},
		{name: "occupancy start in the future", budget: budget, profile: "xl",
			mutate: func(i *domain.Instance) { i.OccupiedSince = testNow.Add(time.Hour) }},
		{name: "power unread", budget: budget, profile: "xl",
			mutate: func(i *domain.Instance) { i.Power = domain.InstancePowerUnknown }},
	} {
		t.Run(test.name, func(t *testing.T) {
			instance := occupant(test.profile, occupancyProfiles(test.budget), 75*time.Minute)
			test.mutate(&instance)
			plan := PlanTick(occupancyInput([]domain.Instance{instance}, nil, test.budget))
			if _, drained := drainOf(plan, instance.ID); drained {
				t.Fatalf("%s must not authorize a reclaim; plan operations: %#v", test.name, plan.Operations)
			}
		})
	}
}

// An over-budget instance is exactly the condition an operator must be able to
// see BEFORE the reap, and the one that turns a slow job into a fleet incident
// only when queued work would fit the vector it is holding.
func TestOccupancyReportsNameTheHoldAndTheStarvation(t *testing.T) {
	const budget = 45 * time.Minute
	profiles := occupancyProfiles(budget)
	over := occupant("xl", profiles, 75*time.Minute)
	warned := occupant("xl", profiles, 40*time.Minute)
	warned.ID = "trf-xl-warned"
	healthy := occupant("xl", profiles, time.Minute)
	healthy.ID = "trf-xl-healthy"

	in := occupancyInput([]domain.Instance{over, warned, healthy},
		[]domain.Demand{demand("rnw-repo/maestro", 1, 85*time.Minute, "medium")}, budget)
	reports := Occupancies(in.Now, in.Config, in.Instances.Value, in.Demands.Value)

	byID := map[string]Occupancy{}
	for _, report := range reports {
		byID[report.Instance] = report
	}
	if len(byID) != 3 {
		t.Fatalf("every live instance holding a vector must be reported; got %#v", reports)
	}
	if report := byID[over.ID]; !report.OverBudget || !report.Warned || !report.StarvesQueuedDemand ||
		report.Age != 75*time.Minute || report.Budget != budget {
		t.Fatalf("the over-budget hold must be reported with its age, budget, and starvation; got %#v", report)
	}
	if report := byID[warned.ID]; report.OverBudget || !report.Warned {
		t.Fatalf("an instance past the warning fraction but inside its budget must warn, not breach; got %#v", report)
	}
	if report := byID[healthy.ID]; report.OverBudget || report.Warned {
		t.Fatalf("a young instance must be reported quietly; got %#v", report)
	}
}
