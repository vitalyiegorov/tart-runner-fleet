package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
)

// capHeldReservation is issue #226 as the CLI receives it: a `c/repo` `xl` head
// its own repository's cap of two is holding out, with six vCPU reserved for a
// head that no amount of freed CPU can admit.
func capHeldReservation() adminapi.Reservation {
	return adminapi.Reservation{Demand: "c/repo/1009/1/500009", Repo: "c/repo", Profile: "xl",
		CPU: 6, MemoryMiB: 12_288, Slots: 1, HeldSeconds: 780, Axis: "repository_cap", LendsVector: true}
}

func reservationStatus(held *adminapi.Reservation, check *adminapi.Check) adminapi.StatusEnvelope {
	status := healthyStatus()
	status.Data.Reservation = held
	status.Data.ReservationCheck = check
	return status
}

// TestReservationSectionNamesTheHeadItsRepositoryAndTheAxis is the whole reason
// this section exists.
//
// Issue #226 shipped, was reachable, and left no artifact: nothing published
// named the held reservation, its repository, or which of the two axes held it,
// so a defect that stranded six vCPU for the entire runtime of a blocking job
// was invisible to the fleet running it and was found only by a simulator.
// Every one of those three facts has to appear.
func TestReservationSectionNamesTheHeadItsRepositoryAndTheAxis(t *testing.T) {
	var buffer bytes.Buffer
	renderReservation(&buffer, capHeldReservation())
	out := buffer.String()
	for _, want := range []string{"HEAD", "PROFILE", "AXIS", "c/repo", "xl", "6 cpu", "12288 MiB",
		"13m0s", "repository_cap", "c/repo/1009/1/500009", "lent to work it outranks"} {
		if !strings.Contains(out, want) {
			t.Fatalf("reservation section missing %q:\n%s", want, out)
		}
	}
}

// TestReservationSectionSaysWhenAVectorIsStandingIdle pins the expensive
// reading. A reservation that lends the vector it cannot use costs the fleet
// nothing but the head's own wait; one that withholds it is an idle vector the
// size of the head's profile for as long as whatever blocks the head runs.
func TestReservationSectionSaysWhenAVectorIsStandingIdle(t *testing.T) {
	withholding := capHeldReservation()
	withholding.LendsVector = false
	var buffer bytes.Buffer
	renderReservation(&buffer, withholding)
	if out := buffer.String(); !strings.Contains(out, "idle") {
		t.Fatalf("a withheld vector must read as idle:\n%s", out)
	}
}

// TestStatusOmitsTheReservationSectionWhenNothingIsHeld keeps absence honest:
// a fleet holding nothing prints no section, so "no reservation" and "a
// reservation nobody published" cannot look the same.
func TestStatusOmitsTheReservationSectionWhenNothingIsHeld(t *testing.T) {
	var buffer bytes.Buffer
	renderStatus(&buffer, reservationStatus(nil, &adminapi.Check{OK: true}))
	if out := buffer.String(); strings.Contains(out, "RESERVATION") {
		t.Fatalf("nothing is held, so no reservation section belongs in the document:\n%s", out)
	}
}

// TestStatusReportsAWithheldVectorAsDegraded proves the judgement reaches the
// headline. An operator reading `fleet status` during an incident must see the
// state change, not have to notice a new section.
func TestStatusReportsAWithheldVectorAsDegraded(t *testing.T) {
	held := capHeldReservation()
	held.LendsVector = false
	check := &adminapi.Check{OK: false, Reasons: []string{"reservation for c/repo/1009/1/500009 of profile xl " +
		"has withheld 6 cpu / 12288 MiB for 13m0s on the repository_cap axis"}}
	var buffer bytes.Buffer
	renderStatus(&buffer, reservationStatus(&held, check))
	out := buffer.String()
	for _, want := range []string{"DEGRADED", "reservation:", "repository_cap axis", "RESERVATION"} {
		if !strings.Contains(out, want) {
			t.Fatalf("status missing %q:\n%s", want, out)
		}
	}
}

// TestDoctorNamesTheReservationEvenWhenItPasses is the check that would have
// made issue #226 visible. A cap-held head that lends its vector is healthy, so
// the check PASSES — but a passing check with an empty detail would leave the
// reservation exactly as unobservable as it was before.
func TestDoctorNamesTheReservationEvenWhenItPasses(t *testing.T) {
	held := capHeldReservation()
	client := &fakeClient{status: reservationStatus(&held, &adminapi.Check{OK: true}), metrics: "fleet_up 1"}
	var stdout, stderr bytes.Buffer
	if code := runDoctor(context.Background(), client, "", &stdout, &stderr); code != exitSuccess {
		t.Fatalf("a lending reservation is healthy, got exit %d: %s", code, stdout.String())
	}
	out := stdout.String()
	for _, want := range []string{"reservation", "c/repo/1009/1/500009", "repository_cap", "lending its vector"} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor must name the reservation it passed on, missing %q:\n%s", want, out)
		}
	}
}

// TestDoctorFailsOnAVectorStandingIdle is the alertable half.
func TestDoctorFailsOnAVectorStandingIdle(t *testing.T) {
	held := capHeldReservation()
	held.LendsVector = false
	check := &adminapi.Check{OK: false, Reasons: []string{"reservation for c/repo/1009/1/500009 of profile xl " +
		"has withheld 6 cpu / 12288 MiB for 13m0s on the repository_cap axis"}}
	client := &fakeClient{status: reservationStatus(&held, check), metrics: "fleet_up 1"}
	var stdout, stderr bytes.Buffer
	if code := runDoctor(context.Background(), client, "json", &stdout, &stderr); code != exitDegraded {
		t.Fatalf("a withheld vector is a fault, got exit %d: %s", code, stdout.String())
	}
	out := stdout.String()
	for _, want := range []string{`"name": "reservation"`, `"ok": false`, "repository_cap"} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor JSON missing %q:\n%s", want, out)
		}
	}
}

// TestDoctorPassesWhenTheDaemonNeverPublishedAReservation is the fleet.v1
// handoff rule: a daemon that cannot see a condition must never be rendered as
// having found none, and it must not be rendered as a fault either.
func TestDoctorPassesWhenTheDaemonNeverPublishedAReservation(t *testing.T) {
	status := healthyStatus()
	status.Data.Reservation, status.Data.ReservationCheck = nil, nil
	client := &fakeClient{status: status, metrics: "fleet_up 1"}
	var stdout, stderr bytes.Buffer
	if code := runDoctor(context.Background(), client, "", &stdout, &stderr); code != exitSuccess {
		t.Fatalf("an older daemon publishes no reservation and that is not a fault: %s", stdout.String())
	}
	if out := stdout.String(); !strings.Contains(out, "no reservation held") {
		t.Fatalf("doctor must say so plainly:\n%s", out)
	}
}
