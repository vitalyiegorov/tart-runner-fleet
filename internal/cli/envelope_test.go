package cli

import (
	"strings"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
)

// studioEnvelope is the mac studio's tick of 2026-08-23 as the CLI receives it:
// a 2 CPU / 9216 MiB / 3 slot residual behind one live macos-4x7, which is the
// envelope the whole of issue #263 was argued about without anyone being able to
// read it.
func studioEnvelope() *adminapi.Envelope {
	return &adminapi.Envelope{CPU: 2, MemoryMiB: 9216, Slots: 3,
		AgedCPU: 2, AgedMemoryMiB: 9216, AgedSlots: 3}
}

// TestReservationDetailNamesTheEnvelopeTheAxisWasDecidedAgainst is issue #263.
//
// The studio held a reservation on the `vector` axis for 75 minutes for a head
// of 2 CPU / 4096 MiB. Every published number said that head fitted. Answering
// "against what?" took hours of SSH archaeology across six configuration knobs
// and reached the wrong conclusion. The axis and the envelope have to appear in
// the same sentence.
func TestReservationDetailNamesTheEnvelopeTheAxisWasDecidedAgainst(t *testing.T) {
	status := reservationStatus(&adminapi.Reservation{Demand: "knee-doctor/32661314068", Repo: "knee-doctor",
		Profile: "linux-2x4", CPU: 2, MemoryMiB: 4096, Slots: 1, HeldSeconds: 4499, Axis: "vector"}, nil)
	status.Data.Envelope = studioEnvelope()

	detail := reservationDetail(status.Data, adminapi.Check{OK: true})

	for _, want := range []string{"vector axis", "envelope", "2 cpu", "9216 MiB", "3 slots"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("reservation detail %q omits %q", detail, want)
		}
	}
}

// The two envelopes are quoted separately only when they differ. They differ by
// exactly the advisory CPU-idle clamp, which aged work does not pay, and that
// gap is the arithmetic an operator cannot otherwise reconstruct.
func TestReservationDetailSeparatesTheAgedEnvelopeFromTheYoungOneWhenTheyDiffer(t *testing.T) {
	status := reservationStatus(&adminapi.Reservation{Demand: "a/repo/1/1/1", Repo: "a/repo",
		Profile: "large", CPU: 4, MemoryMiB: 8192, Slots: 1, HeldSeconds: 60, Axis: "vector"}, nil)
	status.Data.Envelope = &adminapi.Envelope{CPU: 0, MemoryMiB: 8192, Slots: 4,
		AgedCPU: 6, AgedMemoryMiB: 8192, AgedSlots: 4}

	detail := reservationDetail(status.Data, adminapi.Check{OK: true})

	if !strings.Contains(detail, "6 cpu / 8192 MiB / 4 slots aged") {
		t.Fatalf("the aged envelope must be named: %q", detail)
	}
	if !strings.Contains(detail, "0 cpu / 8192 MiB / 4 slots young") {
		t.Fatalf("a young envelope that differs must be named beside it: %q", detail)
	}
}

// A host whose two envelopes agree says so once. Repeating identical numbers is
// noise in the line an operator reads on every quiet tick.
func TestReservationDetailQuotesOneEnvelopeWhenTheClampIsNotBinding(t *testing.T) {
	status := reservationStatus(nil, nil)
	status.Data.Envelope = studioEnvelope()

	detail := reservationDetail(status.Data, adminapi.Check{OK: true})

	if strings.Contains(detail, "young") {
		t.Fatalf("identical envelopes must not be printed twice: %q", detail)
	}
	if !strings.Contains(detail, "no reservation held") || !strings.Contains(detail, "envelope 2 cpu") {
		t.Fatalf("an unreserved tick still publishes what it had: %q", detail)
	}
}

// A daemon that predates the field omits it, and the line must read exactly as
// it did before rather than growing an empty clause.
func TestReservationDetailOmitsTheEnvelopeAnOlderDaemonDoesNotPublish(t *testing.T) {
	status := reservationStatus(nil, nil)

	if detail := reservationDetail(status.Data, adminapi.Check{OK: true}); detail != "no reservation held" {
		t.Fatalf("an absent envelope must add nothing: %q", detail)
	}
}
