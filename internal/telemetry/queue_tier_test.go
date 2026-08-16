package telemetry

import (
	"testing"
	"time"
)

func tierRows() []ScopeQueueMetrics {
	return []ScopeQueueMetrics{{Scope: "vitalyiegorov", Profile: "builder", ScaleSetID: 7, Count: 3,
		OldestEnqueuedAt: time.Unix(900, 0).UTC(), Tiers: []QueueTierMetrics{
			{Tier: "release", Rank: 1, Count: 1, OldestEnqueuedAt: time.Unix(940, 0).UTC()},
			{Tier: "default", Count: 2, OldestEnqueuedAt: time.Unix(900, 0).UTC()},
		}}}
}

func TestScopeQueueTiersArePublishedAndCopied(t *testing.T) {
	health := scopeQueueHealth(t)
	rows := tierRows()
	if err := health.SetScopeQueues(rows); err != nil {
		t.Fatal(err)
	}
	rows[0].Tiers[0].Tier = "mutated"

	snapshot := health.Snapshot()
	if len(snapshot.ScopeQueues) != 1 || len(snapshot.ScopeQueues[0].Tiers) != 2 {
		t.Fatalf("published rows = %#v", snapshot.ScopeQueues)
	}
	if snapshot.ScopeQueues[0].Tiers[0].Tier != "release" {
		t.Fatalf("published tier shares storage with its caller: %#v", snapshot.ScopeQueues[0].Tiers)
	}
}

func TestScopeQueueTiersMustBeNamedAndNonNegative(t *testing.T) {
	health := scopeQueueHealth(t)
	for _, rows := range [][]ScopeQueueMetrics{
		{{Scope: "s", Profile: "p", Tiers: []QueueTierMetrics{{Tier: "", Count: 1}}}},
		{{Scope: "s", Profile: "p", Tiers: []QueueTierMetrics{{Tier: "release", Count: -1}}}},
	} {
		if err := health.SetScopeQueues(rows); err == nil {
			t.Fatalf("SetScopeQueues(%#v) accepted an invalid tier row", rows)
		}
	}
}

func TestStatusEnvelopeCarriesTheTierBreakdown(t *testing.T) {
	health := scopeQueueHealth(t)
	if err := health.SetScopeQueues(tierRows()); err != nil {
		t.Fatal(err)
	}
	envelope := statusEnvelope(health.Snapshot(), "v", "authority", HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true},
		HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true})
	if len(envelope.Data.ScopeQueues) != 1 {
		t.Fatalf("scope queues = %#v", envelope.Data.ScopeQueues)
	}
	tiers := envelope.Data.ScopeQueues[0].Tiers
	if len(tiers) != 2 || tiers[0].Tier != "release" || tiers[0].Jobs != 1 || tiers[0].OldestAgeSeconds <= 0 {
		t.Fatalf("published tiers = %#v", tiers)
	}
}
