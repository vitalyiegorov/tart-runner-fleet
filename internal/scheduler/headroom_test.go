package scheduler

import (
	"math"
	"slices"
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
// MacBook starts two 2x4 guests, and two scopes each bind a 2x4 set on it.
// Every set used to be offered both; together they are offered two, one each,
// whichever set the rotation puts first.
func TestPartitionHeadroomOffersTheResidualOnceAcrossSets(t *testing.T) {
	claims := []Claim{{Profile: "medium", Room: math.MaxInt}, {Profile: "medium", Room: math.MaxInt}}
	for step := range 2 {
		in := macbookInput(nil, nil)
		in.Now = in.Now.Add(time.Duration(step) * shareRotation)
		shares, ok := PartitionHeadroom(in, claims)
		if !ok || shares[0] != 1 || shares[1] != 1 {
			t.Fatalf("rotation %d: shares %v, want one each", step, shares)
		}
	}
}

// TestPartitionHeadroomNeverStrandsASetWithRoom is issue #412: three scopes bind
// a 2x4 set on an idle node that starts two. Dividing the two left the third
// set advertising nothing, and GitHub, which places a job only on a set
// advertising room when the job is queued and never moves it, left that scope's
// jobs queued for hours while the node sat idle. A set whose vector the
// residual can start is offered at least one, whichever way the rotation turns.
func TestPartitionHeadroomNeverStrandsASetWithRoom(t *testing.T) {
	claims := []Claim{{Profile: "medium", Room: math.MaxInt}, {Profile: "medium", Room: math.MaxInt},
		{Profile: "medium", Room: math.MaxInt}}
	for step := range 3 {
		in := macbookInput(nil, nil)
		in.Now = in.Now.Add(time.Duration(step) * shareRotation)
		shares, ok := PartitionHeadroom(in, claims)
		if !ok {
			t.Fatal("usable observations must be sized")
		}
		for index, share := range shares {
			if share != 1 {
				t.Fatalf("rotation %d: set %d offers %d: %v; every set offers one, and none takes both", step, index, share, shares)
			}
		}
	}
	maestro := runningInstance(testConfig(), "maestro-1", "a/repo", "maestro")
	full := macbookInput(nil, []domain.Instance{maestro, runningInstance(testConfig(), "medium-1", "b/repo", "medium")})
	if shares, _ := PartitionHeadroom(full, claims); shareTotal(shares) != 0 {
		t.Fatalf("a node with no free slot offers nothing beyond what it holds, floor or not: %v", shares)
	}
	if shares, _ := PartitionHeadroom(macbookInput(nil, nil), []Claim{{Profile: "medium", Room: 0},
		{Profile: "medium", Room: math.MaxInt}}); shares[0] != 0 || shares[1] != 2 {
		t.Fatalf("a set at its configured capacity is never floored: %v", shares)
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
// set on the MacBook draw on one 10 CPU / 14 GiB budget with two slots. The
// round robin charges every grant against that one budget, so no set is handed
// a second instance at the expense of a sibling; a set it leaves with nothing
// is floored at the one instance it could start (issue #412).
func TestPartitionHeadroomChargesEveryProfileAgainstOneBudget(t *testing.T) {
	in := macbookInput(nil, nil)
	shares, ok := PartitionHeadroom(in, []Claim{{Profile: "large", Room: math.MaxInt},
		{Profile: "medium", Room: math.MaxInt}, {Profile: "large", Room: math.MaxInt}})
	if !ok {
		t.Fatal("usable observations must be sized")
	}
	for index, share := range shares {
		if share != 1 {
			t.Fatalf("shares %v: set %d offers %d, want one each", shares, index, share)
		}
	}
}

// TestPartitionHeadroomSharesAMacOSCohortAcrossItsSets: MaxActive bounds the
// profile on the node, not each set. The round robin hands out the cohort's
// free places once; a set it leaves with nothing is floored at one while a
// place is free (issue #412), and no set is offered more than the cohort has.
func TestPartitionHeadroomSharesAMacOSCohortAcrossItsSets(t *testing.T) {
	in := input(nil, nil, State{})
	shares, ok := PartitionHeadroom(in, []Claim{{Profile: "maestro", Room: math.MaxInt},
		{Profile: "maestro", Room: math.MaxInt}, {Profile: "maestro", Room: math.MaxInt}})
	if !ok || shareTotal(shares) != 3 || slices.Max(shares) != 1 {
		t.Fatalf("shares %v; two free maestro places are handed out once and the third set is floored at one", shares)
	}
	running := runningInstance(testConfig(), "maestro-1", "a/repo", "maestro")
	shares, _ = PartitionHeadroom(input(nil, []domain.Instance{running}, State{}),
		[]Claim{{Profile: "maestro", Room: math.MaxInt}, {Profile: "maestro", Room: math.MaxInt}})
	if shares[0] != 1 || shares[1] != 1 {
		t.Fatalf("shares %v; one running maestro leaves one place, which each set may offer", shares)
	}
	full := []domain.Instance{running, runningInstance(testConfig(), "maestro-2", "b/repo", "maestro")}
	if shares, _ := PartitionHeadroom(input(nil, full, State{}), []Claim{{Profile: "maestro", Room: math.MaxInt}}); shares[0] != 0 {
		t.Fatalf("a cohort at MaxActive offers nothing, floor or not: %v", shares)
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
