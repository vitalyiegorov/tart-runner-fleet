package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
)

// A node that withdrew its scale-set sessions must not read like a healthy idle
// one. That indistinguishability is what let one node hold a sibling's work for
// eleven hours (#292), so both surfaces an operator actually looks at — the
// doctor check and the status page — have to say it.

func TestDoctorFailsAndExplainsAWithdrawnNode(t *testing.T) {
	const reason = "this node withdrew its scale-set sessions (disk reserve); GitHub binds new jobs to a sibling"
	status := healthyStatus()
	status.Data.SessionYield = &adminapi.SessionYield{Yielded: true, Reason: "disk reserve", Bindings: 6, Withdrawn: 6}
	status.Data.SessionYieldCheck = &adminapi.Check{Reasons: []string{reason}}
	assertDoctorNames(t, status, "session yield", "withdrew its scale-set sessions")
}

func TestDoctorPassesWhileTheNodeHoldsItsSessions(t *testing.T) {
	status := healthyStatus()
	status.Data.SessionYield = &adminapi.SessionYield{Bindings: 6}
	status.Data.SessionYieldCheck = &adminapi.Check{OK: true}
	assertDoctorPasses(t, status, "session yield")
}

// An older daemon publishes no yield posture at all. Absence is a pass, and the
// detail says so rather than implying this node is serving.
func TestDoctorReportsAnUnpublishedYieldPostureAsUnknown(t *testing.T) {
	status := healthyStatus()
	assertDoctorPasses(t, status, "session yield")
	if detail := sessionYieldDetail(status.Data, adminapi.Check{OK: true}); detail != "not reported by this daemon" {
		t.Fatalf("unpublished posture rendered as %q", detail)
	}
}

func TestStatusPrintsWithdrawnSessionsAboveTheQueues(t *testing.T) {
	status := healthyStatus()
	status.Data.SessionYield = &adminapi.SessionYield{Yielded: true, Reason: "disk reserve", Bindings: 6, Withdrawn: 5}
	rendered := renderYieldStatus(status)
	if !strings.Contains(rendered, "SESSIONS WITHDRAWN") {
		t.Fatalf("a withdrawn node rendered without saying so:\n%s", rendered)
	}
	if !strings.Contains(rendered, "5 of 6") || !strings.Contains(rendered, "disk reserve") {
		t.Fatalf("the withdrawal did not carry its count and cause:\n%s", rendered)
	}
	withdrawn := strings.Index(rendered, "SESSIONS WITHDRAWN")
	if queues := strings.Index(rendered, "QUEUES"); queues < withdrawn {
		t.Fatal("the withdrawal printed below the queues it explains")
	}
}

func TestStatusOfAServingNodeSaysNothingAboutSessions(t *testing.T) {
	status := healthyStatus()
	status.Data.SessionYield = &adminapi.SessionYield{Bindings: 6}
	if rendered := renderYieldStatus(status); strings.Contains(rendered, "SESSIONS WITHDRAWN") {
		t.Fatalf("a serving node announced a withdrawal:\n%s", rendered)
	}
}

// A withdrawal with no admission reason still explains itself rather than
// printing an empty parenthesis.
func TestStatusNamesADefaultCauseWhenAdmissionGaveNone(t *testing.T) {
	status := healthyStatus()
	status.Data.SessionYield = &adminapi.SessionYield{Yielded: true, Bindings: 2, Withdrawn: 2}
	if rendered := renderYieldStatus(status); !strings.Contains(rendered, "admission refused") {
		t.Fatalf("a reasonless withdrawal rendered as:\n%s", rendered)
	}
}

func renderYieldStatus(status adminapi.StatusEnvelope) string {
	var buffer bytes.Buffer
	renderStatus(&buffer, status)
	return buffer.String()
}
