package telemetry

import (
	"strings"
	"testing"
	"time"
)

func membershipStatus(t *testing.T, health *Health) Snapshot {
	t.Helper()
	return health.Snapshot()
}

func membershipEnvelopeCheck(snapshot Snapshot) (*MembershipMetric, bool, []string) {
	envelope := statusEnvelope(snapshot, "v", "authority", HealthResult{OK: true}, HealthResult{OK: true},
		HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true},
		HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true})
	check := envelope.Data.EffectiveMembershipCheck()
	return snapshot.Membership, check.OK, check.Reasons
}

// ADR 0063: a node that declared no membership publishes none, and its absence
// passes -- a node that runs no watchdog has excluded nothing.
func TestMembershipIsAbsentUntilPublishedAndAbsencePasses(t *testing.T) {
	health := yieldHealth(t)
	envelope := statusEnvelope(health.Snapshot(), "v", "authority", HealthResult{OK: true}, HealthResult{OK: true},
		HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true},
		HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true})
	if envelope.Data.Membership != nil || envelope.Data.MembershipCheck == nil || !envelope.Data.MembershipCheck.OK {
		t.Fatalf("an unpublished membership must be absent and passing: %+v %+v", envelope.Data.Membership, envelope.Data.MembershipCheck)
	}
	if check := (envelope.Data).EffectiveMembershipCheck(); !check.OK {
		t.Fatal("absence must pass")
	}
	envelope.Data.MembershipCheck = nil
	if check := envelope.Data.EffectiveMembershipCheck(); !check.OK {
		t.Fatal("a daemon predating the check must pass")
	}
	if metrics := renderMetrics(health.Snapshot()); strings.Contains(metrics, "fleet_membership") {
		t.Fatalf("a node with no watchdog published membership metrics:\n%s", metrics)
	}
}

func TestMembershipFailsOnlyForAnExcludedSetAndNamesEveryStage(t *testing.T) {
	health := yieldHealth(t)
	since := time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC)
	health.SetMembership(MembershipMetric{Mode: "roaming", State: "healing", Bindings: []MembershipBindingMetric{
		{Scope: "knee", ScaleSetID: 19, Stage: "healing", Since: since, Attempts: 1},
		{Scope: "knee", ScaleSetID: 20, Stage: "probation", Since: since, Exclusions: 1},
	}})
	metric, ok, reasons := membershipEnvelopeCheck(membershipStatus(t, health))
	if metric == nil || !ok || len(reasons) != 2 {
		t.Fatalf("healing and probation are evidence, not failures: ok=%v %v", ok, reasons)
	}
	if !strings.Contains(reasons[0], "knee scale set 19") || !strings.Contains(reasons[0], "recycled 1 time") ||
		!strings.Contains(reasons[1], "probation") {
		t.Fatalf("reasons do not describe the stages: %v", reasons)
	}

	health.SetMembership(MembershipMetric{Mode: "stationary", State: "excluded", Bindings: []MembershipBindingMetric{
		{Scope: "knee", ScaleSetID: 19, Stage: "excluded", Since: since, Attempts: 2, Exclusions: 1},
	}})
	snapshot := membershipStatus(t, health)
	_, ok, reasons = membershipEnvelopeCheck(snapshot)
	if ok || len(reasons) != 1 || !strings.Contains(reasons[0], "excluded") || !strings.Contains(reasons[0], "scale-sets recreate") {
		t.Fatalf("an excluded set must fail and name the operator remedy: ok=%v %v", ok, reasons)
	}
	envelope := statusEnvelope(snapshot, "v", "authority", HealthResult{OK: true}, HealthResult{OK: true},
		HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true},
		HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true})
	row := envelope.Data.Membership
	if row == nil || row.Mode != "stationary" || row.State != "excluded" || len(row.Bindings) != 1 ||
		row.Bindings[0].ScaleSetID != 19 || row.Bindings[0].Exclusions != 1 || !row.Bindings[0].Since.Equal(since) {
		t.Fatalf("the versioned surface described membership as %+v", row)
	}
	metrics := renderMetrics(snapshot)
	if !strings.Contains(metrics, `fleet_membership_scale_sets{stage="excluded"} 1`) ||
		!strings.Contains(metrics, `fleet_membership_scale_sets{stage="healing"} 0`) {
		t.Fatalf("membership is not scrapeable:\n%s", metrics)
	}
}
