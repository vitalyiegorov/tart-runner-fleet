package adminapi

import "testing"

// EffectiveHealthy is the one Effective* accessor whose absence is not a free
// pass, and this test pins that departure. ADR 0052 split a single published
// field in two; a daemon older than it published only readiness, which is the
// strictly stronger claim, so reading readiness in place of health loses
// nothing. Treating absence as a pass would let the update transaction swap a
// generation under a daemon that never said it was working at all.
func TestEffectiveHealthyFallsBackToReadinessOnAnOlderDaemon(t *testing.T) {
	ready := Status{Ready: Check{OK: true}}
	if got := ready.EffectiveHealthy(); !got.OK {
		t.Fatal("a ready daemon older than ADR 0052 was read as unhealthy")
	}
	if got := ready.EffectiveHealthy(); got.Reasons == nil {
		t.Fatal("reasons are nil, which encodes differently from the empty set")
	}
	blocked := Status{Ready: Check{Reasons: []string{"critical_observation_stale"}}}
	got := blocked.EffectiveHealthy()
	if got.OK {
		t.Fatal("an older daemon that refused readiness was read as healthy")
	}
	if len(got.Reasons) != 1 || got.Reasons[0] != "critical_observation_stale" {
		t.Fatalf("reasons = %v, want the daemon's own", got.Reasons)
	}
}

// A daemon that publishes the field is believed verbatim, in both directions:
// the whole point of #320 is a node that is healthy while not ready.
func TestEffectiveHealthyPrefersThePublishedField(t *testing.T) {
	withdrawn := Status{
		Ready:   Check{Reasons: []string{"critical_observation_stale"}},
		Healthy: &Check{OK: true, Reasons: []string{}},
	}
	if got := withdrawn.EffectiveHealthy(); !got.OK || len(got.Reasons) != 0 {
		t.Fatalf("a withdrawn but healthy node read as %+v", got)
	}
	failing := Status{Ready: Check{OK: true}, Healthy: &Check{Reasons: []string{"successful_tick_expired"}}}
	if got := failing.EffectiveHealthy(); got.OK {
		t.Fatalf("a published failure was softened into a pass: %+v", got)
	}
}
