package cli

import (
	"strings"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
)

// A node draining toward its own update is healthy and admitting nothing, which
// is exactly what a fault looks like from outside. Both surfaces an operator
// reads have to say which it is (ADR 0048, issues #230 and #282).

func TestDoctorFailsAndExplainsANodeDrainingTowardItsUpdate(t *testing.T) {
	const reason = "this node is refusing admission to reach the quiescence v0.1.510 needs; running work finishes untouched"
	status := healthyStatus()
	status.Data.UpdateDrain = &adminapi.UpdateDrain{Draining: true, Candidate: "v0.1.510"}
	status.Data.UpdateDrainCheck = &adminapi.Check{Reasons: []string{reason}}
	assertDoctorNames(t, status, "update drain", "refusing admission")
}

func TestDoctorPassesWhileTheNodeAdmitsNormally(t *testing.T) {
	status := healthyStatus()
	status.Data.UpdateDrain = &adminapi.UpdateDrain{Candidate: "v0.1.510"}
	status.Data.UpdateDrainCheck = &adminapi.Check{OK: true}
	assertDoctorPasses(t, status, "update drain")

	// The detail distinguishes "a candidate is waiting but we are still serving"
	// from "there is nothing to install" — the first is the state that becomes a
	// drain in half an hour.
	waiting := updateDrainDetail(status.Data, adminapi.Check{OK: true})
	if !strings.Contains(waiting, "v0.1.510") || !strings.Contains(waiting, "admitting normally") {
		t.Fatalf("waiting candidate rendered as %q", waiting)
	}
	status.Data.UpdateDrain = &adminapi.UpdateDrain{}
	if current := updateDrainDetail(status.Data, adminapi.Check{OK: true}); !strings.Contains(current, "newest generation") {
		t.Fatalf("up-to-date node rendered as %q", current)
	}
}

func TestDoctorReportsAnUnpublishedDrainPostureAsUnknown(t *testing.T) {
	status := healthyStatus()
	assertDoctorPasses(t, status, "update drain")
	if detail := updateDrainDetail(status.Data, adminapi.Check{OK: true}); detail != "not reported by this daemon" {
		t.Fatalf("unpublished posture rendered as %q", detail)
	}
}

func TestStatusPrintsTheDrainAboveTheQueuesItExplains(t *testing.T) {
	status := healthyStatus()
	status.Data.UpdateDrain = &adminapi.UpdateDrain{Draining: true, Candidate: "v0.1.510+main.b"}
	rendered := renderYieldStatus(status)
	if !strings.Contains(rendered, "UPDATE DRAIN") || !strings.Contains(rendered, "v0.1.510+main.b") {
		t.Fatalf("a draining node rendered without saying so:\n%s", rendered)
	}
	if drain, queues := strings.Index(rendered, "UPDATE DRAIN"), strings.Index(rendered, "QUEUES"); queues < drain {
		t.Fatal("the drain printed below the queue it explains")
	}
	// A nameless drain still explains itself.
	status.Data.UpdateDrain = &adminapi.UpdateDrain{Draining: true}
	if nameless := renderYieldStatus(status); !strings.Contains(nameless, "a newer generation") {
		t.Fatalf("nameless drain rendered as:\n%s", nameless)
	}
}

func TestStatusOfANodeAdmittingNormallySaysNothingAboutDraining(t *testing.T) {
	status := healthyStatus()
	status.Data.UpdateDrain = &adminapi.UpdateDrain{Candidate: "v0.1.510"}
	if rendered := renderYieldStatus(status); strings.Contains(rendered, "UPDATE DRAIN") {
		t.Fatalf("a serving node announced a drain:\n%s", rendered)
	}
}

// guestConsoleDetail's "no Linux guests" branch is the common case on a macOS
// node and had no test: a node that boots none has no console to lose, and the
// detail has to say that rather than implying a missing sink.
func TestGuestConsoleDetailDistinguishesEveryPosture(t *testing.T) {
	status := healthyStatus()
	for name, test := range map[string]struct {
		console *adminapi.GuestConsole
		want    string
	}{
		"unpublished":         {nil, "not reported by this daemon"},
		"no linux guests":     {&adminapi.GuestConsole{}, "no Linux guests booted on this node"},
		"sink not configured": {&adminapi.GuestConsole{BootsLinuxGuests: true}, "serial console NOT captured"},
		"sink configured": {&adminapi.GuestConsole{BootsLinuxGuests: true, SerialLogConfigured: true},
			"serial console captured"},
	} {
		t.Run(name, func(t *testing.T) {
			status.Data.GuestConsole = test.console
			if got := guestConsoleDetail(status.Data, adminapi.Check{OK: true}); got != test.want {
				t.Fatalf("guestConsoleDetail = %q, want %q", got, test.want)
			}
		})
	}

	// A failing check reports its reasons rather than the posture, so the thing
	// an operator must act on is never displaced by a description.
	status.Data.GuestConsole = &adminapi.GuestConsole{BootsLinuxGuests: true}
	failing := adminapi.Check{Reasons: []string{"writes no guest console"}}
	if got := guestConsoleDetail(status.Data, failing); !strings.Contains(got, "writes no guest console") {
		t.Fatalf("a failing check rendered as %q", got)
	}
}
