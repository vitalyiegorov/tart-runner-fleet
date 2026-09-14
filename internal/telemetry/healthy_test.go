package telemetry

import (
	"testing"
	"time"
)

// markTicking puts a daemon in the state ADR 0052 calls healthy: a fresh
// successful tick and every critical observation fresh.
func markTicking(t *testing.T, health *Health) {
	t.Helper()
	health.RecordTick(true)
	for _, name := range []string{"github", "host", "tart"} {
		if err := health.RecordObservation(name, ObservationFresh); err != nil {
			t.Fatal(err)
		}
	}
}

// A node that withdrew its sessions under ADR 0047 is NOT ready — it is
// admitting nothing — and IS healthy, because the only thing it stopped
// observing is the thing it deliberately stopped listening to. This is the
// whole of issue #320: without the split, the most quiescent state a node can
// be in is the one state it can never be updated from.
func TestAWithdrawnNodeIsHealthyButNotReady(t *testing.T) {
	health, _ := newTestHealth(t)
	markTicking(t, health)
	if !health.Ready().OK || !health.Healthy().OK {
		t.Fatalf("a serving node was not ready/healthy: ready=%+v healthy=%+v", health.Ready(), health.Healthy())
	}
	if err := health.RecordObservationDetail("github", ObservationStale, SessionYieldedObservation); err != nil {
		t.Fatal(err)
	}
	if health.Ready().OK {
		t.Fatal("a node admitting nothing reported itself ready: readiness must keep its meaning for the scheduler")
	}
	healthy := health.Healthy()
	if !healthy.OK {
		t.Fatalf("a withdrawn node reported itself unhealthy: %v", healthy.Reasons)
	}
	if len(healthy.Reasons) != 0 {
		t.Fatalf("a healthy node carried reasons: %v", healthy.Reasons)
	}
}

// The excuse is narrow on purpose. Any other stale observation, and any
// unavailable one, is a failed observation rather than a decision — including
// the store's own `operations` tick, which is why health still means the store
// is writable.
func TestHealthExcusesOnlyAYieldedObservation(t *testing.T) {
	for name, record := range map[string]func(*Health) error{
		"stale for another reason": func(h *Health) error {
			return h.RecordObservationDetail("github", ObservationStale, "ingest_refused")
		},
		"stale with no detail at all": func(h *Health) error {
			return h.RecordObservation("github", ObservationStale)
		},
		"unavailable while yielded": func(h *Health) error {
			return h.RecordObservationDetail("github", ObservationUnavailable, SessionYieldedObservation)
		},
	} {
		health, _ := newTestHealth(t)
		markTicking(t, health)
		if err := record(health); err != nil {
			t.Fatal(err)
		}
		if health.Healthy().OK {
			t.Fatalf("%s: a failed observation was excused as a withdrawal", name)
		}
	}
}

// A withdrawal is written every tick, so an excused observation still has to be
// fresh. A daemon that stopped ticking while withdrawn must not inherit the
// excuse: that is exactly the fault healthiness exists to catch, and it is the
// one the release transaction would otherwise swap a generation under.
func TestAStoppedLoopIsNotExcusedByItsOwnWithdrawal(t *testing.T) {
	health, clock := newTestHealth(t)
	markTicking(t, health)
	if err := health.RecordObservationDetail("github", ObservationStale, SessionYieldedObservation); err != nil {
		t.Fatal(err)
	}
	if !health.Healthy().OK {
		t.Fatal("a ticking withdrawn node was not healthy")
	}
	clock.Advance(21 * time.Second)
	result := health.Healthy()
	if result.OK {
		t.Fatal("a withdrawal whose own record expired was still excused")
	}
	if len(result.Reasons) == 0 || result.Reasons[0] != "critical_observation_expired" {
		t.Fatalf("reasons = %v, want the expiry named", result.Reasons)
	}
}

// A daemon that has never completed a tick is not healthy however quiet it is:
// the transaction's question is whether this process works, and a process that
// has produced nothing has not answered it.
func TestHealthRequiresASuccessfulTick(t *testing.T) {
	health, _ := newTestHealth(t)
	for _, name := range []string{"github", "host", "tart"} {
		if err := health.RecordObservation(name, ObservationFresh); err != nil {
			t.Fatal(err)
		}
	}
	result := health.Healthy()
	if result.OK || len(result.Reasons) != 1 || result.Reasons[0] != "successful_tick_missing" {
		t.Fatalf("healthy without a tick: %+v", result)
	}
}

// The status document publishes the same judgement the accessor returns, which
// is why it is derived from the snapshot rather than passed in beside it.
func TestStatusEnvelopePublishesHealthAlongsideReadiness(t *testing.T) {
	health, _ := newTestHealth(t)
	markTicking(t, health)
	if err := health.RecordObservationDetail("github", ObservationStale, SessionYieldedObservation); err != nil {
		t.Fatal(err)
	}
	envelope := statusEnvelope(health.Snapshot(), "v", "authority", health.Live(), health.Ready(),
		HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true},
		HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true})
	if envelope.Data.Healthy == nil {
		t.Fatal("the status document published no health field")
	}
	if !envelope.Data.Healthy.OK || envelope.Data.Healthy.Reasons == nil {
		t.Fatalf("healthy = %+v, want a pass with an empty reason set", *envelope.Data.Healthy)
	}
	if envelope.Data.Ready.OK {
		t.Fatal("the status document called a withdrawn node ready")
	}
	if got := envelope.Data.EffectiveHealthy(); !got.OK {
		t.Fatalf("the accessor disagreed with the document: %+v", got)
	}
}
