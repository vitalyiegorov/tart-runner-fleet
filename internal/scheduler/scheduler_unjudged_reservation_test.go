package scheduler

import (
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// The tests in this file pin issue #235 and ADR 0045: a reservation withholds
// ORDER and one repository slot; it never withholds a vector.
//
// The incident is the mac studio, 2026-08-18. `fleet doctor` reported
//
//	FAIL reservation  reservation for budgie-at/budgie/31389581206/1/7647894864237434209
//	                  of profile linux-2x4 has withheld 2 cpu / 4096 MiB for 1h5m0s
//	                  on the unjudged axis
//
// beside a queue three hours deep on a node with two idle cores. The node held
// one live `macos-4x7` (4 CPU / 7168 MiB) for `budgie-at/budgie` against a
// `hostBudget {cpu: 6, memoryMb: 16384}`, leaving 2 CPU / 9216 MiB free, and the
// reserved head was a `linux-2x4` — 2 CPU / 4096 MiB, which fits that envelope
// exactly and whose repository was one instance into a cap of four.
//
// So the head was admissible on BOTH of `feasible`'s terms, and `unjudged` is not
// a third axis: `admissionAxis` IS the decision `feasible` makes, so a plan that
// judges a held head always names one of them. An empty axis means the plan never
// judged the head at all — it carried the prior reservation through a tick that
// planned another platform.
//
// That carried reservation is not inert. `fillMacRemainder` and
// `planMacHandoff` charge it through `chargeReservedHead`, whose withhold branch
// binds exactly when the head FITS and is under its cap — the state `planLinux`
// can never produce, because it would have admitted such a head. After ADR 0038
// taught both axes to lend, the withhold branch became reachable only for a head
// no plan judged, and it sterilized that head's vector against the only pass
// that was running.
var studioNow = time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

// studioConfig is the incident node: a 6-vCPU / 16 GiB declared budget on a
// larger machine, one Linux slot ceiling of four, and `budgie-at/budgie` capped
// at four.
func studioConfig() Config {
	return Config{
		LinuxCapacity:          domain.Resources{CPU: 6, MemoryMB: 16_384, Slots: 4},
		FairnessAge:            5 * time.Minute,
		AssignedTimeout:        15 * time.Minute,
		ElasticHostEnvelope:    true,
		HostBudget:             domain.Resources{CPU: 6, MemoryMB: 16_384},
		MixedPlatformAdmission: false,
		MixedProfileCohorts:    true,
		RepoCaps:               map[string]int{"budgie-at/budgie": 4, "knee-repo/knee": 4},
		Profiles: map[domain.ProfileID]domain.Profile{
			"linux-1x2": studioProfile("linux-1x2", 1, 2_048),
			"linux-2x4": studioProfile("linux-2x4", 2, 4_096),
			"macos-4x7": studioProfile("macos-4x7", 4, 7_168),
		},
	}
}

// studioProfile builds one of the node's resource-explicit profiles (ADR 0032)
// from its own name, which is where every other fact about it already lives: a
// `macos-` prefix is the platform and the maestro route, and the numbers are the
// vector.
func studioProfile(id domain.ProfileID, cpu, memory int) domain.Profile {
	profile := domain.Profile{ID: id, Platform: domain.PlatformLinux, Route: "tiered",
		Resources: domain.Resources{CPU: cpu, MemoryMB: memory, Slots: 1}}
	if strings.HasPrefix(string(id), "macos-") {
		profile.Platform, profile.Route, profile.MaxActive = domain.PlatformMacOS, "macos-maestro", 2
	}
	return profile
}

func studioDemand(cfg Config, repo string, run, job int64, age time.Duration, profile domain.ProfileID) domain.Demand {
	return domain.Demand{Key: domain.DemandKey{Repo: repo, RunID: run, Attempt: 1, JobID: job},
		CreatedAt: studioNow.Add(-age), Profile: profile, Route: cfg.Profiles[profile].Route,
		Platform: cfg.Profiles[profile].Platform, Event: domain.EventPullRequest}
}

func studioInstance(cfg Config, id, repo string, profile domain.ProfileID) domain.Instance {
	return domain.Instance{ID: id, Repo: repo, Platform: cfg.Profiles[profile].Platform, Profile: profile,
		Route: cfg.Profiles[profile].Route, Resources: cfg.Profiles[profile].Resources,
		State: domain.InstanceRunning, Power: domain.InstancePowerRunning}
}

// studioHead is the reserved head of the incident: `budgie-at/budgie`'s
// `linux-2x4`, aged past the fairness age.
func studioHead(cfg Config) domain.Demand {
	return studioDemand(cfg, "budgie-at/budgie", 31_389_581_206, 7_647_894_864_237_434_209, 70*time.Minute, "linux-2x4")
}

func studioInput(cfg Config, demands []domain.Demand, instances []domain.Instance, prior State) Input {
	return Input{Now: studioNow, Config: cfg, Demands: domain.Fresh(demands, studioNow),
		Instances: domain.Fresh(instances, studioNow),
		Host: domain.Fresh(domain.Host{Available: domain.Resources{CPU: 6, MemoryMB: 17_408, Slots: 4},
			Capacity: domain.Resources{CPU: 10, MemoryMB: 24_576, Slots: 4}}, studioNow), Prior: prior}
}

// studioReservation is the reservation `fleet doctor` reported, minted 65
// minutes before this tick under a tighter envelope that has since opened.
func studioReservation(cfg Config) *domain.Reservation {
	head := studioHead(cfg)
	return &domain.Reservation{Demand: head.Key, Profile: head.Profile,
		Resources: cfg.Profiles[head.Profile].Resources, Since: studioNow.Add(-65 * time.Minute)}
}

// TestIssue235EnvelopeIsTheIncidentsOwn checks the reconstruction before it is
// used to conclude anything. `fleet doctor` reported 2 CPU / 9216 MiB free
// against the declared budget, and the two envelopes this package plans in have
// to reproduce that number, or every test below is about a different node.
func TestIssue235EnvelopeIsTheIncidentsOwn(t *testing.T) {
	cfg := studioConfig()
	in := studioInput(cfg, []domain.Demand{studioHead(cfg)},
		[]domain.Instance{studioInstance(cfg, "gha-macos-1", "budgie-at/budgie", "macos-4x7")}, State{})
	want := domain.Resources{CPU: 2, MemoryMB: 9_216, Slots: 3}
	if got := linuxFreeAged(in); got != want {
		t.Fatalf("starvation envelope is not the incident's: got %v want %v", got, want)
	}
	if got := linuxFree(in); got != want {
		t.Fatalf("throttled envelope is not the incident's: got %v want %v", got, want)
	}
}

// TestIssue235ReservedHeadIsAdmissibleOnBothAxes is the first thing the issue
// asks and the answer to it: NO condition of the reservation logic holds this
// head. Its vector fits the starvation envelope exactly, and its repository is
// one instance into a cap of four, so `feasible` says yes and `planLinux`
// admits it on any tick that judges it.
//
// This is what makes `unjudged` the whole finding rather than a missing axis.
// The reservation names a head the fleet could have started.
func TestIssue235ReservedHeadIsAdmissibleOnBothAxes(t *testing.T) {
	cfg := studioConfig()
	head := studioHead(cfg)
	instances := []domain.Instance{studioInstance(cfg, "gha-macos-1", "budgie-at/budgie", "macos-4x7")}
	demands := []domain.Demand{head, studioDemand(cfg, "knee-repo/knee", 900, 901, 44*time.Minute, "linux-1x2")}
	in := studioInput(cfg, demands, instances, State{Reservation: studioReservation(cfg)})

	if !feasible(cfg.Profiles[head.Profile].Resources, linuxFreeAged(in), head.Key.Repo,
		activeRepoCounts(instances), nil, cfg.RepoCaps) {
		t.Fatal("the reserved head is refused by feasible, so the incident is not the state this reconstructs")
	}
	plan := PlanTick(in)
	if plan.Next.Reservation != nil {
		t.Fatalf("a tick that judges an admissible head must discharge its reservation, got %+v", plan.Next.Reservation)
	}
	if keys := spawnedKeys(plan); len(keys) == 0 || keys[0] != head.Key {
		t.Fatalf("the admissible head is not admitted first: %v", keys)
	}
}

// studioCarriedInput is the incident tick itself: a macOS demand older than the
// Linux head heads the queue, so `PlanTick` plans the macOS lane and the Linux
// reservation is carried without ever being judged.
func studioCarriedInput(cfg Config) Input {
	head := studioHead(cfg)
	demands := []domain.Demand{
		head,
		studioDemand(cfg, "knee-repo/knee", 900, 901, 44*time.Minute, "linux-1x2"),
		studioDemand(cfg, "budgie-at/budgie", 31_381_184_045, 801, 187*time.Minute, "macos-4x7"),
	}
	instances := []domain.Instance{studioInstance(cfg, "gha-macos-1", "budgie-at/budgie", "macos-4x7")}
	return studioInput(cfg, demands, instances, State{Reservation: studioReservation(cfg)})
}

// TestIssue235ACarriedReservationIsJudgedBeforeItIsPublished pins the classifier
// half. The plan that carries a reservation through a macOS-headed tick used to
// publish an EMPTY axis, which `fleet doctor` rendered as "unjudged" and the
// daemon read as "withholding its vector" — a claim nothing had established.
//
// A carried reservation is now judged against the same envelope and the same
// occupancy `planLinux` judges a head in, so the axis names what is true: no
// axis refuses this head.
func TestIssue235ACarriedReservationIsJudgedBeforeItIsPublished(t *testing.T) {
	cfg := studioConfig()
	plan := PlanTick(studioCarriedInput(cfg))
	if plan.Next.Reservation == nil {
		t.Fatal("the incident tick carries the reservation; this reconstruction does not")
	}
	if plan.ReservationAxis != ReservationAxisNone {
		t.Fatalf("a carried reservation for an admissible head names no axis: got %q want %q",
			plan.ReservationAxis, ReservationAxisNone)
	}
}

// TestIssue235ACarriedReservationWithholdsNoVector is the fleet half, and it is
// the capacity the issue is about. The macOS remainder pass is the only pass
// running on this tick, and it was charged the reserved head's whole 2 CPU /
// 4096 MiB on behalf of a head that no plan had judged and that `planLinux`
// would have admitted outright.
//
// ADR 0045 deletes that charge: a reservation withholds order and one repository
// slot, never a vector. What the head still holds is its slot, so this asserts
// both — the vector is lent, and the slot is not.
func TestIssue235ACarriedReservationWithholdsNoVector(t *testing.T) {
	cfg := studioConfig()
	in := studioCarriedInput(cfg)
	charged := chargeReservedHead(in, in.Prior.Reservation)
	reserved := charged.Instances.Value[len(charged.Instances.Value)-1]
	if reserved.Resources != (domain.Resources{}) {
		t.Fatalf("a reservation charges no vector to a complementary pass: got %v", reserved.Resources)
	}
	if reserved.Repo != in.Prior.Reservation.Demand.Repo {
		t.Fatalf("a reservation still charges its repository slot: got %q", reserved.Repo)
	}
	if got := linuxFreeAged(charged); got != linuxFreeAged(in) {
		t.Fatalf("a charged reservation narrowed the envelope: got %v want %v", got, linuxFreeAged(in))
	}
}

// TestIssue235TheLentVectorIsStillNotJumped is ADR 0017's no-jump guarantee on
// the axis this record adds, and it is what stops the deletion above from
// becoming a licence. `reservedRemainderDemands` used to apply the rule only
// while the head lent; the head now always lends, so the rule always binds.
//
// The `macos-4x7` peer is 4 CPU against the head's 2, so it could take the
// head's whole vector and must be refused. The counterweight is in the same
// test: the same pass admits a demand that cannot take that vector, so the
// refusal cannot be passing on an empty envelope.
func TestIssue235TheLentVectorIsStillNotJumped(t *testing.T) {
	cfg := studioConfig()
	in := studioCarriedInput(cfg)
	plan := Plan{Status: PlanReady, Next: State{Reservation: studioReservation(cfg)}}
	peer := studioDemand(cfg, "knee-repo/knee", 700, 701, 90*time.Minute, "macos-4x7")
	small := studioDemand(cfg, "knee-repo/knee", 700, 702, 90*time.Minute, "linux-1x2")

	kept := reservedRemainderDemands(in, plan, []domain.Demand{peer, small})
	if len(kept) != 1 || kept[0].Key != small.Key {
		t.Fatalf("a peer that could take the reserved vector whole must be refused, and a smaller one admitted: %v", kept)
	}
}
