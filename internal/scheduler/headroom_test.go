package scheduler

import (
	"math"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// headroom is one set alone on the node with unbounded room: the most of its
// own vector the residual can start.
func headroom(in Input, profile domain.ProfileID) (int, bool) {
	shares, ok := PartitionHeadroom(in, []Claim{{Profile: profile, Room: math.MaxInt}})
	if !ok || shares[0] == Unsized {
		return 0, false
	}
	return shares[0], true
}

// macbookInput is issue #388's node: a MacBook whose operator budget is 10 CPU,
// 14 GiB and two slots, on a host physically larger than that, with one
// maestro guest (4 CPU / 7 GiB) running. The residual is 6 CPU / 7 GiB / one
// slot, so a `large` (4 CPU / 8 GiB) cannot start until the maestro exits.
func macbookInput(demands []domain.Demand, instances []domain.Instance) Input {
	in := input(demands, instances, State{})
	in.Config.MixedPlatformAdmission = true
	in.Config.MixedProfileCohorts = true
	in.Config.ElasticHostEnvelope = true
	in.Config.LinuxCapacity = domain.Resources{CPU: 10, MemoryMB: 14_336, Slots: 2}
	in.Config.HostBudget = domain.Resources{CPU: 10, MemoryMB: 14_336, Slots: 2}
	in.Host = domain.Fresh(domain.Host{
		Capacity:  domain.Resources{CPU: 12, MemoryMB: 24_576, Slots: 4},
		Available: domain.Resources{CPU: 12, MemoryMB: 20_000, Slots: 4},
	}, testNow)
	return in
}

// TestHeadroomCountsOnlyWhatTheResidualCanStart is the pure half of issue #388:
// a scale set may advertise only the instances of its own vector that the
// node's CURRENT residual can start. A maestro guest holds 7 GiB of a 14 GiB
// budget, so no `large` fits and every `large` set on the node must advertise
// nothing beyond what it already holds.
func TestHeadroomCountsOnlyWhatTheResidualCanStart(t *testing.T) {
	maestro := runningInstance(testConfig(), "maestro-1", "a/repo", "maestro")
	cases := map[string]struct {
		in      Input
		profile domain.ProfileID
		want    int
	}{
		"a 4x8 vector does not fit beside a running maestro": {
			in: macbookInput(nil, []domain.Instance{maestro}), profile: "large", want: 0},
		"a 2x4 vector fits once: one slot remains": {
			in: macbookInput(nil, []domain.Instance{maestro}), profile: "medium", want: 1},
		"an idle node fits the budget's worth of 4x8 vectors": {
			in: macbookInput(nil, nil), profile: "large", want: 1},
		"an idle node fits two 2x4 vectors, bounded by the two slots": {
			in: macbookInput(nil, nil), profile: "medium", want: 2},
		// Already-assigned work that has not launched yet will consume the residual
		// before anything newly advertised can, wherever it is queued.
		"a queued demand is charged before anything new is advertised": {
			in:      macbookInput([]domain.Demand{demand("b/repo", 1, time.Minute, "medium")}, []domain.Instance{maestro}),
			profile: "small", want: 0},
		// macOS profiles also carry their own MaxActive; a cohort at its limit has
		// no headroom however much envelope is left.
		"a macOS profile at its MaxActive has no headroom": {
			in: func() Input {
				in := macbookInput(nil, []domain.Instance{maestro})
				in.Config.HostBudget = domain.Resources{}
				in.Config.LinuxCapacity = domain.Resources{CPU: 12, MemoryMB: 24_576, Slots: 4}
				profile := in.Config.Profiles["maestro"]
				profile.MaxActive = 1
				in.Config.Profiles["maestro"] = profile
				return in
			}(), profile: "maestro", want: 0},
		// A queued maestro demand speaks for a MaxActive slot before it launches;
		// the running guest's own demand is not counted twice.
		"a queued macOS demand takes its MaxActive slot before launching": {
			in: func() Input {
				running := maestro
				running.Demand = demand("a/repo", 7, time.Hour, "maestro").Key
				in := macbookInput([]domain.Demand{demand("a/repo", 7, time.Hour, "maestro"),
					demand("b/repo", 8, time.Minute, "maestro")}, []domain.Instance{running})
				in.Config.HostBudget = domain.Resources{}
				in.Config.LinuxCapacity = domain.Resources{CPU: 12, MemoryMB: 24_576, Slots: 4}
				return in
			}(), profile: "maestro", want: 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, ok := headroom(tc.in, tc.profile)
			if !ok || got != tc.want {
				t.Fatalf("headroom(%s) = %d, %v; want %d, true", tc.profile, got, ok, tc.want)
			}
		})
	}
}

