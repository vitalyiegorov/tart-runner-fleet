package scheduler

import (
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

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
			got, ok := Headroom(tc.in, tc.profile)
			if !ok || got != tc.want {
				t.Fatalf("Headroom(%s) = %d, %v; want %d, true", tc.profile, got, ok, tc.want)
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
		if got, ok := Headroom(in, "large"); ok {
			t.Fatalf("%s: Headroom = %d, known; an unusable observation must be unknown", name, got)
		}
	}
	if got, ok := Headroom(macbookInput(nil, nil), "nonexistent"); ok {
		t.Fatalf("an unknown profile has no vector to count, got %d known", got)
	}
}
