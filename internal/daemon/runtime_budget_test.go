package daemon

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/sqlite"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/telemetry"
)

type scriptedQueueObserver struct {
	observations []githubscaleset.Observation
	calls        int
}

func (s *scriptedQueueObserver) Refresh(_ context.Context, previous *githubscaleset.Snapshot) githubscaleset.Observation {
	observation := s.observations[min(s.calls, len(s.observations)-1)]
	s.calls++
	if observation.Err != nil && previous != nil {
		observation.Freshness = githubscaleset.Stale
		observation.Snapshot = previous
	}
	return observation
}

// Issue #377: the Mac nodes spent the shared installation budget by ~:45 of
// every hour and then logged `component=ingest` with an unclassified reason
// while health said only queue_observation_stale. The ingester must name the
// rate limit in the bounded vocabulary, keep the prior inventory stale rather
// than empty, and defer the next refresh to the window reset instead of
// re-spending every poll interval. A scarce but unexhausted budget defers the
// same way, so the limit is never reached; a comfortable budget keeps the
// ordinary interval; a far reset is bounded; an elapsed one defers nothing.
func TestRESTQueueIngesterPacesToTheInstallationBudget(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 10, 16, 13, 5, 0, 0, time.UTC)
	binding := app.Binding{ScaleSetID: 1, Targets: []string{"o/r"},
		Profile: domain.Profile{ID: "builder", Route: "macos-builder", Platform: domain.PlatformMacOS}}
	health, _ := telemetry.NewHealth(wallClock{}, telemetry.HealthConfig{Profiles: []string{"builder"}, CriticalObservations: []string{"github-rest-personal"}})
	empty, err := githubscaleset.NewObserver(githubscaleset.ObserverConfig{BaseURL: "https://api.test",
		Repositories: []githubscaleset.Repository{{Owner: "o", Name: "r"}},
		HTTP:         runtimeDoerFunc(func(*http.Request) (*http.Response, error) { return runtimeResponse(`{"workflow_runs":[]}`), nil }),
		Tokens:       githubscaleset.TokenSourceFunc(func(context.Context) (string, error) { return "token", nil })})
	if err != nil {
		t.Fatal(err)
	}
	fresh := empty.Refresh(ctx, nil)
	if fresh.Err != nil || fresh.Snapshot == nil {
		t.Fatalf("empty inventory = %+v", fresh)
	}
	limited := &githubscaleset.APIError{Kind: githubscaleset.RateLimited, Status: 403, RetryAfter: 10 * time.Minute, Cause: errors.New("Forbidden")}
	budget := func(remaining int, reset time.Time) githubscaleset.Budget {
		return githubscaleset.Budget{Limit: 5000, Remaining: remaining, Reset: reset, Observed: true}
	}
	type step struct {
		name     string
		observe  githubscaleset.Observation
		wantNext time.Duration
		wantErr  string
		detail   string
		fresh    telemetry.ObservationFreshness
	}
	steps := []step{
		{"comfortable budget keeps the interval", withBudget(fresh, budget(4000, now.Add(20*time.Minute))), 30 * time.Second, "", "", telemetry.ObservationFresh},
		{"exhausted limit names itself and waits for the reset", withBudgetErr(limited, budget(0, now.Add(10*time.Minute))), 10 * time.Minute, githubscaleset.ReasonRateLimited, githubscaleset.ReasonRateLimited, telemetry.ObservationStale},
		{"scarce budget defers before the limit is reached", withBudget(fresh, budget(400, now.Add(20*time.Minute))), 20 * time.Minute, "", "", telemetry.ObservationFresh},
		{"a far reset is bounded", withBudget(fresh, budget(0, now.Add(3*time.Hour))), time.Hour, "", "", telemetry.ObservationFresh},
		{"an elapsed reset defers nothing", withBudget(fresh, budget(0, now.Add(-time.Minute))), 30 * time.Second, "", "", telemetry.ObservationFresh},
		{"a limit without a horizon keeps the interval", githubscaleset.Observation{Err: &githubscaleset.APIError{Kind: githubscaleset.RateLimited, Status: 429, Cause: errors.New("limit")}}, 30 * time.Second, githubscaleset.ReasonRateLimited, githubscaleset.ReasonRateLimited, telemetry.ObservationStale},
		{"a server error is its own reason", githubscaleset.Observation{Err: &githubscaleset.APIError{Kind: githubscaleset.Server, Status: 502, Cause: errors.New("bad gateway")}}, 30 * time.Second, githubscaleset.ReasonServerError, githubscaleset.ReasonServerError, telemetry.ObservationStale},
		{"a transport failure stays the stale observation", githubscaleset.Observation{Err: errors.New("down")}, 30 * time.Second, githubscaleset.ReasonQueueObservationStale, githubscaleset.ReasonQueueObservationStale, telemetry.ObservationStale},
	}
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			observer := &scriptedQueueObserver{observations: []githubscaleset.Observation{s.observe}}
			ingester := &restQueueIngester{coordinator: app.DemandCoordinator{Store: store}, bindings: []app.Binding{binding},
				observer: observer, interval: 30 * time.Second, health: health, observation: "github-rest-personal",
				now: func() time.Time { return now }, previous: fresh.Snapshot}
			_, err := ingester.IngestChanged(ctx)
			if got := reasonOf(err); got != s.wantErr {
				t.Fatalf("failure reason = %q (err=%v), want %q", got, err, s.wantErr)
			}
			if observation := health.Snapshot().Observations["github-rest-personal"]; observation.Freshness != s.fresh || observation.Detail != s.detail {
				t.Fatalf("observation = %+v, want %s/%q", observation, s.fresh, s.detail)
			}
			if !ingester.next.Equal(now.Add(s.wantNext)) {
				t.Fatalf("next = %v, want now+%v", ingester.next.Sub(now), s.wantNext)
			}
			if s.wantErr != "" && ingester.previous != fresh.Snapshot {
				t.Fatal("a failed refresh must keep the prior inventory")
			}
		})
	}
	unavailable := &restQueueIngester{coordinator: app.DemandCoordinator{Store: store}, bindings: []app.Binding{binding},
		observer: &scriptedQueueObserver{observations: []githubscaleset.Observation{{Err: limited, Budget: budget(0, now.Add(10*time.Minute))}}},
		interval: 30 * time.Second, health: health, observation: "github-rest-personal", now: func() time.Time { return now }}
	if _, err := unavailable.IngestChanged(ctx); reasonOf(err) != githubscaleset.ReasonRateLimited ||
		health.Snapshot().Observations["github-rest-personal"].Freshness != telemetry.ObservationUnavailable ||
		health.Snapshot().Observations["github-rest-personal"].Detail != githubscaleset.ReasonRateLimited {
		t.Fatalf("first-ever limited refresh = %v observation=%+v", err, health.Snapshot().Observations["github-rest-personal"])
	}
	again := &restQueueIngester{coordinator: app.DemandCoordinator{Store: store}, bindings: []app.Binding{binding},
		observer: unavailable.observer, interval: 30 * time.Second, now: func() time.Time { return now }}
	described := githubscaleset.DescribeIngestFailure(func() error { _, err := again.IngestChanged(ctx); return err }())
	if described.Reason != githubscaleset.ReasonRateLimited || described.Status != 403 || described.RetryAfter != 10*time.Minute {
		t.Fatalf("described failure = %+v", described)
	}
}

