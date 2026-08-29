package scheduler

import (
	"reflect"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// 2026-08-02 incident, ~13:50Z on the production Mac mini (Mac16,10: 10 cores,
// 24 GiB). A live `xl` Linux VM (6 CPU / 12288 MB) had been busy for more than
// an hour. The aged global-FIFO head was a second `xl` that cannot fit beside it
// (6 + 6 > 10), so `planLinux` correctly held its reservation. A queued macOS
// `maestro` (4 CPU / 7168 MB) fits the four free cores EXACTLY, and was refused
// every tick for more than an hour, because `fillMacRemainder` returned early
// whenever `plan.Next.Reservation != nil` — regardless of whether the reserved
// head could use the vector it was refusing to lend.
//
// That is the asymmetry PR #112 reported and deferred: ADR 0017 already decided
// the same question for `safeBackfill` on the Linux queue. A head that does not
// fit the free envelope AT ALL is blocked by live instances holding what it
// needs, not by the remainder pass, so holding the residual idle protects
// nothing and only costs the fleet an hour of an idle machine.

// reservedRemainderConfig is the live topology: the ten physical cores and
// 24 GiB of the Mac mini as the shared envelope, mixed-platform admission on
// (as in production), and the live vectors of the three profiles involved —
// Linux `xl` 6 CPU / 12288 MB, macOS `builder` 6 CPU / 12288 MB, and macOS
// `maestro` 4 CPU / 7168 MB.
func reservedRemainderConfig() Config {
	cfg := testConfig()
	cfg.LinuxCapacity = domain.Resources{CPU: 10, MemoryMB: 24_576, Slots: 4}
	cfg.MixedPlatformAdmission = true
	cfg.RepoCaps = map[string]int{"a/repo": 4, "b/repo": 4, "mac-a": 2}
	cfg.Profiles["xl"] = domain.Profile{ID: "xl", Platform: domain.PlatformLinux, Route: "tiered",
		Resources: domain.Resources{CPU: 6, MemoryMB: 12_288, Slots: 1}}
	builder := cfg.Profiles["builder"]
	builder.Resources = domain.Resources{CPU: 6, MemoryMB: 12_288, Slots: 1}
	cfg.Profiles["builder"] = builder
	return cfg
}

func reservedRemainderInput(cfg Config, demands []domain.Demand, instances []domain.Instance, prior State) Input {
	return Input{Now: testNow, Config: cfg, Demands: domain.Fresh(demands, testNow),
		Instances: domain.Fresh(instances, testNow),
		Host:      domain.Fresh(domain.Host{Available: domain.Resources{CPU: 10, MemoryMB: 24_576, Slots: 4}}, testNow),
		Prior:     prior}
}

// profileDemand is demand() against a caller-supplied config, so profiles the
// default testConfig() does not carry (`xl`) can be queued.
func profileDemand(cfg Config, repo string, jobID int64, age time.Duration, profile domain.ProfileID) domain.Demand {
	return domain.Demand{Key: domain.DemandKey{Repo: repo, RunID: 100, Attempt: 1, JobID: jobID},
		CreatedAt: testNow.Add(-age), Profile: profile, Route: cfg.Profiles[profile].Route,
		Platform: cfg.Profiles[profile].Platform, Event: domain.EventPullRequest}
}

func liveInstance(cfg Config, id, repo string, profile domain.ProfileID) domain.Instance {
	return domain.Instance{ID: id, Repo: repo, Platform: cfg.Profiles[profile].Platform, Profile: profile,
		Route: cfg.Profiles[profile].Route, Resources: cfg.Profiles[profile].Resources,
		State: domain.InstanceRunning, Power: domain.InstancePowerRunning}
}

func spawnsProfile(plan Plan, profile domain.ProfileID) bool {
	for _, operation := range plan.Operations {
		if operation.Kind == OperationSpawn && operation.Profile == profile {
			return true
		}
	}
	return false
}

// TestMacRemainderBehindAReservedLinuxHead is the incident replay plus the two
// guard cases that keep the reservation contract intact. Every case holds a
// reservation for the aged Linux head; they differ only in whether that head can
// use the vector the macOS pass wants.
//
//   - infeasible head: the head does not fit the free envelope at all, so it is
//     waiting on the busy `xl` to exit, not on this pass. Admitting the maestro
//     cannot make the head wait for anything it was not already waiting for.
//   - feasible head, macOS demand larger than the remainder: the head fits the
//     free envelope and is held back only by its repository cap, which clears
//     when a live instance of that repository finishes. Admitting a 6 CPU
//     builder would leave 3 free cores where the head needs 6, so the pass must
//     refuse — this is the case the blanket early return got right, and the one
//     a naive "just drop the guard" fix would break.
//   - feasible head, macOS demand inside the remainder: the head's whole vector
//     is withheld and what is left over is still put to work.
func TestMacRemainderBehindAReservedLinuxHead(t *testing.T) {
	for _, test := range []struct {
		name        string
		headProfile domain.ProfileID
		headRepoCap int
		blocker     domain.ProfileID
		mac         domain.ProfileID
		wantMac     bool
	}{
		{
			name:        "infeasible head does not strand the free vector",
			headProfile: "xl", headRepoCap: 4, blocker: "xl", mac: "maestro", wantMac: true,
		},
		{
			name:        "feasible head keeps its whole vector withheld",
			headProfile: "xl", headRepoCap: 1, blocker: "small", mac: "builder", wantMac: false,
		},
		{
			name:        "feasible head still lends what it does not need",
			headProfile: "large", headRepoCap: 1, blocker: "small", mac: "maestro", wantMac: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := reservedRemainderConfig()
			cfg.RepoCaps["b/repo"] = test.headRepoCap
			head := profileDemand(cfg, "b/repo", 1, 65*time.Minute, test.headProfile)
			mac := profileDemand(cfg, "mac-a", 2, 60*time.Minute, test.mac)
			// The blocker is busy in the repository whose cap the head needs when
			// the head is meant to be feasible-but-blocked, and in another
			// repository when the head is meant to be resource-infeasible.
			blockerRepo := "b/repo"
			if test.headRepoCap > 1 {
				blockerRepo = "a/repo"
			}
			instances := []domain.Instance{liveInstance(cfg, "linux-1", blockerRepo, test.blocker)}

			plan := PlanTick(reservedRemainderInput(cfg, []domain.Demand{head, mac}, instances, State{}))

			if plan.Next.Reservation == nil || plan.Next.Reservation.Demand != head.Key {
				t.Fatalf("the aged head must hold its reservation, got %#v", plan.Next.Reservation)
			}
			if containsDrain(plan.Operations) {
				t.Fatalf("a remainder pass must never drain to make room: %#v", plan.Operations)
			}
			if spawnsProfile(plan, test.headProfile) {
				t.Fatalf("the reserved head cannot fit and must not spawn: %#v", plan.Operations)
			}
			if got := spawnsProfile(plan, test.mac); got != test.wantMac {
				free := linuxFree(reservedRemainderInput(cfg, []domain.Demand{head, mac}, instances, State{}))
				t.Fatalf("macOS %s admitted = %v, want %v (free %#v, reserved %#v): %#v",
					test.mac, got, test.wantMac, free, plan.Next.Reservation.Resources, plan.Operations)
			}
		})
	}
}

