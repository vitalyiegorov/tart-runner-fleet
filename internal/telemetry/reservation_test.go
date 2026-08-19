package telemetry

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// capHeldReservationMetric is issue #226 as the daemon publishes it: a `c/repo`
// `xl` head its own repository's cap of two holds out, six vCPU reserved for
// thirteen minutes for a head no amount of freed CPU can admit.
func capHeldReservationMetric() *ReservationMetric {
	return &ReservationMetric{Demand: "c/repo/1009/1/500009", Repo: "c/repo", Profile: "builder",
		CPU: 6, MemoryMiB: 12_288, Slots: 1, Held: 13 * time.Minute, Axis: "repository_cap"}
}

// TestStatusAndMetricsPublishTheHeldReservation is the observability gap issue
// #226 exposed, closed end to end.
//
// The defect shipped, was reachable on production caps of 3-4 (and 1 where
// unconfigured), and left no artifact: `grep reservation` over the authority log
// returned nothing at all, because nothing published the reservation, its
// repository, or which axis was holding it. A simulator found it; the fleet
// could not have.
func TestStatusAndMetricsPublishTheHeldReservation(t *testing.T) {
	health, _ := newTestHealth(t)
	if err := health.SetReservation(capHeldReservationMetric()); err != nil {
		t.Fatalf("SetReservation: %v", err)
	}

	envelope := statusEnvelope(health.Snapshot(), "v", "authority", HealthResult{OK: true},
		HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true}, health.Reservation(), health.Progress(),
		health.GuestLiveness(), health.RunnerVersions())
	held := envelope.Data.Reservation
	if held == nil {
		t.Fatal("a held reservation must reach the status document")
	}
	if held.Demand != "c/repo/1009/1/500009" || held.Repo != "c/repo" || held.Axis != "repository_cap" {
		t.Fatalf("the head, its repository and the axis are the whole diagnosis: %#v", held)
	}
	if held.CPU != 6 || held.HeldSeconds != 780 {
		t.Fatalf("the vector and how long it has been held: %#v", held)
	}

	metrics := renderTestMetrics(t, health)
	for _, want := range []string{
		`fleet_reservation_held_seconds{profile="builder"} 780`,
		`fleet_reservation_vector_cpu{profile="builder"} 6`,
		`fleet_reservation_axis{axis="repository_cap"} 1`,
		`fleet_reservation_axis{axis="vector"} 0`,
		`fleet_reservation_axis{axis="unjudged"} 0`,
	} {
		if !strings.Contains(metrics, want) {
			t.Fatalf("metrics missing %q:\n%s", want, metrics)
		}
	}
	// Every axis is emitted on every scrape, so an alert can say "the cap axis
	// has been set for twenty minutes" rather than having to infer it from a
	// series appearing.
	for _, axis := range reservationAxes {
		if !strings.Contains(metrics, `fleet_reservation_axis{axis="`+axis+`"}`) {
			t.Fatalf("the axis vocabulary must be emitted whole, missing %q:\n%s", axis, metrics)
		}
	}
	// `fleet_reservation_lends_vector` was published beside these until ADR 0045.
	// It is deleted rather than pinned to 1: a reservation withholds ORDER and one
	// repository slot on every axis there is, so the gauge had no state left to
	// distinguish.
	if strings.Contains(metrics, "lends_vector") {
		t.Fatalf("ADR 0045 deletes the lending gauge rather than pinning it:\n%s", metrics)
	}
	// The head's demand key and repository are unbounded cardinality and must
	// never become labels.
	if strings.Contains(metrics, "repo=") || strings.Contains(metrics, "demand=") ||
		strings.Contains(metrics, "job=") {
		t.Fatalf("unbounded labels in reservation metrics:\n%s", metrics)
	}
}

// TestFleetHoldingNoReservationPublishesNoneAtAll keeps absence honest. "No
// reservation" and "a reservation nobody published" must not be the same
// observation, because their being the same is what let #226 run unseen.
func TestFleetHoldingNoReservationPublishesNoneAtAll(t *testing.T) {
	health, _ := newTestHealth(t)
	if err := health.SetReservation(capHeldReservationMetric()); err != nil {
		t.Fatalf("SetReservation: %v", err)
	}
	if err := health.SetReservation(nil); err != nil {
		t.Fatalf("clearing a released reservation must succeed: %v", err)
	}
	envelope := statusEnvelope(health.Snapshot(), "v", "authority", HealthResult{OK: true},
		HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true}, health.Reservation(), health.Progress(),
		health.GuestLiveness(), health.RunnerVersions())
	if envelope.Data.Reservation != nil {
		t.Fatalf("a released reservation must disappear, not linger: %#v", envelope.Data.Reservation)
	}
	if metrics := renderTestMetrics(t, health); strings.Contains(metrics, "fleet_reservation_") {
		t.Fatalf("a fleet holding nothing emits no reservation series:\n%s", metrics)
	}
}

