package scheduler

import (
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
		[]domain.Demand{head, small})

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

// TestChargeReservedHeadIsExact pins the envelope arithmetic directly: no
// reservation charges nothing, a reservation that fits is charged in full so a
// complementary pass plans inside free - reservation, and a reservation that
// does not fit is not charged at all (ADR 0017: the head is not waiting on this
// pass).
func TestChargeReservedHeadIsExact(t *testing.T) {
	cfg := reservedRemainderConfig()
	base := reservedRemainderInput(cfg, nil, []domain.Instance{liveInstance(cfg, "linux-1", "a/repo", "small")}, State{})
	fits := &domain.Reservation{Demand: domain.DemandKey{Repo: "b/repo", RunID: 1, Attempt: 1, JobID: 1},
		Profile: "large", Resources: cfg.Profiles["large"].Resources}
	infeasible := &domain.Reservation{Demand: domain.DemandKey{Repo: "b/repo", RunID: 1, Attempt: 1, JobID: 2},
		Profile: "builder", Resources: domain.Resources{CPU: 99, MemoryMB: 1, Slots: 1}}

	for _, test := range []struct {
		name        string
		reservation *domain.Reservation
		want        domain.Resources
	}{
		{name: "no reservation charges nothing", want: domain.Resources{CPU: 9, MemoryMB: 22_528, Slots: 3}},
		{name: "a reservation that fits is withheld in full", reservation: fits,
			want: domain.Resources{CPU: 5, MemoryMB: 14_336, Slots: 2}},
		{name: "a reservation that cannot fit withholds nothing", reservation: infeasible,
			want: domain.Resources{CPU: 9, MemoryMB: 22_528, Slots: 3}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := linuxFree(chargeReservedHead(base, test.reservation)); got != test.want {
				t.Fatalf("envelope = %#v, want %#v", got, test.want)
			}
		})
	}
}

// TestReservedRemainderDemandsExcludesOnlyTheReservedRepository pins the filter
// itself: without a reservation nothing is dropped, with one exactly the
// reserved head's repository is dropped and order is preserved.
func TestReservedRemainderDemandsExcludesOnlyTheReservedRepository(t *testing.T) {
	cfg := reservedRemainderConfig()
	head := profileDemand(cfg, "b/repo", 1, time.Minute, "xl")
	sameRepo := profileDemand(cfg, "b/repo", 2, time.Minute, "maestro")
	otherRepo := profileDemand(cfg, "mac-a", 3, time.Minute, "maestro")
	demands := []domain.Demand{sameRepo, otherRepo}
	reservation := &domain.Reservation{Demand: head.Key, Profile: "xl", Resources: cfg.Profiles["xl"].Resources}

	if got := reservedRemainderDemands(demands, nil); len(got) != 2 {
		t.Fatalf("no reservation must drop nothing, got %#v", got)
	}
	got := reservedRemainderDemands(demands, reservation)
	if len(got) != 1 || got[0].Key != otherRepo.Key {
		t.Fatalf("reservedRemainderDemands = %#v, want only %#v", got, otherRepo.Key)
	}
}