// TestRefusedRemainderIsWhatLetsTheReservedHeadStart proves the guard direction
// is load-bearing rather than merely conservative, by playing the counterfactual
// out. The head fits the free envelope and is blocked only by its repository
// cap. When the pass refuses the 6 CPU builder, the head starts on the very tick
// its blocker exits. Had the pass admitted it — as a naive "drop the reservation
// check" fix would — the host would have been left with 4 cores where the head
// needs 6, so the head would have waited for the whole builder job.
func TestRefusedRemainderIsWhatLetsTheReservedHeadStart(t *testing.T) {
	cfg := reservedRemainderConfig()
	cfg.RepoCaps["b/repo"] = 1
	head := profileDemand(cfg, "b/repo", 1, 65*time.Minute, "xl")
	builder := profileDemand(cfg, "mac-a", 2, 60*time.Minute, "builder")
	blocker := liveInstance(cfg, "linux-small-1", "b/repo", "small")

	first := PlanTick(reservedRemainderInput(cfg, []domain.Demand{head, builder}, []domain.Instance{blocker}, State{}))
	if containsSpawn(first.Operations) {
		t.Fatalf("the reserved head's own vector must not be lent out: %#v", first.Operations)
	}

	// The cap blocker finishes. Nothing else took the vector, so the head starts.
	second := PlanTick(reservedRemainderInput(cfg, []domain.Demand{head, builder}, nil, first.Next))
	if !spawnsProfile(second, "xl") {
		t.Fatalf("the reserved head must start as soon as its cap clears: %#v", second.Operations)
	}

	// The counterfactual: the same tick with the builder live instead.
	admitted := reservedRemainderInput(cfg, []domain.Demand{head}, []domain.Instance{
		liveInstance(cfg, "mac-builder-1", "mac-a", "builder")}, first.Next)
	if free := linuxFree(admitted); free.CanFit(cfg.Profiles["xl"].Resources) {
		t.Fatalf("counterfactual is not a delay: %#v still fits the head, so this test proves nothing", free)
	}
}

