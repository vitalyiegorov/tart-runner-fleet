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

// TestTakesTheReservedVectorIsTheNoJumpRuleItself holds ADR 0017's guarantee at
// the level of the predicate, so it survives whatever a future change decides a
// reservation should lend.
//
// The vector-axis rows are the ones worth reading. They are the proof that
// applying this filter on the axis where it can never bind costs nothing: a head
// that does not fit `free` overflows it in some dimension, so a candidate inside
// `free` is strictly smaller there and cannot contain the head's vector. Keeping
// the filter on both axes is what turns "by construction" into "by rule".
func TestTakesTheReservedVectorIsTheNoJumpRuleItself(t *testing.T) {
	cfg := capHeldConfig()
	reservation := &domain.Reservation{Demand: domain.DemandKey{Repo: "c/repo"}, Profile: "xl",
		Resources: cfg.Profiles["xl"].Resources}

	for _, test := range []struct {
		profile domain.ProfileID
		takes   bool
		why     string
	}{
		{"xl", true, "an equal vector is exactly the job ADR 0017 forbids from jumping the queue"},
		{"large", false, "4 CPU cannot hold a 6 CPU head"},
		{"medium", false, "2 CPU cannot hold a 6 CPU head"},
		{"small", false, "1 CPU cannot hold a 6 CPU head"},
		{"builder", true, "8 CPU / 12288 MB contains the head's 6 CPU / 12288 MB whole"},
		{"maestro", false, "4 CPU / 7168 MB is short on both terms"},
	} {
		demand := capHeldDemand(cfg, "a/repo", 1, time.Minute, test.profile)
		if got := takesTheReservedVector(cfg, reservation, demand); got != test.takes {
			t.Fatalf("%s (%v) takes the reserved %v = %v, want %v: %s", test.profile,
				cfg.Profiles[test.profile].Resources, reservation.Resources, got, test.takes, test.why)
		}
	}
}

// TestReservedHeadAtRepositoryCapReadsTheSameOccupancyFeasibleWill pins the
// other predicate against the one it must never disagree with. If this count
// ever diverged from `feasible`'s, the scheduler could lend a vector on the
// grounds that the cap holds the head while `feasible` was about to admit it.
func TestReservedHeadAtRepositoryCapReadsTheSameOccupancyFeasibleWill(t *testing.T) {
	cfg := capHeldConfig()
	reservation := &domain.Reservation{Demand: domain.DemandKey{Repo: "c/repo"}, Profile: "xl",
		Resources: cfg.Profiles["xl"].Resources}
	unconfigured := &domain.Reservation{Demand: domain.DemandKey{Repo: "z/repo"}, Profile: "xl",
		Resources: cfg.Profiles["xl"].Resources}

	for _, test := range []struct {
		name        string
		reservation *domain.Reservation
		occupied    map[string]int
		want        bool
	}{
		{"under cap", reservation, map[string]int{"c/repo": 1}, false},
		{"at cap", reservation, map[string]int{"c/repo": 2}, true},
		{"no live instance at all", reservation, map[string]int{}, false},
		// An unconfigured repository caps at one, exactly as `feasible` reads it
		// through the same `repoCapLimit`, so a single live instance holds it.
		{"an unconfigured repository caps at one", unconfigured, map[string]int{"z/repo": 1}, true},
	} {
		got := reservedHeadAtRepositoryCap(cfg, test.occupied, test.reservation)
		if got != test.want {
			t.Fatalf("%s: reservedHeadAtRepositoryCap = %v, want %v", test.name, got, test.want)
		}
		// The predicate is the head's own half of `feasible`: whenever it says
		// the cap holds the head, `feasible` must refuse the head on that term.
		admissible := feasible(test.reservation.Resources, cfg.LinuxCapacity,
			test.reservation.Demand.Repo, test.occupied, nil, cfg.RepoCaps)
		if got && admissible {
			t.Fatalf("%s: the cap predicate and feasible() disagree about the same occupancy", test.name)
		}
	}
}

