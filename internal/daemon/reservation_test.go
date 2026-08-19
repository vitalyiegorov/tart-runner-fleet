package daemon

import (
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/telemetry"
)

// reservationHealthForTest declares the profile a reservation names, because
// SetReservation admits only profiles the process itself declared -- a mistyped
// profile must never open a new time series.
func reservationHealthForTest(t *testing.T) *telemetry.Health {
	t.Helper()
	health, err := telemetry.NewHealth(wallClock{}, telemetry.HealthConfig{
		CriticalObservations: []string{"scheduler"}, Profiles: []string{"builder"}})
	if err != nil {
		t.Fatal(err)
	}
	return health
}

// TestEngineTickerPublishesTheHeldReservationAndItsAxis closes the observability
// gap issue #226 exposed, at the seam that reaches both `/v1/status` and
// `/metrics`.
//
// The defect shipped and left no artifact at all: nothing published the held
// reservation, its repository, or which of the two axes was holding it, so
// `grep reservation` over the authority log returned nothing and only a
// deterministic simulator could find the wedge. The axis is taken from the plan
// that decided it rather than recomputed here, so what an operator reads is what
// the scheduler acted on.
func TestEngineTickerPublishesTheHeldReservationAndItsAxis(t *testing.T) {
	health := reservationHealthForTest(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	result := app.TickResult{At: now, Plan: scheduler.Plan{
		Status:          scheduler.PlanReady,
		ReservationAxis: scheduler.ReservationAxisRepositoryCap,
		Next: scheduler.State{Reservation: &domain.Reservation{
			Demand:    domain.DemandKey{Repo: "c/repo", RunID: 1009, Attempt: 1, JobID: 500_009},
			Profile:   "builder",
			Resources: domain.Resources{CPU: 6, MemoryMB: 12_288, Slots: 1},
			Since:     now.Add(-13 * time.Minute),
		}},
	}}

	engineTicker{health: health}.recordReservation(result)

	published := health.Snapshot().Reservation
	if published == nil {
		t.Fatal("a held reservation must be published; its absence is what made issue #226 invisible")
	}
	if published.Repo != "c/repo" || published.Demand != "c/repo/1009/1/500009" {
		t.Fatalf("the head and its repository must be named: %#v", published)
	}
	if published.Axis != string(scheduler.ReservationAxisRepositoryCap) {
		t.Fatalf("the axis is the whole diagnosis and must come from the plan: %q", published.Axis)
	}
	if published.Held != 13*time.Minute || published.CPU != 6 || published.MemoryMiB != 12_288 {
		t.Fatalf("the vector and how long it has been held: %#v", published)
	}
}

// TestEngineTickerClearsAReleasedReservation keeps absence honest: a fleet that
// holds nothing must publish nothing, or a stale row would go on reporting a
// hold that has ended.
func TestEngineTickerClearsAReleasedReservation(t *testing.T) {
	health := reservationHealthForTest(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	if err := health.SetReservation(&telemetry.ReservationMetric{Profile: "builder", Axis: "vector"}); err != nil {
		t.Fatalf("SetReservation: %v", err)
	}

	engineTicker{health: health}.recordReservation(app.TickResult{At: now, Plan: scheduler.Plan{}})

	if published := health.Snapshot().Reservation; published != nil {
		t.Fatalf("a released reservation must disappear: %#v", published)
	}
}

// TestEngineTickerPublishesAnUnjudgedReservationAsSuch covers the one plan that
// can still carry a reservation without judging it: an unusable observation.
// A plan that judged nothing must not publish a judgement.
//
// Every OTHER exit used to reach here too, which is issue #235:
// `judgeCarriedReservation` now judges a carried reservation on any tick whose
// observation was usable, so "unjudged" means exactly one thing.
func TestEngineTickerPublishesAnUnjudgedReservationAsSuch(t *testing.T) {
	health := reservationHealthForTest(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	result := app.TickResult{At: now, Plan: scheduler.Plan{
		Status: scheduler.PlanBlockedObservation,
		Next: scheduler.State{Reservation: &domain.Reservation{
			Demand:    domain.DemandKey{Repo: "a/repo", RunID: 1, Attempt: 1, JobID: 2},
			Profile:   "builder",
			Resources: domain.Resources{CPU: 6, MemoryMB: 12_288, Slots: 1},
			Since:     now.Add(-time.Minute),
		}},
	}}

	engineTicker{health: health}.recordReservation(result)

	published := health.Snapshot().Reservation
	if published == nil || published.Axis != "" {
		t.Fatalf("a plan that judged nothing publishes no axis: %#v", published)
	}
}
