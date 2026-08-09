package scheduler

import (
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// The tests in this file pin ADR 0038: what a reserved head its own REPOSITORY
// CAP holds out may lend, and what it may never lend.
//
// `feasible` folds two terms — the vector fit and the repository cap — and
// `planLinux` holds the reservation when either one refuses the head. ADR 0017
// gave the vector term a release rule in 2026-07-25's incident: a head that does
// not fit cannot start until live instances release what it needs, whatever
// backfill does, so holding the residual idle protects nothing. The cap term
// never got that rule, although the rationale transfers word for word — a head
// at its repository's cap cannot start until one of that repository's own
// instances exits, whatever backfill does.
//
// The cost of the omission is the one ADR 0029 states in units: an idle vector
// the size of the starved profile, for the entire duration of the blocking job.
// Issue #226 is that state, and it reduces to a single tick.
//
// What replaces the withheld vector is NOT nothing. ADR 0017's Consequences
// promise that "anything admitted in this path is by construction too small to
// fit the reserved vector, so no equal-or-larger job can jump the queue". That
// construction is an ACCIDENT of the fit test: it holds automatically while the
// only capacity ever lent belongs to a head that does not fit `free`, because a
// candidate bounded by `free` must then be strictly smaller than the head in the
// dimension the head overflows. A cap-held head fits `free`, so the accident
// ends and the promise has to become a predicate. `takesTheReservedVector` is
// that predicate, and the tests below hold it directly rather than inferring it
// from arithmetic.

// capHeldConfig is issue #226's own topology, in this package's vocabulary: the
// twelve-core container node, `c/repo` capped at two, and the four Linux
// profiles of the simulated world.
func capHeldConfig() Config {
	cfg := testConfig()
	cfg.LinuxCapacity = domain.Resources{CPU: 12, MemoryMB: 32_768, Slots: 4}
	cfg.RepoCaps = map[string]int{"a/repo": 4, "b/repo": 4, "c/repo": 2}
	cfg.Profiles["xl"] = domain.Profile{ID: "xl", Platform: domain.PlatformLinux, Route: "tiered",
		Resources: domain.Resources{CPU: 6, MemoryMB: 12_288, Slots: 1}}
	return cfg
}

func capHeldDemand(cfg Config, repo string, jobID int64, age time.Duration, profile domain.ProfileID) domain.Demand {
	return domain.Demand{Key: domain.DemandKey{Repo: repo, RunID: 1000 + jobID, Attempt: 1, JobID: 500_000 + jobID},
		CreatedAt: testNow.Add(-age), Profile: profile, Route: cfg.Profiles[profile].Route,
		Platform: cfg.Profiles[profile].Platform, Event: domain.EventPullRequest}
}

func capHeldInput(cfg Config, demands []domain.Demand, instances []domain.Instance, prior State) Input {
	return Input{Now: testNow, Config: cfg, Demands: domain.Fresh(demands, testNow),
		Instances: domain.Fresh(instances, testNow),
		Host: domain.Fresh(domain.Host{Available: domain.Resources{CPU: 6, MemoryMB: 18_432, Slots: 4},
			Capacity: domain.Resources{CPU: 12, MemoryMB: 30_720, Slots: 4}}, testNow),
		Prior: prior}
}

// issue226Occupancy is `c/repo` at its cap of two, holding 6 CPU / 12288 MB.
func issue226Occupancy(cfg Config) []domain.Instance {
	return []domain.Instance{
		liveInstance(cfg, "trf-large-33754a03d97c06c1", "c/repo", "large"),
		liveInstance(cfg, "trf-medium-befa30d57c9b846b", "c/repo", "medium"),
	}
}

// TestCapHeldReservedHeadLendsItsVector is issue #226, reduced to one tick.
//
//	next reservation=&{c/repo/1009/1/500009 xl {6 12288 1} ...}
//	  demand   a/repo/1010/1/500010 profile=large age=9m30s
//	  demand   c/repo/1009/1/500009 profile=xl    age=13m0s
//	  instance trf-large-…  repo=c/repo profile=large  state=assigned
//	  instance trf-medium-… repo=c/repo profile=medium state=running
//	host available={CPU:6 MemoryMB:18432 Slots:4}
//
// The head is the oldest demand and is refused on the cap term, so `planLinux`
// holds a reservation for it. `safeBackfill` then subtracted the head's whole
// {6, 12288, 1} from a {6, 18432, 2} envelope and offered `a/repo`'s `large` the
// {0, 6144, 1} that was left. The `large` needs {4, 8192, 1} and fits `free`
// outright, and `a/repo` has no live instance at all.
//
// Both directions of the prior state are pinned, because the issue's own caveat
// claimed the wedge was a cross-tick composition rather than a property of one
// tick's inputs. It is not: the reservation is minted on this tick from nothing
// just as readily as it is carried in.
func TestCapHeldReservedHeadLendsItsVector(t *testing.T) {
	cfg := capHeldConfig()
	head := capHeldDemand(cfg, "c/repo", 9, 13*time.Minute, "xl")
	waiting := capHeldDemand(cfg, "a/repo", 10, 9*time.Minute+30*time.Second, "large")

	for _, test := range []struct {
		name  string
		prior State
	}{
		{"the reservation carried in", State{Reservation: &domain.Reservation{Demand: head.Key,
			Profile: head.Profile, Resources: cfg.Profiles["xl"].Resources, Since: testNow.Add(-time.Hour)}}},
		{"no prior reservation at all", State{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan := PlanTick(capHeldInput(cfg, []domain.Demand{waiting, head}, issue226Occupancy(cfg), test.prior))

			spawned := spawnedKeys(plan)
			if len(spawned) != 1 || spawned[0] != waiting.Key {
				t.Fatalf("a head its repository cap holds out cannot use the vector it reserves: it "+
					"must lend it to the `large` it outranks, got %v", spawned)
			}
			if plan.Next.Reservation == nil || plan.Next.Reservation.Demand != head.Key {
				t.Fatalf("lending the vector must not drop the reservation — the head is still first "+
					"in line for the cap slot it waits on, got %#v", plan.Next.Reservation)
			}
			if containsDrain(plan.Operations) {
				t.Fatalf("backfill must never drain to make room: %#v", plan.Operations)
			}
		})
	}
}

// TestCapHeldReservedHeadRefusesAPeerThatCouldTakeItsVector is ADR 0017's
// no-jump guarantee, held directly instead of inherited.
//
// The same tick, with two demands waiting: a `medium` the head outranks and an
// `xl` peer whose vector is EQUAL to the head's. `free` fits both, because
// `free` fits the head. Admitting the peer would hand the oldest aged demand's
// exact vector to a younger one, which is the aged-FIFO inversion the whole
// reservation mechanism exists to prevent.
//
// Before ADR 0038 this test would pass for the wrong reason — the residual was
// too small for either demand, so nothing was admitted and the guarantee was
// never exercised. Asserting the `medium` IS admitted on the same tick is what
// makes the refusal of the peer mean something.
func TestCapHeldReservedHeadRefusesAPeerThatCouldTakeItsVector(t *testing.T) {
	cfg := capHeldConfig()
	head := capHeldDemand(cfg, "c/repo", 9, 13*time.Minute, "xl")
	peer := capHeldDemand(cfg, "a/repo", 10, 9*time.Minute, "xl")
	smaller := capHeldDemand(cfg, "b/repo", 11, 8*time.Minute, "medium")

	plan := PlanTick(capHeldInput(cfg, []domain.Demand{peer, smaller, head}, issue226Occupancy(cfg), State{}))

	spawned := spawnedKeys(plan)
	if containsDemandKey(spawned, peer.Key) {
		t.Fatalf("ADR 0017: nothing equal-or-larger may jump the reserved head's queue position, "+
			"and a cap-held head's lent vector is where that guarantee stops being automatic: %v", spawned)
	}
	if !containsDemandKey(spawned, smaller.Key) {
		t.Fatalf("the refusal above must be the no-jump rule and not an empty envelope: the `medium` "+
			"the head outranks has to be admitted on this very tick, got %v", spawned)
	}
}

// containsDemandKey reports whether a spawn list names one demand.
func containsDemandKey(keys []domain.DemandKey, want domain.DemandKey) bool {
	for _, key := range keys {
		if key == want {
			return true
		}
	}
	return false
}