// TestReservationAxisNamesWhichTermRefusedTheHead pins the diagnostic issue #226
// was invisible for want of.
//
// Nothing published on a live fleet named the held reservation, its repository,
// or which of `feasible`'s two terms was holding it, so a defect that stranded a
// vector for the whole runtime of a blocking job left no artifact at all — only
// a simulator found it. The axis is what an operator needs: a `vector` hold ends
// when live instances release, and a `repository_cap` hold ends only when one of
// the head's OWN repository's instances exits, which no amount of freed CPU can
// hasten.
//
// `none` is the row worth keeping. The planner cannot produce it, because a
// reservation is minted only where `feasible` is false — but it is the state an
// operator most needs named if it ever appears, since a fleet reserving a vector
// for work it could have started is issue #125's wedge wearing a reservation.
func TestReservationAxisNamesWhichTermRefusedTheHead(t *testing.T) {
	cfg := capHeldConfig()
	vector := cfg.Profiles["xl"].Resources
	roomy := domain.Resources{CPU: 12, MemoryMB: 32_768, Slots: 4}
	tight := domain.Resources{CPU: 2, MemoryMB: 4_096, Slots: 4}

	for _, test := range []struct {
		name     string
		agedFree domain.Resources
		occupied map[string]int
		want     ReservationAxis
	}{
		{"the envelope refuses it", tight, map[string]int{"c/repo": 0}, ReservationAxisVector},
		{"its repository cap refuses it", roomy, map[string]int{"c/repo": 2}, ReservationAxisRepositoryCap},
		{"both refuse it", tight, map[string]int{"c/repo": 2}, ReservationAxisBoth},
		{"neither refuses it", roomy, map[string]int{"c/repo": 0}, ReservationAxisNone},
	} {
		got := reservationAxis(cfg, vector, test.agedFree, "c/repo", test.occupied)
		if got != test.want {
			t.Fatalf("%s: reservationAxis = %q, want %q", test.name, got, test.want)
		}
	}
}

// TestPlanPublishesTheAxisHoldingItsReservation is the same diagnostic through
// `PlanTick`, which is the only way an operator ever sees it. Issue #226's own
// tick must publish `repository_cap`, because that is the fact whose absence
// made the defect unobservable in production.
func TestPlanPublishesTheAxisHoldingItsReservation(t *testing.T) {
	cfg := capHeldConfig()
	head := capHeldDemand(cfg, "c/repo", 9, 13*time.Minute, "xl")
	waiting := capHeldDemand(cfg, "a/repo", 10, 9*time.Minute+30*time.Second, "large")

	plan := PlanTick(capHeldInput(cfg, []domain.Demand{waiting, head}, issue226Occupancy(cfg), State{}))
	if plan.ReservationAxis != ReservationAxisRepositoryCap {
		t.Fatalf("issue #226's tick is held by the repository cap and must say so: %q", plan.ReservationAxis)
	}

	// The vector axis, same topology, with the cap opened and the envelope
	// closed instead: one `xl` live in another repository leaves too little for
	// a second one.
	roomy := capHeldConfig()
	roomy.RepoCaps["c/repo"] = 4
	vectorHeld := PlanTick(capHeldInput(roomy, []domain.Demand{waiting, head},
		[]domain.Instance{liveInstance(roomy, "trf-xl-1", "b/repo", "xl")}, State{}))
	if vectorHeld.ReservationAxis != ReservationAxisVector {
		t.Fatalf("a head no envelope can hold is held by the vector axis: %q", vectorHeld.ReservationAxis)
	}

	// A plan that decided nothing publishes no judgement: a stale observation
	// carries the prior reservation through without ever judging the head.
	blocked := capHeldInput(cfg, []domain.Demand{waiting, head}, issue226Occupancy(cfg),
		State{Reservation: &domain.Reservation{Demand: head.Key, Profile: head.Profile,
			Resources: cfg.Profiles["xl"].Resources, Since: testNow.Add(-time.Hour)}})
	blocked.Host = domain.Stale(blocked.Host.Value, testNow.Add(-time.Hour), "probe stale")
	if carried := PlanTick(blocked); carried.ReservationAxis != "" {
		t.Fatalf("a plan that judged nothing must publish no axis, got %q", carried.ReservationAxis)
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