// TestBoundedHandoffWaveHonoursTheReservation covers the other pass that admits
// macOS work while a Linux reservation may be held: the one-shot backfill wave
// inside planLinuxHandoff, which runs when a busy macOS instance blocks an aged
// Linux head. Bounded is not the same as safe, so the same invariant binds it.
func TestBoundedHandoffWaveHonoursTheReservation(t *testing.T) {
	for _, test := range []struct {
		name        string
		headProfile domain.ProfileID
		headRepoCap int
		mac         domain.ProfileID
		wantMac     bool
	}{
		{name: "an infeasible head lends the stranded residual",
			headProfile: "xl", headRepoCap: 4, mac: "maestro", wantMac: true},
		// ADR 0038: a head its repository cap holds out cannot use the vector it
		// reserves, so it lends it to work it outranks. A `maestro`
		// (4 CPU / 7168 MB) cannot take a `large`'s (4 CPU / 8192 MB) vector.
		{name: "a cap-held head lends its vector to work it outranks",
			headProfile: "large", headRepoCap: 1, mac: "maestro", wantMac: true},
		// ...and never to work that could take the vector whole. A `maestro`
		// (4 CPU / 7168 MB) fits the free envelope but not the {3, 11264, 1}
		// remainder beside a `medium` head, so it can only be admitted by eating
		// into that head's own vector — and it could take the vector whole. That
		// is ADR 0017's no-jump guarantee, which is a RULE here rather than a
		// by-product of the subtraction.
		{name: "a cap-held head still refuses a peer that could take its vector",
			headProfile: "medium", headRepoCap: 1, mac: "maestro", wantMac: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := reservedRemainderConfig()
			cfg.RepoCaps["b/repo"] = test.headRepoCap
			head := profileDemand(cfg, "b/repo", 1, 65*time.Minute, test.headProfile)
			mac := profileDemand(cfg, "mac-a", 2, 60*time.Minute, test.mac)
			// A busy maestro makes the host mixed, so the Linux head goes through
			// planLinuxWithCoexistence; a live small in the head's repository is
			// what blocks a head that would otherwise fit.
			live := []domain.Instance{liveInstance(cfg, "mac-1", "mac-b", "maestro"),
				liveInstance(cfg, "linux-small-1", "b/repo", "small")}

			plan := PlanTick(reservedRemainderInput(cfg, []domain.Demand{head, mac}, live, State{}))

			if plan.Next.Reservation == nil || plan.Next.Reservation.Demand != head.Key {
				t.Fatalf("the aged head must hold its reservation, got %#v", plan.Next.Reservation)
			}
			if got := spawnsProfile(plan, test.mac); got != test.wantMac {
				t.Fatalf("bounded wave admitted %s = %v, want %v: %#v", test.mac, got, test.wantMac, plan.Operations)
			}
			// The latch is written only by planLinuxHandoff's wave, so it proves
			// WHICH pass decided — the remainder pass cannot set it.
			if plan.Next.LinuxHandoff == nil || plan.Next.LinuxHandoff.BackfillAdmitted != test.wantMac {
				t.Fatalf("the handoff wave, not the remainder pass, must be the deciding pass: %#v", plan.Next.LinuxHandoff)
			}
		})
	}
}

