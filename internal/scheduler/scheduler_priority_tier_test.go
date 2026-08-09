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

// TestEscalationLetsADefaultTierDemandOvertakeAStreamOfReleases is the
// starvation guard stated as an ordering fact: once the waiter is older than the
// release by the tier gap times the threshold, it is planned first.
func TestEscalationLetsADefaultTierDemandOvertakeAStreamOfReleases(t *testing.T) {
	waiter := demand("a/repo", 1, 95*time.Minute, "builder")
	release := tiered(demand("b/repo", 2, time.Minute, "builder"), "release", 3)
	in := input([]domain.Demand{release, waiter}, nil, State{})
	in.Config.PriorityEscalation = 30 * time.Minute

	if got := spawnedKeys(PlanTick(in)); !reflect.DeepEqual(got, []domain.DemandKey{waiter.Key}) {
		t.Fatalf("escalated order = %#v, want the starved demand %#v", got, waiter.Key)
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
