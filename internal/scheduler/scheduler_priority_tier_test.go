package scheduler

import (
	"reflect"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// tiered stamps a demand with the tier its classification put it in. Rank is
// the only thing the planner reads; the name exists for operators.
func tiered(d domain.Demand, tier string, rank int) domain.Demand {
	d.Priority = domain.Priority{Tier: tier, Rank: rank}
	return d
}

// TestTheIncidentOf20260809 is the reason this feature exists. Two demands, one
// free builder slot: an App Store release that has waited an hour and five
// minutes, and a pull request's E2E build sixteen minutes older. Aged FIFO gave
// the slot to the E2E build. With a release tier declared, the release takes it.
func TestTheIncidentOf20260809(t *testing.T) {
	release := tiered(demand("vitalyiegorov/suuudokuuu", 1, 65*time.Minute, "builder"), "release", 1)
	e2e := demand("budgie-at/budgie", 2, 81*time.Minute, "builder")
	in := input([]domain.Demand{release, e2e}, nil, State{})
	in.Config.PriorityEscalation = 30 * time.Minute

	if got := spawnedKeys(PlanTick(in)); !reflect.DeepEqual(got, []domain.DemandKey{release.Key}) {
		t.Fatalf("builder slot went to %#v, want the release %#v", got, release.Key)
	}
}

// TestAgedFifoIsUnchangedWhenNoTierIsDeclared is the constraint the whole change
// is bounded by: an undeclared policy leaves every demand in the default tier
// and the order is the aged FIFO it always was.
func TestAgedFifoIsUnchangedWhenNoTierIsDeclared(t *testing.T) {
	young := demand("a/repo", 1, time.Minute, "builder")
	old := demand("b/repo", 2, 81*time.Minute, "builder")
	in := input([]domain.Demand{young, old}, nil, State{})

	if got := spawnedKeys(PlanTick(in)); !reflect.DeepEqual(got, []domain.DemandKey{old.Key}) {
		t.Fatalf("undeclared policy = %#v, want the older demand %#v", got, old.Key)
	}
}

// TestTierOrderIsRespectedWhenVectorsAreEqual is the first new property: with
// identical resource vectors and no escalation in play, the higher tier is
// planned first regardless of the order the demands were observed in.
func TestTierOrderIsRespectedWhenVectorsAreEqual(t *testing.T) {
	high := tiered(demand("a/repo", 1, time.Minute, "small"), "release", 2)
	middle := tiered(demand("b/repo", 2, 2*time.Minute, "small"), "main", 1)
	low := demand("c/repo", 3, 3*time.Minute, "small")
	want := []domain.DemandKey{high.Key, middle.Key, low.Key}

	for _, observed := range [][]domain.Demand{{high, middle, low}, {low, middle, high}, {middle, low, high}} {
		in := input(observed, nil, State{})
		in.Config.PriorityEscalation = time.Hour
		in.Config.LinuxCapacity = domain.Resources{CPU: 3, MemoryMB: 6_144, Slots: 3}
		in.Host = domain.Fresh(domain.Host{Available: in.Config.LinuxCapacity}, testNow)
		if got := spawnedKeys(PlanTick(in)); !reflect.DeepEqual(got, want) {
			t.Fatalf("tier order = %#v, want %#v", got, want)
		}
	}
}

// TestAHighTierDemandWaitsForTheNextFreeSlot pins the blast radius: this change
// reorders a queue, it never kills a job in flight.
func TestAHighTierDemandWaitsForTheNextFreeSlot(t *testing.T) {
	release := tiered(demand("a/repo", 1, time.Minute, "builder"), "release", 1)
	in := input([]domain.Demand{release}, []domain.Instance{runningInstance(testConfig(), "gha-macos-1", "b/repo", "builder")}, State{})
	in.Config.PriorityEscalation = 30 * time.Minute

	plan := PlanTick(in)
	for _, operation := range plan.Operations {
		if operation.Kind == OperationDrain {
			t.Fatalf("a release tier preempted a running instance: %#v", operation)
		}
	}
}

// TestEscalationIsMonotonic is the third new property. Effective rank never
// falls as a demand waits, and it crosses each declared tier exactly once.
func TestEscalationIsMonotonic(t *testing.T) {
	config := testConfig()
	config.PriorityEscalation = 10 * time.Minute
	waiter := demand("a/repo", 1, 0, "small")
	previous := -1
	for minutes := range 200 {
		now := waiter.CreatedAt.Add(time.Duration(minutes) * time.Minute)
		rank := effectiveTier(now, waiter, config)
		if rank < previous {
			t.Fatalf("effective tier fell from %d to %d after %d minutes", previous, rank, minutes)
		}
		if want := minutes / 10; rank != want {
			t.Fatalf("effective tier after %d minutes = %d, want %d", minutes, rank, want)
		}
		previous = rank
	}
}

// TestEscalationIsInertWithoutAThreshold keeps the feature switchable: with no
// escalation configured a demand's rank is exactly the one it was classified
// with, which is what makes an undeclared policy a no-op.
func TestEscalationIsInertWithoutAThreshold(t *testing.T) {
	config := testConfig()
	waiter := tiered(demand("a/repo", 1, 10*time.Hour, "small"), "release", 2)
	if got := effectiveTier(testNow, waiter, config); got != 2 {
		t.Fatalf("effective tier without a threshold = %d, want the classified rank 2", got)
	}
	undated := waiter
	undated.CreatedAt = time.Time{}
	config.PriorityEscalation = time.Minute
	if got := effectiveTier(testNow, undated, config); got != 2 {
		t.Fatalf("effective tier of an undated demand = %d, want the classified rank 2", got)
	}
}

// TestEscalationLetsADefaultTierDemandOvertakeABacklogOfReleases is the
// starvation guard stated as an ordering fact. Both demands are aged, so the
// aged band cannot separate them and only the tier can; the waiter wins because
// ninety-five minutes of escalation have lifted it to the release's own rank,
// where being older decides.
func TestEscalationLetsADefaultTierDemandOvertakeABacklogOfReleases(t *testing.T) {
	waiter := demand("a/repo", 1, 95*time.Minute, "builder")
	release := tiered(demand("b/repo", 2, 6*time.Minute, "builder"), "release", 3)
	in := input([]domain.Demand{release, waiter}, nil, State{})
	in.Config.PriorityEscalation = 30 * time.Minute

	if got := spawnedKeys(PlanTick(in)); !reflect.DeepEqual(got, []domain.DemandKey{waiter.Key}) {
		t.Fatalf("escalated order = %#v, want the starved demand %#v", got, waiter.Key)
	}
	// Without escalation the same queue hands the slot to the release forever.
	in.Config.PriorityEscalation = 0
	if got := spawnedKeys(PlanTick(in)); !reflect.DeepEqual(got, []domain.DemandKey{release.Key}) {
		t.Fatalf("unescalated order = %#v, want the release %#v", got, release.Key)
	}
}

// TestAgingRemainsTheOutermostKey pins the band structure a tier lives inside.
// ADR 0004 calls aging the absolute starvation guard, and a declared tier
// decides between demands that have waited comparably -- not between a fresh job
// and one that is already past the fairness age.
func TestAgingRemainsTheOutermostKey(t *testing.T) {
	freshRelease := tiered(demand("a/repo", 1, time.Minute, "builder"), "release", 2)
	agedStandard := demand("b/repo", 2, 10*time.Minute, "builder")
	in := input([]domain.Demand{freshRelease, agedStandard}, nil, State{})
	in.Config.PriorityEscalation = time.Hour

	if got := spawnedKeys(PlanTick(in)); !reflect.DeepEqual(got, []domain.DemandKey{agedStandard.Key}) {
		t.Fatalf("band order = %#v, want the aged demand %#v", got, agedStandard.Key)
	}
}

// TestAReservationYieldsToATierThatEscalatedAboveIt is the defect the simulator
// found on the tiered arm's seed 1. A reservation is derived from the aged
// band's head, so the test that decides whether it still heads the queue has to
// be the aged band's own rule. Comparing ages alone kept obeying a reservation
// the tier order had already overtaken -- the 2026-08-09 incident returning
// through the mechanism that exists to protect the head.
func TestAReservationYieldsToATierThatEscalatedAboveIt(t *testing.T) {
	reserved := demand("c/repo", 1, 8*time.Minute, "large")
	release := tiered(demand("a/repo", 2, 6*time.Minute, "large"), "release", 1)
	prior := State{Reservation: &domain.Reservation{Demand: reserved.Key, Profile: reserved.Profile,
		Resources: testConfig().Profiles["large"].Resources, Since: testNow.Add(-time.Minute)}}
	in := input([]domain.Demand{reserved, release}, nil, prior)
	in.Config.PriorityEscalation = 5 * time.Minute
	in.Config.LinuxCapacity = domain.Resources{CPU: 4, MemoryMB: 8_192, Slots: 1}
	in.Host = domain.Fresh(domain.Host{Available: in.Config.LinuxCapacity}, testNow)

	if got := spawnedKeys(PlanTick(in)); !reflect.DeepEqual(got, []domain.DemandKey{release.Key}) {
		t.Fatalf("reserved head = %#v, want the escalated release %#v", got, release.Key)
	}
}

// TestAReservationSurvivesEqualTierWork keeps the ADR 0017 contract intact where
// no tier separates the two: the older reserved head still wins its vector.
func TestAReservationSurvivesEqualTierWork(t *testing.T) {
	reserved := demand("c/repo", 1, 8*time.Minute, "large")
	other := demand("a/repo", 2, 6*time.Minute, "large")
	prior := State{Reservation: &domain.Reservation{Demand: reserved.Key, Profile: reserved.Profile,
		Resources: testConfig().Profiles["large"].Resources, Since: testNow.Add(-time.Minute)}}
	in := input([]domain.Demand{reserved, other}, nil, prior)
	in.Config.PriorityEscalation = 5 * time.Minute
	in.Config.LinuxCapacity = domain.Resources{CPU: 4, MemoryMB: 8_192, Slots: 1}
	in.Host = domain.Fresh(domain.Host{Available: in.Config.LinuxCapacity}, testNow)

	if got := spawnedKeys(PlanTick(in)); !reflect.DeepEqual(got, []domain.DemandKey{reserved.Key}) {
		t.Fatalf("reserved head = %#v, want %#v", got, reserved.Key)
	}
}

// TestExactAdmissionCannotAdmitMoreLowTierWorkAheadOfAFeasibleHighTierDemand is
// the same rule inside exactSelect: count maximization is a throughput
// optimization and may never overrule the tier order (the shape of ADR 0031's
// FINDING 3, one level up).
func TestExactAdmissionCannotAdmitMoreLowTierWorkAheadOfAFeasibleHighTierDemand(t *testing.T) {
	release := tiered(demand("a/repo", 1, time.Minute, "large"), "release", 1)
	first := demand("b/repo", 2, 2*time.Minute, "small")
	second := demand("b/repo", 3, 3*time.Minute, "small")
	in := input([]domain.Demand{release, first, second}, nil, State{})
	in.Config.PriorityEscalation = time.Hour
	in.Config.LinuxCapacity = domain.Resources{CPU: 4, MemoryMB: 8_192, Slots: 4}
	in.Host = domain.Fresh(domain.Host{Available: in.Config.LinuxCapacity}, testNow)

	if got := spawnedKeys(PlanTick(in)); !reflect.DeepEqual(got, []domain.DemandKey{release.Key}) {
		t.Fatalf("exact admission = %#v, want only the release %#v", got, release.Key)
	}
}

// TestTierOrderIsIndependentOfObservationOrder guards determinism: the planner
// is pure, so two observation orders of one tiered queue must produce one plan.
func TestTierOrderIsIndependentOfObservationOrder(t *testing.T) {
	release := tiered(demand("a/repo", 1, time.Minute, "small"), "release", 1)
	standard := tiered(demand("b/repo", 2, time.Minute, "small"), "", 0)
	setUp := func(demands []domain.Demand) Input {
		in := input(demands, nil, State{})
		in.Config.PriorityEscalation = time.Hour
		return in
	}

	if !reflect.DeepEqual(PlanTick(setUp([]domain.Demand{release, standard})), PlanTick(setUp([]domain.Demand{standard, release}))) {
		t.Fatal("a tiered plan depends on observation order")
	}
}