// TestReservedHeadWinsTheVectorAfterAMacRemainderFill pins the other half of the
// contract, exactly as ADR 0017 does for the Linux queue: residual admission
// behind an infeasible head must not weaken the reservation. The head keeps its
// turn and takes the first vector large enough for it, ahead of everything the
// remainder pass admitted.
func TestReservedHeadWinsTheVectorAfterAMacRemainderFill(t *testing.T) {
	cfg := reservedRemainderConfig()
	head := profileDemand(cfg, "b/repo", 1, 65*time.Minute, "xl")
	mac := profileDemand(cfg, "mac-a", 2, 60*time.Minute, "maestro")
	busy := liveInstance(cfg, "linux-xl-1", "a/repo", "xl")

	first := PlanTick(reservedRemainderInput(cfg, []domain.Demand{head, mac}, []domain.Instance{busy}, State{}))
	if !spawnsProfile(first, "maestro") {
		t.Fatalf("the feasible maestro must be admitted behind the infeasible head: %#v", first.Operations)
	}
	if first.Next.Reservation == nil || first.Next.Reservation.Demand != head.Key {
		t.Fatalf("the head must still be reserved after the fill, got %#v", first.Next.Reservation)
	}

	// The hour-long xl job finishes; the maestro admitted above is now live.
	spawned := liveInstance(cfg, "mac-1", "mac-a", "maestro")
	second := PlanTick(reservedRemainderInput(cfg, []domain.Demand{head}, []domain.Instance{spawned}, first.Next))

	if !spawnsProfile(second, "xl") {
		t.Fatalf("the reserved head must win the first vector large enough for it: %#v", second.Operations)
	}
	if second.Next.Reservation != nil {
		t.Fatalf("an admitted head must release its reservation, got %#v", second.Next.Reservation)
	}
}

// TestMacRemainderNeverTakesTheReservedHeadsRepositorySlot covers the
// non-resource half of the invariant. Repository caps are counted over EVERY
// live instance, macOS included (activeRepoCounts), so a macOS spawn in the
// reserved head's repository can consume the very cap slot the head is waiting
// for — a delay no remainder arithmetic can see. The identical demand in
// another repository IS admitted (first case of the table above), which is what
// makes the exclusion load-bearing rather than incidental.
func TestMacRemainderNeverTakesTheReservedHeadsRepositorySlot(t *testing.T) {
	cfg := reservedRemainderConfig()
	cfg.RepoCaps["b/repo"] = 1
	head := profileDemand(cfg, "b/repo", 1, 65*time.Minute, "xl")
	sameRepo := profileDemand(cfg, "b/repo", 2, 60*time.Minute, "maestro")
	busy := liveInstance(cfg, "linux-xl-1", "a/repo", "xl")

	plan := PlanTick(reservedRemainderInput(cfg, []domain.Demand{head, sameRepo}, []domain.Instance{busy}, State{}))

	if plan.Next.Reservation == nil || plan.Next.Reservation.Demand != head.Key {
		t.Fatalf("the aged head must hold its reservation, got %#v", plan.Next.Reservation)
	}
	if containsSpawn(plan.Operations) {
		t.Fatalf("a macOS spawn in the reserved head's repository would take its cap slot: %#v", plan.Operations)
	}
}

// TestLinuxRemainderBackfillsBehindAnInfeasibleReservation is the symmetry
// check the incident report asked for: fillLinuxRemainder does NOT carry
// fillMacRemainder's flaw. It delegates to planLinux, which owns the
// reservation and already applies ADR 0017 — the head stays reserved and the
// feasible residual is put to work.
func TestLinuxRemainderBackfillsBehindAnInfeasibleReservation(t *testing.T) {
	cfg := reservedRemainderConfig()
	head := profileDemand(cfg, "b/repo", 1, 65*time.Minute, "xl")
	small := profileDemand(cfg, "a/repo", 2, time.Minute, "small")
	busy := liveInstance(cfg, "linux-xl-1", "a/repo", "xl")
	reservation := &domain.Reservation{Demand: head.Key, Profile: "xl",
		Resources: cfg.Profiles["xl"].Resources, Since: testNow.Add(-time.Hour)}
	in := reservedRemainderInput(cfg, []domain.Demand{head, small}, []domain.Instance{busy},
		State{Reservation: reservation})

	plan := fillLinuxRemainder(in, Plan{Status: PlanReady, Next: State{Reservation: reservation}},
		[]domain.Demand{head, small}, nil)

	if !spawnsProfile(plan, "small") {
		t.Fatalf("the feasible residual must be admitted behind the infeasible head: %#v", plan.Operations)
	}
	if spawnsProfile(plan, "xl") {
		t.Fatalf("the head does not fit and must not spawn: %#v", plan.Operations)
	}
	if plan.Next.Reservation == nil || plan.Next.Reservation.Demand != head.Key {
		t.Fatalf("the head must keep its reservation, got %#v", plan.Next.Reservation)
	}
}