// TestHeadroomIsUnknownWithoutUsableObservations: the advertised capacity fails
// open to the configured value when the envelope cannot be computed, so the
// pure function must say "unknown" rather than "zero".
func TestHeadroomIsUnknownWithoutUsableObservations(t *testing.T) {
	cases := map[string]Input{
		"instances unavailable": func() Input {
			in := macbookInput(nil, nil)
			in.Instances = domain.Unavailable[[]domain.Instance]("tart down")
			return in
		}(),
		"host stale": func() Input {
			in := macbookInput(nil, nil)
			in.Host = domain.Stale(in.Host.Value, testNow.Add(-time.Hour), "stale")
			return in
		}(),
		"demands unavailable": func() Input {
			in := macbookInput(nil, nil)
			in.Demands = domain.Unavailable[[]domain.Demand]("store")
			return in
		}(),
	}
	for name, in := range cases {
		if got, ok := headroom(in, "large"); ok {
			t.Fatalf("%s: Headroom = %d, known; an unusable observation must be unknown", name, got)
		}
	}
	if got, ok := headroom(macbookInput(nil, nil), "nonexistent"); ok {
		t.Fatalf("an unknown profile has no vector to count, got %d known", got)
	}
}

func shareTotal(shares []int) int {
	total := 0
	for _, share := range shares {
		total += max(0, share)
	}
	return total
}

// TestPartitionHeadroomOffersTheResidualOnceAcrossSets is issue #397: an idle
// MacBook starts two 2x4 guests, and three scopes each bind a 2x4 set on it.
// Every set used to be offered both; together they are offered two, one each,
// whichever set the rotation puts first.
func TestPartitionHeadroomOffersTheResidualOnceAcrossSets(t *testing.T) {
	claims := []Claim{{Profile: "medium", Room: math.MaxInt}, {Profile: "medium", Room: math.MaxInt},
		{Profile: "medium", Room: math.MaxInt}}
	for step := range 3 {
		in := macbookInput(nil, nil)
		in.Now = in.Now.Add(time.Duration(step) * shareRotation)
		shares, ok := PartitionHeadroom(in, claims)
		if !ok || shareTotal(shares) != 2 {
			t.Fatalf("rotation %d: shares %v, want two in total", step, shares)
		}
		for _, share := range shares {
			if share > 1 {
				t.Fatalf("rotation %d: shares %v; one set took two while another had none", step, shares)
			}
		}
	}
}

// TestPartitionHeadroomServesWaitingWorkBeforeSpeculating: a set GitHub shows a
// job queued for gets the slot, wherever the rotation stands, and never more
// than the work it is waiting on before the others are served.
func TestPartitionHeadroomServesWaitingWorkBeforeSpeculating(t *testing.T) {
	maestro := runningInstance(testConfig(), "maestro-1", "a/repo", "maestro")
	claims := []Claim{{Profile: "medium", Room: math.MaxInt}, {Profile: "medium", Room: math.MaxInt},
		{Profile: "medium", Room: math.MaxInt, Waiting: 3}}
	for step := range 3 {
		in := macbookInput(nil, []domain.Instance{maestro})
		in.Now = in.Now.Add(time.Duration(step) * shareRotation)
		shares, ok := PartitionHeadroom(in, claims)
		if !ok || shares[2] != 1 || shareTotal(shares) != 1 {
			t.Fatalf("rotation %d: shares %v; the one free slot belongs to the set with work waiting", step, shares)
		}
	}
	shares, _ := PartitionHeadroom(macbookInput(nil, nil), []Claim{{Profile: "medium", Room: math.MaxInt, Waiting: 1},
		{Profile: "medium", Room: math.MaxInt, Waiting: 1}, {Profile: "medium", Room: math.MaxInt}})
	if shares[0] != 1 || shares[1] != 1 || shares[2] != 0 {
		t.Fatalf("two sets with one waiting job each take the two slots: %v", shares)
	}
}

