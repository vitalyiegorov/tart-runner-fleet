package scheduler

import (
	"slices"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// wideNodeInput is issue #393's node-b after its re-budget: a Linux-only node
// promised 20 CPU / 44 GiB with ten slots, more than the four every node used to
// be held to.
func wideNodeInput(demands []domain.Demand, instances []domain.Instance) Input {
	in := input(demands, instances, State{})
	in.Config.LinuxCapacity = domain.Resources{CPU: 20, MemoryMB: 45_056, Slots: 10}
	in.Config.HostBudget = domain.Resources{CPU: 20, MemoryMB: 45_056}
	in.Config.RepoCaps = map[string]int{"a/repo": 10, "b/repo": 10, "c/repo": 10}
	in.Host = domain.Fresh(domain.Host{Available: domain.Resources{CPU: 20, MemoryMB: 45_056, Slots: 10}}, testNow)
	return in
}

func mediumDemands(count int, age time.Duration) []domain.Demand {
	demands := make([]domain.Demand, 0, count)
	for i := range count {
		demands = append(demands, demand("a/repo", int64(i+1), age, "medium"))
	}
	return demands
}

// TestAWideNodeAdvertisesItsWholeEnvelope: a scale set is told how many guests
// the node can start (ADR 0073). The per-tick admission bound paces the
// planner; it is not the node's capacity, and advertising it as one hands work
// a node can start to a sibling that may not be able to.
func TestAWideNodeAdvertisesItsWholeEnvelope(t *testing.T) {
	got, ok := Headroom(wideNodeInput(nil, nil), "medium")
	if !ok || got != 10 {
		t.Fatalf("Headroom(medium) = %d, %v; want all ten 2x4 guests the idle envelope holds", got, ok)
	}
	plan := PlanTick(wideNodeInput(nil, nil))
	if plan.Envelope.Free.Slots != 10 || plan.Envelope.AgedFree.Slots != 10 {
		t.Fatalf("published envelope = %+v, want ten free slots in both", plan.Envelope)
	}
}

// TestAWideNodeFillsItsSlotsAFewPerTick: the exact allocator stays exhaustive,
// so one tick admits at most admissionsPerTick guests however many slots are
// free; the next ticks admit the rest. Young and aged work are paced alike.
func TestAWideNodeFillsItsSlotsAFewPerTick(t *testing.T) {
	for name, age := range map[string]time.Duration{"young": time.Minute, "aged": time.Hour} {
		t.Run(name, func(t *testing.T) {
			waiting := mediumDemands(12, age)
			var instances []domain.Instance
			var perTick []int
			prior := State{}
			for tick := range 4 {
				in := wideNodeInput(waiting, instances)
				in.Prior = prior
				plan := PlanTick(in)
				prior = plan.Next
				// The pace is not a capacity: a tick that has admitted its four while
				// slots remain free holds no reservation, because nothing refused the
				// next head but the pace.
				if tick < 2 && prior.Reservation != nil {
					t.Fatalf("tick %d minted a reservation for %v with free slots left", tick, prior.Reservation.Demand)
				}
				spawned := spawnedKeys(plan)
				perTick = append(perTick, len(spawned))
				for _, key := range spawned {
					instance := runningInstance(testConfig(), "vm-"+key.String(), key.Repo, "medium")
					instance.Demand = key
					instances = append(instances, instance)
					waiting = slices.DeleteFunc(waiting, func(d domain.Demand) bool { return d.Key == key })
				}
			}
			if want := []int{4, 4, 2, 0}; !slices.Equal(perTick, want) {
				t.Fatalf("spawns per tick = %v, want %v (ten slots, four per tick)", perTick, want)
			}
		})
	}
}

// TestExactSelectSearchesAtMostTheTickBound keeps the allocator's cost where it
// was: an exhaustive search over N candidates is O(N^k), and k is the per-tick
// bound, never the node's slot count.
func TestExactSelectSearchesAtMostTheTickBound(t *testing.T) {
	in := wideNodeInput(nil, nil)
	in.Config.LinuxCapacity.Slots = 16
	candidates := make([]domain.Demand, 0, 60)
	for i := range 60 {
		candidates = append(candidates, demand("a/repo", int64(i+1), time.Minute, "small"))
	}
	free := domain.Resources{CPU: 64, MemoryMB: 262_144, Slots: 16}
	if got := exactSelect(testNow, candidates, free, nil, in.Config); len(got) != admissionsPerTick {
		t.Fatalf("exactSelect admitted %d, want the per-tick bound %d", len(got), admissionsPerTick)
	}
}