// TestChargeReservedHeadChargesTheSlotAndNeverTheVector pins ADR 0045's
// arithmetic directly, and it is the pin that used to say the opposite.
//
// It read "a reservation that fits is withheld in full", which was ADR 0029
// condition 1. That condition survived exactly as long as ONE of the two axes
// released: once ADR 0038 taught the repository-cap axis to lend as well, a
// reservation held by a judging tick lends on every axis there is, and the
// withheld branch became reachable only for a head no plan had judged — which is
// how issue #235 sterilized two cores for an hour on the mac studio.
//
// So the vector is never charged, and what a reservation costs a complementary
// pass is one repository slot: an instance holding no resources narrows no
// envelope and occupies exactly the slot ADR 0030 says the head still needs.
func TestChargeReservedHeadChargesTheSlotAndNeverTheVector(t *testing.T) {
	cfg := reservedRemainderConfig()
	base := reservedRemainderInput(cfg, nil, []domain.Instance{liveInstance(cfg, "linux-1", "a/repo", "small")}, State{})
	free := domain.Resources{CPU: 9, MemoryMB: 22_528, Slots: 3}
	fits := &domain.Reservation{Demand: domain.DemandKey{Repo: "b/repo", RunID: 1, Attempt: 1, JobID: 1},
		Profile: "large", Resources: cfg.Profiles["large"].Resources}
	infeasible := &domain.Reservation{Demand: domain.DemandKey{Repo: "b/repo", RunID: 1, Attempt: 1, JobID: 2},
		Profile: "builder", Resources: domain.Resources{CPU: 99, MemoryMB: 1, Slots: 1}}

	for _, test := range []struct {
		name        string
		reservation *domain.Reservation
		slots       map[string]int
	}{
		{name: "no reservation charges nothing", slots: map[string]int{"a/repo": 1}},
		{name: "a reservation that fits charges its slot and no vector", reservation: fits,
			slots: map[string]int{"a/repo": 1, "b/repo": 1}},
		{name: "a reservation that cannot fit charges the same", reservation: infeasible,
			slots: map[string]int{"a/repo": 1, "b/repo": 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			charged := chargeReservedHead(base, test.reservation)
			if got := linuxFree(charged); got != free {
				t.Fatalf("envelope = %#v, want the uncharged %#v", got, free)
			}
			if got := activeRepoCounts(charged.Instances.Value); !reflect.DeepEqual(got, test.slots) {
				t.Fatalf("repository slots = %#v, want %#v", got, test.slots)
			}
		})
	}
}

