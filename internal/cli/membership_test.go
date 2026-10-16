package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
)

// ADR 0063: an excluded scale set is this node choosing not to serve it, and
// must never read like a quiet day on either surface an operator looks at.

func excludedMembership() *adminapi.Membership {
	return &adminapi.Membership{Mode: "roaming", State: "excluded", Bindings: []adminapi.MembershipBinding{
		{Scope: "knee", ScaleSetID: 19, Stage: "excluded", Since: time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC), Exclusions: 1},
		{Scope: "knee", ScaleSetID: 20, Stage: "healing", Attempts: 1},
	}}
}

func TestDoctorFailsAndExplainsAnExcludedScaleSet(t *testing.T) {
	status := healthyStatus()
	status.Data.Membership = excludedMembership()
	status.Data.MembershipCheck = &adminapi.Check{Reasons: []string{"knee scale set 19 is excluded since ..."}}
	assertDoctorNames(t, status, "membership", "knee scale set 19 is excluded")
}

func TestDoctorDescribesMembershipWhicheverWayItReads(t *testing.T) {
	status := healthyStatus()
	assertDoctorPasses(t, status, "membership")
	if detail := membershipDetail(status.Data, adminapi.Check{OK: true}); !strings.Contains(detail, "no membership declared") {
		t.Fatalf("an unpublished membership rendered as %q", detail)
	}
	status.Data.Membership = &adminapi.Membership{Mode: "stationary", State: "serving", Bindings: []adminapi.MembershipBinding{}}
	if detail := membershipDetail(status.Data, adminapi.Check{OK: true}); detail != "stationary: every bound scale set is serving" {
		t.Fatalf("a serving membership rendered as %q", detail)
	}
	healing := adminapi.Check{OK: true, Reasons: []string{"knee scale set 20 is wedged and healing"}}
	status.Data.Membership.State = "healing"
	if detail := membershipDetail(status.Data, healing); !strings.Contains(detail, "stationary, healing: knee scale set 20") {
		t.Fatalf("a healing membership rendered as %q", detail)
	}
}

func TestStatusPrintsNonServingScaleSetsAboveTheQueues(t *testing.T) {
	status := healthyStatus()
	status.Data.Membership = excludedMembership()
	rendered := renderYieldStatus(status)
	header := strings.Index(rendered, "MEMBERSHIP EXCLUDED (roaming)")
	if header < 0 || !strings.Contains(rendered, "knee/19  excluded") || !strings.Contains(rendered, "knee/20  healing") {
		t.Fatalf("an excluded set rendered without saying so:\n%s", rendered)
	}
	if queues := strings.Index(rendered, "QUEUES"); queues < header {
		t.Fatal("membership printed below the queues it explains")
	}
	status.Data.Membership = &adminapi.Membership{Mode: "roaming", State: "serving", Bindings: []adminapi.MembershipBinding{}}
	if rendered := renderYieldStatus(status); strings.Contains(rendered, "MEMBERSHIP") {
		t.Fatalf("a serving node announced its membership:\n%s", rendered)
	}
}