// TestReservationCheckFailsOnAHeadNoAxisRefuses separates the ordinary case from
// the incident, and it is the separation issue #235 corrected.
//
// The check used to fail on a reservation that did not LEND its vector. That
// could not survive its own record: ADR 0017 released the vector on one axis,
// ADR 0038 on the other, and ADR 0045 established that a reservation withholds
// ORDER rather than a vector on either — so the only thing left that could make
// the check fire was a plan that published no judgement at all. On the mac studio
// it did, for an hour, and reported "has withheld 2 cpu / 4096 MiB" for a head
// whose vector fit the free envelope exactly.
//
// The alertable state is `none`: no axis refuses the head, so the fleet is
// holding a turn for work it could have started. One such tick is ordinary — the
// tick planned the other platform — so the bound is the queue SLO the fleet
// already carries, and no new knob is introduced.
func TestReservationCheckFailsOnAHeadNoAxisRefuses(t *testing.T) {
	for _, test := range []struct {
		name string
		axis string
		held time.Duration
		ok   bool
	}{
		{"a cap-held head is ordinary", "repository_cap", 13 * time.Minute, true},
		{"a vector-held head is ordinary", "vector", time.Hour, true},
		{"an unjudged plan is not a reservation fault", "", time.Hour, true},
		{"a head no axis refuses, inside the SLO, is one tick of ordinary lane change",
			"none", time.Minute, true},
		{"a head no axis refuses, past the SLO, is the incident", "none", 65 * time.Minute, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			health, _ := newTestHealth(t)
			metric := capHeldReservationMetric()
			metric.Axis, metric.Held = test.axis, test.held
			if err := health.SetReservation(metric); err != nil {
				t.Fatalf("SetReservation: %v", err)
			}
			result := health.Reservation()
			if result.OK != test.ok {
				t.Fatalf("Reservation().OK = %v, want %v: %v", result.OK, test.ok, result.Reasons)
			}
			if test.ok {
				return
			}
			if len(result.Reasons) != 1 {
				t.Fatalf("one reservation, one reason: %v", result.Reasons)
			}
			for _, want := range []string{"c/repo/1009/1/500009", "6 cpu", "12288 MiB", "1h5m0s",
				"no axis refuses"} {
				if !strings.Contains(result.Reasons[0], want) {
					t.Fatalf("the reason must name %q: %s", want, result.Reasons[0])
				}
			}
		})
	}
}

// TestSetReservationRejectsRatherThanClamps applies the rule SetOccupancy
// already follows: a mangled metric must never masquerade as "no vector is
// standing idle". The axis vocabulary is closed because it is a metric LABEL,
// and an open one there is unbounded cardinality.
func TestSetReservationRejectsRatherThanClamps(t *testing.T) {
	for _, test := range []struct {
		name    string
		mutate  func(*ReservationMetric)
		wantErr error
	}{
		{"a negative hold", func(r *ReservationMetric) { r.Held = -time.Second }, errInvalidMetric},
		{"a negative vector", func(r *ReservationMetric) { r.CPU = -1 }, errInvalidMetric},
		{"an axis outside the vocabulary", func(r *ReservationMetric) { r.Axis = "vibes" }, errInvalidMetric},
		{"a profile the process never declared", func(r *ReservationMetric) { r.Profile = "ghost" }, errUnknownProfile},
	} {
		t.Run(test.name, func(t *testing.T) {
			health, _ := newTestHealth(t)
			metric := capHeldReservationMetric()
			test.mutate(metric)
			if err := health.SetReservation(metric); !errors.Is(err, test.wantErr) {
				t.Fatalf("SetReservation = %v, want %v", err, test.wantErr)
			}
			if health.Snapshot().Reservation != nil {
				t.Fatal("a rejected metric must not be published")
			}
		})
	}
	// An unjudged plan carries the reservation without a judgement, and that is
	// grammatical: a plan that judged nothing must not publish a judgement.
	health, _ := newTestHealth(t)
	unjudged := capHeldReservationMetric()
	unjudged.Axis = ""
	if err := health.SetReservation(unjudged); err != nil {
		t.Fatalf("an unjudged axis is legal: %v", err)
	}
	if metrics := renderTestMetrics(t, health); !strings.Contains(metrics, `fleet_reservation_axis{axis="unjudged"} 1`) {
		t.Fatalf("an unjudged plan is published as such:\n%s", metrics)
	}
}

// TestSnapshotDoesNotAliasTheReservation keeps the published document immune to
// a later mutation of the caller's struct, exactly as the occupancy slice is.
func TestSnapshotDoesNotAliasTheReservation(t *testing.T) {
	health, _ := newTestHealth(t)
	metric := capHeldReservationMetric()
	if err := health.SetReservation(metric); err != nil {
		t.Fatalf("SetReservation: %v", err)
	}
	metric.Axis, metric.CPU = "vector", 99
	snapshot := health.Snapshot()
	if snapshot.Reservation.Axis != "repository_cap" || snapshot.Reservation.CPU != 6 {
		t.Fatalf("the snapshot aliases the caller's struct: %#v", snapshot.Reservation)
	}
	snapshot.Reservation.CPU = 1
	if health.Snapshot().Reservation.CPU != 6 {
		t.Fatal("a caller mutating the snapshot must not reach the published document")
	}
}

// renderTestMetrics scrapes the bounded metrics endpoint.
func renderTestMetrics(t *testing.T, health *Health) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	metricsHandler(health)(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("metrics endpoint returned %d", recorder.Code)
	}
	return recorder.Body.String()
}