// TestTheReservedHeadsRepositorySlotBoundsWhoIsAdmitted pins ADR 0030's slot
// where it is now enforced: the admission itself.
//
// The rule used to be a pre-selection inside reservedRemainderDemands, which
// ranked the head's repository and handed the spare slots to the top `slack`
// demands. It is now a CHARGE — chargeReservedHead models the head's slot as
// occupancy and appendMacSpawns reads it against the same cap it already reads
// for every other repository. The bound is identical; what changed is that a
// candidate which cannot be admitted no longer spends it (issue #255).
//
// One live `xl` already occupies a slot of the head's repository and the head
// holds the next one, so a cap of 2 leaves nothing and a cap of 3 leaves exactly
// one. The free envelope holds one maestro, so each case is a statement about
// WHICH demand takes it.
func TestTheReservedHeadsRepositorySlotBoundsWhoIsAdmitted(t *testing.T) {
	cfg := reservedRemainderConfig()
	head := profileDemand(cfg, "b/repo", 1, 65*time.Minute, "xl")
	youngSameRepo := profileDemand(cfg, "b/repo", 2, time.Minute, "maestro")
	agedSameRepo := profileDemand(cfg, "b/repo", 3, 60*time.Minute, "maestro")
	otherRepo := profileDemand(cfg, "mac-a", 4, time.Minute, "maestro")
	reservation := &domain.Reservation{Demand: head.Key, Profile: "xl", Resources: cfg.Profiles["xl"].Resources}

	for _, test := range []struct {
		name    string
		caps    map[string]int
		demands []domain.Demand
		want    []domain.DemandKey
	}{
		{name: "an unconfigured cap is one slot and it is the head's",
			caps:    map[string]int{"a/repo": 4, "mac-a": 2},
			demands: []domain.Demand{youngSameRepo, agedSameRepo, otherRepo}, want: []domain.DemandKey{otherRepo.Key}},
		{name: "the head's last slot is never lent",
			caps:    map[string]int{"a/repo": 4, "b/repo": 2, "mac-a": 2},
			demands: []domain.Demand{youngSameRepo, agedSameRepo, otherRepo}, want: []domain.DemandKey{otherRepo.Key}},
		{name: "one spare slot goes to the aged demand",
			caps:    map[string]int{"a/repo": 4, "b/repo": 3, "mac-a": 2},
			demands: []domain.Demand{youngSameRepo, agedSameRepo, otherRepo}, want: []domain.DemandKey{agedSameRepo.Key}},
		{name: "spare slots for everyone still serve the aged demand first",
			caps:    map[string]int{"a/repo": 4, "b/repo": 4, "mac-a": 2},
			demands: []domain.Demand{youngSameRepo, agedSameRepo, otherRepo}, want: []domain.DemandKey{agedSameRepo.Key}},
	} {
		t.Run(test.name, func(t *testing.T) {
			in := reservedRemainderInput(cfg, test.demands,
				[]domain.Instance{liveInstance(cfg, "linux-xl-1", "b/repo", "xl")}, State{})
			in.Config.RepoCaps = test.caps
			plan := Plan{Status: PlanReady, Next: State{Reservation: reservation}}
			if got := spawnedKeys(fillMacRemainder(in, plan, test.demands, nil)); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("remainder admitted %#v, want %#v", got, test.want)
			}
		})
	}
}

// TestReservedRemainderDemandsDropsNothingWithoutAReservation is the other half
// of the filter's contract: with no reservation held there is no head to protect
// and every candidate passes through.
func TestReservedRemainderDemandsDropsNothingWithoutAReservation(t *testing.T) {
	cfg := reservedRemainderConfig()
	demands := []domain.Demand{
		profileDemand(cfg, "b/repo", 2, time.Minute, "maestro"),
		profileDemand(cfg, "b/repo", 3, 60*time.Minute, "maestro"),
		profileDemand(cfg, "mac-a", 4, time.Minute, "maestro"),
	}
	base := reservedRemainderInput(cfg, demands, []domain.Instance{liveInstance(cfg, "linux-xl-1", "b/repo", "xl")}, State{})
	if got := reservedRemainderDemands(base, Plan{Status: PlanReady}, demands); len(got) != len(demands) {
		t.Fatalf("no reservation must drop nothing, got %#v", got)
	}
}

