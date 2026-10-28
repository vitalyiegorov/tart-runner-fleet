package telemetry

import (
	"testing"
	"time"
)

// Issue #449: one scope's REST inventory went stale and every node read NOT
// READY and also unhealthy, so the Mac mini drained to zero instances and then
// refused the release it drained for. A REST inventory that has answered at
// least once in this process and is still being written is a working loop
// reporting an external answer; health excuses it, readiness does not, and the
// scale-set observations keep no such excuse. Unavailable and expired records
// are never excused: a candidate that never took an inventory, or a loop that
// stopped writing one, is exactly what the release transaction must refuse.
func TestHealthExcusesAStaleRESTInventoryStillBeingWritten(t *testing.T) {
	inventory := RESTInventoryObservationPrefix + "budgie-org"
	newHealth := func(t *testing.T) (*Health, *fakeClock) {
		t.Helper()
		clock := &fakeClock{now: time.Unix(1_700_000_000, 0).UTC()}
		health, err := NewHealth(clock, HealthConfig{ReadyTickTTL: 10 * time.Second, LiveTickTTL: time.Minute,
			CriticalObservationTTL: 20 * time.Second, Profiles: []string{"builder"},
			CriticalObservations: []string{"github", "operations", inventory}})
		if err != nil {
			t.Fatal(err)
		}
		health.RecordTick(true)
		for _, name := range []string{"github", "operations", inventory} {
			if err := health.RecordObservation(name, ObservationFresh); err != nil {
				t.Fatal(err)
			}
		}
		return health, clock
	}

	health, clock := newHealth(t)
	if err := health.RecordObservationDetail(inventory, ObservationStale, "queue_observation_stale"); err != nil {
		t.Fatal(err)
	}
	if healthy := health.Healthy(); !healthy.OK {
		t.Fatalf("a stale REST inventory still being written made the node unhealthy: %v", healthy.Reasons)
	}
	if ready := health.Ready(); ready.OK || len(ready.Reasons) != 1 || ready.Reasons[0] != "critical_observation_stale" {
		t.Fatalf("readiness must still name the stale inventory: %+v", ready)
	}
	clock.Advance(21 * time.Second)
	health.RecordTick(true)
	for _, name := range []string{"github", "operations"} {
		_ = health.RecordObservation(name, ObservationFresh)
	}
	if healthy := health.Healthy(); healthy.OK || healthy.Reasons[0] != "critical_observation_expired" {
		t.Fatalf("an inventory loop that stopped writing was excused: %+v", healthy)
	}

	for name, record := range map[string]func(*Health) error{
		"an inventory never taken in this process": func(h *Health) error {
			return h.RecordObservationDetail(inventory, ObservationUnavailable, "rate_limited")
		},
		"a stale scale-set stream": func(h *Health) error {
			return h.RecordObservationDetail("github", ObservationStale, "queue_observation_stale")
		},
		"the store's own tick": func(h *Health) error {
			return h.RecordObservationDetail("operations", ObservationStale, "")
		},
	} {
		health, _ := newHealth(t)
		if err := record(health); err != nil {
			t.Fatal(err)
		}
		if health.Healthy().OK {
			t.Fatalf("%s was excused from health", name)
		}
	}
}