// TestPartitionHeadroomNeverHandsASetMoreThanItsRoom: a set already at its
// configured capacity cannot advertise a share, so it must not absorb one.
func TestPartitionHeadroomNeverHandsASetMoreThanItsRoom(t *testing.T) {
	shares, ok := PartitionHeadroom(macbookInput(nil, nil), []Claim{{Profile: "medium", Room: 0, Waiting: 2},
		{Profile: "medium", Room: math.MaxInt}})
	if !ok || shares[0] != 0 || shares[1] != 2 {
		t.Fatalf("shares %v; the full set keeps nothing and its sibling takes both slots", shares)
	}
}

// TestPartitionHeadroomChargesEveryProfileAgainstOneBudget: a 2x4 and a 4x8
// set on the MacBook draw on one 10 CPU / 14 GiB budget with two slots. Each
// alone would be offered what the whole budget starts; together their offers
// must start at once.
func TestPartitionHeadroomChargesEveryProfileAgainstOneBudget(t *testing.T) {
	in := macbookInput(nil, nil)
	shares, ok := PartitionHeadroom(in, []Claim{{Profile: "large", Room: math.MaxInt},
		{Profile: "medium", Room: math.MaxInt}, {Profile: "large", Room: math.MaxInt}})
	if !ok {
		t.Fatal("usable observations must be sized")
	}
	used := domain.Resources{}
	profiles := []domain.ProfileID{"large", "medium", "large"}
	for index, share := range shares {
		for range share {
			used = used.Add(in.Config.Profiles[profiles[index]].Resources)
		}
	}
	if !in.Config.HostBudget.CanFit(used) || used.Slots != 2 {
		t.Fatalf("shares %v offer %+v, which the %+v budget cannot start at once", shares, used, in.Config.HostBudget)
	}
}

// TestPartitionHeadroomSharesAMacOSCohortAcrossItsSets: MaxActive bounds the
// profile on the node, not each set.
func TestPartitionHeadroomSharesAMacOSCohortAcrossItsSets(t *testing.T) {
	in := input(nil, nil, State{})
	shares, ok := PartitionHeadroom(in, []Claim{{Profile: "maestro", Room: math.MaxInt},
		{Profile: "maestro", Room: math.MaxInt}, {Profile: "maestro", Room: math.MaxInt}})
	if !ok || shareTotal(shares) != 2 {
		t.Fatalf("shares %v; a maestro MaxActive of 2 offers two across every set", shares)
	}
	running := runningInstance(testConfig(), "maestro-1", "a/repo", "maestro")
	shares, _ = PartitionHeadroom(input(nil, []domain.Instance{running}, State{}),
		[]Claim{{Profile: "maestro", Room: math.MaxInt}, {Profile: "maestro", Room: math.MaxInt}})
	if shareTotal(shares) != 1 {
		t.Fatalf("shares %v; one running maestro leaves one", shares)
	}
}

// TestPartitionHeadroomLeavesAnUnsizableClaimUnknown: a claim the scheduler
// cannot size is unknown, never zero, and it takes nothing from its siblings.
func TestPartitionHeadroomLeavesAnUnsizableClaimUnknown(t *testing.T) {
	shares, ok := PartitionHeadroom(macbookInput(nil, nil), []Claim{{Profile: "nonexistent", Room: math.MaxInt},
		{Profile: "medium", Room: math.MaxInt}})
	if !ok || shares[0] != Unsized || shares[1] != 2 {
		t.Fatalf("shares %v, want [Unsized 2]", shares)
	}
	if shares, ok := PartitionHeadroom(macbookInput(nil, nil), nil); !ok || len(shares) != 0 {
		t.Fatalf("no claim has no share: %v %v", shares, ok)
	}
}