// TestASameRepositoryCandidateThatCannotFitDoesNotSpendTheHeadsSpareSlot is
// issue #255's second defect, reduced from the tiered arm's seed 82 at tick 298.
//
// An equal-sized `builder` outranked every `maestro` on age, so the deleted
// pre-selection handed it the reserved head's one spare repository slot. It
// could not use that slot -- six cores into the four the head's own charge left
// -- and the `maestro` that fit those four cores exactly had already been
// dropped from the candidate list, so the vector went to a demand of ANOTHER
// repository. That is the aged-FIFO inversion ADR 0030 exists to end, committed
// by the guard that ends it.
//
// In the sweep the two demands carried declared tiers, which is why it surfaced
// as a tier_inversion: tier 13 waiting, tier 10 admitted. Age is the same
// statement without the tier, and it is the one this fleet has always made.
//
// ADR 0045 moves where the `builder` is refused, and moves it EARLIER. The head
// no longer withholds a vector, so ADR 0017's no-jump rule now binds on every
// candidate rather than only on a lending head, and a `builder` equal to the
// `xl` head is refused by that rule before any slot arithmetic sees it. The
// assertion is unchanged in what it forbids: if the `builder` ever spent
// `b/repo`'s spare slot, the older same-repository `maestro` would drop out of
// the result. What the head's vector now buys the fleet is in the second key --
// the residual is not held idle behind it, so the other repository's work is
// admitted beside the one the head outranks rather than instead of it.
func TestASameRepositoryCandidateThatCannotFitDoesNotSpendTheHeadsSpareSlot(t *testing.T) {
	cfg := reservedRemainderConfig()
	cfg.RepoCaps = map[string]int{"b/repo": 2, "mac-a": 2}
	head := profileDemand(cfg, "b/repo", 1, 65*time.Minute, "xl")
	// Six cores, older than either maestro, and unable to fit the four the head's
	// own charge leaves behind.
	unfitting := profileDemand(cfg, "b/repo", 2, 90*time.Minute, "builder")
	sameRepo := profileDemand(cfg, "b/repo", 3, 60*time.Minute, "maestro")
	otherRepo := profileDemand(cfg, "mac-a", 4, 30*time.Minute, "maestro")
	demands := []domain.Demand{unfitting, sameRepo, otherRepo}

	in := reservedRemainderInput(cfg, demands, nil, State{})
	plan := Plan{Status: PlanReady, Next: State{Reservation: &domain.Reservation{
		Demand: head.Key, Profile: "xl", Resources: cfg.Profiles["xl"].Resources}}}

	want := []domain.DemandKey{sameRepo.Key, otherRepo.Key}
	if got := spawnedKeys(fillMacRemainder(in, plan, demands, nil)); !reflect.DeepEqual(got, want) {
		t.Fatalf("remainder admitted %#v, want the older same-repository demand first, %#v", got, want)
	}
}

// TestMacRemainderAdmitsSameRepositoryWorkTheHeadCanSpare is the 2026-08-03
// counterexample to ADR 0029 as first written.
//
// Live at ~06:50Z: the reserved aged head was an `xl` in a repository whose cap
// is 4 with a single live instance, and the queue behind it held `maestro` work
// aged 4h39m in THAT SAME repository. The head could not use the four free
// cores — a live `xl` held what it needs — so ADR 0029's vector condition
// released the residual. Its repository condition then withdrew it again, and
// the aged same-repository work starved for another hour.
//
// It could not have cost the head anything. Three cap slots stood free; the
// head needs one. The two cases below are the same topology and differ only in
// whether a spare slot exists.
func TestMacRemainderAdmitsSameRepositoryWorkTheHeadCanSpare(t *testing.T) {
	for _, test := range []struct {
		name    string
		repoCap int
		wantMac bool
	}{
		{name: "a spare repository slot is lent to aged same-repo work", repoCap: 4, wantMac: true},
		{name: "the head's last repository slot is withheld", repoCap: 2, wantMac: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := reservedRemainderConfig()
			cfg.RepoCaps["b/repo"] = test.repoCap
			head := profileDemand(cfg, "b/repo", 1, 65*time.Minute, "xl")
			sameRepo := profileDemand(cfg, "b/repo", 2, 60*time.Minute, "maestro")
			// The live xl is in the head's own repository, exactly as it was live:
			// it occupies one cap slot AND the six cores the head is waiting for.
			busy := []domain.Instance{liveInstance(cfg, "linux-xl-1", "b/repo", "xl")}

			plan := PlanTick(reservedRemainderInput(cfg, []domain.Demand{head, sameRepo}, busy, State{}))

			if plan.Next.Reservation == nil || plan.Next.Reservation.Demand != head.Key {
				t.Fatalf("the aged head must hold its reservation, got %#v", plan.Next.Reservation)
			}
			if containsDrain(plan.Operations) {
				t.Fatalf("a remainder pass must never drain to make room: %#v", plan.Operations)
			}
			if spawnsProfile(plan, "xl") {
				t.Fatalf("the reserved head cannot fit and must not spawn: %#v", plan.Operations)
			}
			if got := spawnsProfile(plan, "maestro"); got != test.wantMac {
				t.Fatalf("same-repository maestro admitted = %v, want %v: %#v", got, test.wantMac, plan.Operations)
			}
		})
	}
}