func withBudget(o githubscaleset.Observation, b githubscaleset.Budget) githubscaleset.Observation {
	o.Budget = b
	return o
}

func withBudgetErr(err error, b githubscaleset.Budget) githubscaleset.Observation {
	return githubscaleset.Observation{Err: err, Budget: b}
}

func reasonOf(err error) string {
	var reason interface{ FailureReason() string }
	if errors.As(err, &reason) {
		return reason.FailureReason()
	}
	return ""
}

// Issue #449: a budget deferral waits up to an hour for the window reset, and
// the observation used to go unwritten for that whole wait. After the critical
// observation TTL it read as expired, the mark of a stopped loop, so a node
// that had only decided to wait was unhealthy and refused every release until
// the reset. The deferring ingester wakes at its ordinary pace, spends nothing,
// and re-records its inventory as stale on the rate limit.
func TestRESTQueueIngesterKeepsWritingItsObservationWhileDeferred(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 28, 14, 10, 0, 0, time.UTC)
	health, _ := telemetry.NewHealth(wallClock{}, telemetry.HealthConfig{Profiles: []string{"builder"},
		CriticalObservations: []string{"github-rest-budgie-org"}})
	_ = health.RecordObservation("github-rest-budgie-org", telemetry.ObservationFresh)
	for name, previous := range map[string]*githubscaleset.Snapshot{"stale": {}, "unavailable": nil} {
		observer := &scriptedQueueObserver{observations: []githubscaleset.Observation{{Freshness: githubscaleset.Fresh}}}
		ingester := &restQueueIngester{observer: observer, interval: time.Millisecond, health: health,
			observation: "github-rest-budgie-org", now: func() time.Time { return now },
			previous: previous, next: now.Add(20 * time.Minute)}
		changed, err := ingester.IngestChanged(ctx)
		if changed || err != nil || observer.calls != 0 {
			t.Fatalf("%s: deferred refresh = changed %v err %v refreshes %d, want a quiet wait", name, changed, err, observer.calls)
		}
		want := telemetry.ObservationStale
		if previous == nil {
			want = telemetry.ObservationUnavailable
		}
		if got := health.Snapshot().Observations["github-rest-budgie-org"]; got.Freshness != want || got.Detail != githubscaleset.ReasonRateLimited {
			t.Fatalf("%s: observation while deferred = %+v, want %s/%s", name, got, want, githubscaleset.ReasonRateLimited)
		}
		if !ingester.next.Equal(now.Add(20 * time.Minute)) {
			t.Fatalf("%s: the wait moved the reset horizon to %v", name, ingester.next)
		}
	}
}