// budgetMiniInput is issue #439's node: a Mac mini whose operator budget is 10 CPU
// and 23 GiB, with every one of its 10 CPUs open to Linux, running the
// instances given.
func budgetMiniInput(demands []domain.Demand, instances []domain.Instance) Input {
	in := macbookInput(demands, instances)
	in.Config.LinuxCapacity = domain.Resources{CPU: 10, MemoryMB: 23_552, Slots: 4}
	in.Config.HostBudget = domain.Resources{CPU: 10, MemoryMB: 23_552}
	in.Host = domain.Fresh(domain.Host{
		Capacity:  domain.Resources{CPU: 10, MemoryMB: 24_576, Slots: 4},
		Available: domain.Resources{CPU: 10, MemoryMB: 20_000, Slots: 4},
	}, testNow)
	return in
}

// TestFitsOnceLinuxDrainsAsksOnlyWhatLinuxWorkCannotRelease is issue #439. The
// standing offer of one (ADR 0082) promises a job that it waits only for this
// node's own Linux work. Two maestro guests hold 8 of the mini's 10 CPUs for up
// to their occupancy budget, and nothing a Linux guest does releases them: a
// 4x8 cannot start until they end, a 2x4 can start beside them. A node full of
// Linux work, or a host whose measured residual is spent, fits once that Linux
// work drains.
func TestFitsOnceLinuxDrainsAsksOnlyWhatLinuxWorkCannotRelease(t *testing.T) {
	cfg := testConfig()
	maestros := []domain.Instance{runningInstance(cfg, "maestro-1", "a/repo", "maestro"),
		runningInstance(cfg, "maestro-2", "b/repo", "maestro")}
	larges := []domain.Instance{runningInstance(cfg, "large-1", "a/repo", "large"),
		runningInstance(cfg, "large-2", "b/repo", "large")}
	cases := map[string]struct {
		in      Input
		profile domain.ProfileID
		want    bool
	}{
		"a 4x8 cannot start beside two maestro guests": {
			in: budgetMiniInput(nil, maestros), profile: "large", want: false},
		"a 2x4 starts beside two maestro guests": {
			in: budgetMiniInput(nil, maestros), profile: "medium", want: true},
		"a 4x8 behind Linux work alone waits only for that work": {
			in: budgetMiniInput(nil, larges), profile: "large", want: true},
		"a 4x8 behind one maestro and Linux work waits only for the Linux work": {
			in: budgetMiniInput(nil, append([]domain.Instance{maestros[0]}, larges...)), profile: "large", want: true},
		"a queued maestro takes its vector before the 4x8 can": {
			in:      budgetMiniInput([]domain.Demand{demand("b/repo", 9, time.Minute, "maestro")}, maestros[:1]),
			profile: "large", want: false},
		"a queued Linux demand drains like a running one": {
			in:      budgetMiniInput([]domain.Demand{demand("b/repo", 9, time.Minute, "large")}, maestros[:1]),
			profile: "large", want: true},
		"the measured residual is net of Linux work that drains": {
			in: func() Input {
				in := budgetMiniInput(nil, larges)
				in.Host.Value.Available = domain.Resources{}
				return in
			}(), profile: "large", want: true},
		"a static envelope charges a maestro guest too": {
			in: func() Input {
				in := budgetMiniInput(nil, maestros)
				in.Config.ElasticHostEnvelope = false
				in.Config.HostBudget = domain.Resources{}
				return in
			}(), profile: "large", want: false},
		"a macOS profile keeps ADR 0082's offer": {
			in: budgetMiniInput(nil, maestros), profile: "maestro", want: true},
		"a Linux-only node always fits a vector its envelope can hold": {
			in: input(nil, larges, State{}), profile: "large", want: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := FitsOnceLinuxDrains(tc.in, tc.profile); got != tc.want {
				t.Fatalf("FitsOnceLinuxDrains(%s) = %v, want %v", tc.profile, got, tc.want)
			}
		})
	}
}