// TestAgedSameRepositoryWorkOutranksYoungOtherRepositoryWork is the aged-FIFO
// half of the same counterexample. Excluding the head's repository wholesale
// did not merely refuse work — it INVERTED the queue: a young demand from
// another repository was admitted into the residual while aged work that had
// waited four hours was not even a candidate. One maestro fits the four free
// cores, and it must be the aged one.
func TestAgedSameRepositoryWorkOutranksYoungOtherRepositoryWork(t *testing.T) {
	cfg := reservedRemainderConfig()
	head := profileDemand(cfg, "b/repo", 1, 65*time.Minute, "xl")
	agedSameRepo := profileDemand(cfg, "b/repo", 2, 60*time.Minute, "maestro")
	youngOtherRepo := profileDemand(cfg, "mac-a", 3, time.Minute, "maestro")
	busy := []domain.Instance{liveInstance(cfg, "linux-xl-1", "b/repo", "xl")}

	plan := PlanTick(reservedRemainderInput(cfg,
		[]domain.Demand{head, agedSameRepo, youngOtherRepo}, busy, State{}))

	got := spawnedDemands(plan.Operations)
	if len(got) != 1 || got[0].Key != agedSameRepo.Key {
		t.Fatalf("residual spawns = %#v, want only the aged same-repository maestro %#v", got, agedSameRepo.Key)
	}
	if plan.Next.Reservation == nil || plan.Next.Reservation.Demand != head.Key {
		t.Fatalf("the aged head must hold its reservation, got %#v", plan.Next.Reservation)
	}
}

// TestBoundedHandoffWaveAdmitsSameRepositoryWorkTheHeadCanSpare verifies the
// precise slot check on the SECOND pass bound by ADR 0029 — planLinuxHandoff's
// one-shot backfill wave. It is the same invariant, so it must reach the same
// two answers, and the latch proves which pass decided.
func TestBoundedHandoffWaveAdmitsSameRepositoryWorkTheHeadCanSpare(t *testing.T) {
	for _, test := range []struct {
		name    string
		repoCap int
		wantMac bool
	}{
		{name: "a spare repository slot is lent by the wave", repoCap: 4, wantMac: true},
		{name: "the head's last repository slot is withheld by the wave", repoCap: 2, wantMac: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := reservedRemainderConfig()
			cfg.RepoCaps["b/repo"] = test.repoCap
			head := profileDemand(cfg, "b/repo", 1, 65*time.Minute, "xl")
			sameRepo := profileDemand(cfg, "b/repo", 2, 60*time.Minute, "maestro")
			// A busy maestro makes the host mixed, so the Linux head goes through
			// planLinuxWithCoexistence and reaches the wave; the live small in the
			// head's repository holds one of its cap slots. Five cores are free and
			// the head needs six, so its vector is not what refuses anything here.
			live := []domain.Instance{liveInstance(cfg, "mac-1", "mac-b", "maestro"),
				liveInstance(cfg, "linux-small-1", "b/repo", "small")}

			plan := PlanTick(reservedRemainderInput(cfg, []domain.Demand{head, sameRepo}, live, State{}))

			if plan.Next.Reservation == nil || plan.Next.Reservation.Demand != head.Key {
				t.Fatalf("the aged head must hold its reservation, got %#v", plan.Next.Reservation)
			}
			if got := spawnsProfile(plan, "maestro"); got != test.wantMac {
				t.Fatalf("bounded wave admitted maestro = %v, want %v: %#v", got, test.wantMac, plan.Operations)
			}
			if plan.Next.LinuxHandoff == nil || plan.Next.LinuxHandoff.BackfillAdmitted != test.wantMac {
				t.Fatalf("the handoff wave, not the remainder pass, must be the deciding pass: %#v", plan.Next.LinuxHandoff)
			}
		})
	}
}
