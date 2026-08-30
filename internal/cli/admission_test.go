package cli

import (
	"strings"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
)

// TestTheAdmissionLineNamesARefusalAndItsMeasurement is issue #286 at the
// artifact an operator reads.
//
// On 2026-08-25 the mac studio starved the whole fleet for about two and a half
// hours. `fleet doctor` reported 10 of 11 checks OK and named only the breached
// queue SLO — a symptom with a dozen causes — while the one fact that explained
// everything sat unread in `fleet status`.
func TestTheAdmissionLineNamesARefusalAndItsMeasurement(t *testing.T) {
	status := healthyStatus()
	refusal := adminapi.Check{OK: false,
		Reasons: []string{"node is admitting no work: disk reserve (disk 58 GiB free, floor 60)"}}
	status.Data.AdmissionCheck = &refusal

	detail := admissionDetail(status.Data, status.Data.EffectiveAdmissionCheck())

	for _, want := range []string{"admitting no work", "disk reserve", "58", "60"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("the admission line must carry %q: %q", want, detail)
		}
	}
}

// A daemon that predates the check says so, rather than printing a bare "ok".
// A check that cannot tell "healthy" from "not asked" reproduces the silence
// this issue is about, with an extra line.
func TestAnOlderDaemonSaysItIsNotReporting(t *testing.T) {
	status := healthyStatus()

	detail := admissionDetail(status.Data, status.Data.EffectiveAdmissionCheck())

	if detail != "not reported by this daemon" {
		t.Fatalf("an absent check must say so: %q", detail)
	}
	if !status.Data.EffectiveAdmissionCheck().OK {
		t.Fatal("silence must not be rendered as a failure either: a rolling update would fail every node")
	}
}

// An admitting node whose margins are all comfortable still says something an
// operator can read.
func TestAnAdmittingNodeSaysSo(t *testing.T) {
	status := healthyStatus()
	passing := adminapi.Check{OK: true}
	status.Data.AdmissionCheck = &passing

	if detail := admissionDetail(status.Data, passing); detail != "admitting; no configured floor is near" {
		t.Fatalf("admission detail = %q", detail)
	}
}

// A passing check that carries a margin prints the margin: the studio's free
// disk slid 72 to 54 GiB over six hours, and nothing made that visible until it
// arrived.
func TestAPassingCheckStillPrintsItsMargin(t *testing.T) {
	status := healthyStatus()
	passing := adminapi.Check{OK: true, Reasons: []string{"nearest floor: disk 72 GiB free, floor 60"}}
	status.Data.AdmissionCheck = &passing

	if detail := admissionDetail(status.Data, passing); !strings.Contains(detail, "disk 72") {
		t.Fatalf("a margin must survive to the line: %q", detail)
	}
}
