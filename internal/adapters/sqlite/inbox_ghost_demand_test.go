package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

const ghostScaleSet = int64(2)

func ghostDemandEvent(requestID int64, queued time.Time) operations.DemandEvent {
	event := demandEvent(operations.DemandJobAvailable, requestID)
	event.DisplayName = "build"
	event.Labels = []string{"self-hosted", "large"}
	event.QueueTime = queued
	return event
}

func ghostJobObservation(queued time.Time) operations.GitHubJobObservation {
	return operations.GitHubJobObservation{WorkflowJobID: 4242, Owner: "owner", Repository: "repo", WorkflowRunID: 77,
		RunAttempt: 1, DisplayName: "build", Labels: []string{"self-hosted", "large"}, Status: "queued",
		CreatedAt: queued, QueueTimeExact: true}
}

func ghostEvidence(t *testing.T, store *Store, scaleSetID int64) (absentSince int64, observations int, expiredAt int64) {
	t.Helper()
	if err := store.db.QueryRow(`SELECT absent_since,absent_observations,expired_at FROM runner_demands
		WHERE scale_set_id=? AND runner_request_id=?`, scaleSetID, int64(901)).
		Scan(&absentSince, &observations, &expiredAt); err != nil {
		t.Fatalf("read ghost evidence: %v", err)
	}
	return absentSince, observations, expiredAt
}

// seedCorroboratedGhost reproduces the issue #113 incident up to the moment
// GitHub's REST scope went empty: the broker advertised one queued job, a
// complete REST snapshot corroborated it, and then `count` further complete
// snapshots, spaced five minutes apart, no longer contained it.
func seedCorroboratedGhost(t *testing.T, store *Store, queued time.Time, absent int) time.Time {
	t.Helper()
	ctx := context.Background()
	if _, err := store.ApplyDemandBatch(ctx, ghostScaleSet, 1, []operations.DemandEvent{ghostDemandEvent(901, queued)}); err != nil {
		t.Fatalf("seed demand: %v", err)
	}
	observed := queued.Add(time.Minute)
	if _, err := store.ReconcileGitHubJobs(ctx, ghostScaleSet, observed,
		[]operations.GitHubJobObservation{ghostJobObservation(queued)}); err != nil {
		t.Fatalf("corroborate demand: %v", err)
	}
	for range absent {
		observed = observed.Add(5 * time.Minute)
		if _, err := store.ReconcileGitHubJobs(ctx, ghostScaleSet, observed, nil); err != nil {
			t.Fatalf("empty snapshot: %v", err)
		}
	}
	return observed
}

// The incident: a cancelled job left one JobAvailable demand row alive for
// 11 hours while every complete REST snapshot of the scope was empty.
func TestAbsentQueuedDemandExpiresOnlyOnCompleteRESTEvidence(t *testing.T) {
	ctx := context.Background()
	queued := time.Date(2026, 8, 1, 18, 32, 52, 0, time.UTC)
	for _, test := range []struct {
		name       string
		absent     int
		window     time.Duration
		mutate     func(t *testing.T, store *Store, observed time.Time)
		wantExpiry int64
		wantActive int
	}{
		{name: "absent from enough snapshots for long enough", absent: 4, window: 15 * time.Minute, wantExpiry: 1},
		{name: "absence shorter than the window", absent: 4, window: time.Hour, wantActive: 1},
		{name: "too few snapshots missed it", absent: 2, window: time.Minute, wantActive: 1},
		{
			name: "never corroborated by REST", absent: 4, window: 15 * time.Minute, wantActive: 1,
			mutate: func(t *testing.T, store *Store, _ time.Time) {
				t.Helper()
				if _, err := store.db.Exec(`UPDATE runner_demands SET corroborated_at=0,absent_since=0,absent_observations=0`); err != nil {
					t.Fatalf("clear corroboration: %v", err)
				}
			},
		},
		{
			name: "broker assigned the job before expiry", absent: 4, window: 15 * time.Minute, wantActive: 1,
			mutate: func(t *testing.T, store *Store, _ time.Time) {
				t.Helper()
				if _, err := store.ApplyDemandBatch(ctx, ghostScaleSet, 2,
					[]operations.DemandEvent{demandEvent(operations.DemandJobAssigned, 901)}); err != nil {
					t.Fatalf("assign job: %v", err)
				}
			},
		},
		{
			name: "REST saw the job queued again", absent: 4, window: 15 * time.Minute, wantActive: 1,
			mutate: func(t *testing.T, store *Store, observed time.Time) {
				t.Helper()
				if _, err := store.ReconcileGitHubJobs(ctx, ghostScaleSet, observed.Add(time.Minute),
					[]operations.GitHubJobObservation{ghostJobObservation(queued)}); err != nil {
					t.Fatalf("recorroborate: %v", err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := testStore(t)
			observed := seedCorroboratedGhost(t, store, queued, test.absent)
			if test.mutate != nil {
				test.mutate(t, store, observed)
				observed = observed.Add(2 * time.Minute)
			}
			criteria := operations.GhostDemandCriteria{ObservedAt: observed,
				AbsentBefore: observed.Add(-test.window), MinObservations: 3}
			expired, err := store.ExpireGhostDemands(ctx, ghostScaleSet, criteria)
			if err != nil || expired != test.wantExpiry {
				t.Fatalf("expired = %d, %v; want %d", expired, err, test.wantExpiry)
			}
			active, err := store.ActiveDemands(ctx, ghostScaleSet)
			if err != nil || len(active) != test.wantActive {
				t.Fatalf("active demand = %#v, %v; want %d", active, err, test.wantActive)
			}
		})
	}
}

// A ghost is retired at most once, and only ever revoked by newer evidence.
func TestGhostExpiryIsIdempotentAndRevocable(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	queued := time.Date(2026, 8, 1, 18, 32, 52, 0, time.UTC)
	observed := seedCorroboratedGhost(t, store, queued, 4)
	criteria := operations.GhostDemandCriteria{ObservedAt: observed, AbsentBefore: observed.Add(-15 * time.Minute), MinObservations: 3}
	if expired, err := store.ExpireGhostDemands(ctx, ghostScaleSet, criteria); err != nil || expired != 1 {
		t.Fatalf("first expiry = %d, %v", expired, err)
	}
	if expired, err := store.ExpireGhostDemands(ctx, ghostScaleSet, criteria); err != nil || expired != 0 {
		t.Fatalf("second expiry = %d, %v", expired, err)
	}
	// An expired row accrues no further absence: it is already a conclusion.
	if _, err := store.ReconcileGitHubJobs(ctx, ghostScaleSet, observed.Add(5*time.Minute), nil); err != nil {
		t.Fatal(err)
	}
	if _, observations, expiredAt := ghostEvidence(t, store, ghostScaleSet); observations != 4 || expiredAt == 0 {
		t.Fatalf("expired ghost kept accruing: observations=%d expired_at=%d", observations, expiredAt)
	}
	// The broker re-advertising the same request revives it unchanged.
	if _, err := store.ApplyDemandBatch(ctx, ghostScaleSet, 9,
		[]operations.DemandEvent{ghostDemandEvent(901, queued)}); err != nil {
		t.Fatal(err)
	}
	active, err := store.ActiveDemands(ctx, ghostScaleSet)
	if err != nil || len(active) != 1 || active[0].RunnerRequestID != 901 {
		t.Fatalf("re-advertised demand stayed expired: %#v, %v", active, err)
	}
	absentSince, observations, expiredAt := ghostEvidence(t, store, ghostScaleSet)
	if absentSince != 0 || observations != 0 || expiredAt != 0 {
		t.Fatalf("broker evidence did not reset absence: %d %d %d", absentSince, observations, expiredAt)
	}
}

// A replayed or clock-skewed snapshot is not new evidence, so it can neither
// start nor extend an absence.
func TestReplayedSnapshotAccruesNoAbsence(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	queued := time.Date(2026, 8, 1, 18, 32, 52, 0, time.UTC)
	observed := seedCorroboratedGhost(t, store, queued, 1)
	absentSince, observations, _ := ghostEvidence(t, store, ghostScaleSet)
	if absentSince == 0 || observations != 1 {
		t.Fatalf("first empty snapshot did not record absence: %d %d", absentSince, observations)
	}
	for _, replay := range []time.Time{observed, observed.Add(-time.Minute)} {
		if _, err := store.ReconcileGitHubJobs(ctx, ghostScaleSet, replay, nil); err != nil {
			t.Fatal(err)
		}
	}
	if since, count, _ := ghostEvidence(t, store, ghostScaleSet); since != absentSince || count != 1 {
		t.Fatalf("replayed snapshot counted as evidence: %d %d", since, count)
	}
}

func TestGhostExpiryRejectsUnprovableCriteria(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	observed := time.Date(2026, 8, 2, 5, 0, 0, 0, time.UTC)
	valid := operations.GhostDemandCriteria{ObservedAt: observed, AbsentBefore: observed.Add(-time.Hour), MinObservations: 3}
	for _, test := range []struct {
		name       string
		scaleSetID int64
		criteria   operations.GhostDemandCriteria
	}{
		{name: "unknown scale set", scaleSetID: 0, criteria: valid},
		{name: "no observation", scaleSetID: 1, criteria: operations.GhostDemandCriteria{AbsentBefore: observed, MinObservations: 3}},
		{name: "no absence bound", scaleSetID: 1, criteria: operations.GhostDemandCriteria{ObservedAt: observed, MinObservations: 3}},
		{
			name: "absence bound after the observation", scaleSetID: 1,
			criteria: operations.GhostDemandCriteria{ObservedAt: observed, AbsentBefore: observed.Add(time.Second), MinObservations: 3},
		},
		{
			name: "no observation floor", scaleSetID: 1,
			criteria: operations.GhostDemandCriteria{ObservedAt: observed, AbsentBefore: observed.Add(-time.Hour)},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := store.ExpireGhostDemands(ctx, test.scaleSetID, test.criteria); !errors.Is(err, operations.ErrInvalid) {
				t.Fatalf("criteria accepted: %v", err)
			}
		})
	}
	store.injectFault = func(point string) error {
		if point == "inbox.ghost.expire" {
			return errors.New("injected")
		}
		return nil
	}
	if _, err := store.ExpireGhostDemands(ctx, 1, valid); err == nil {
		t.Fatal("expiry write failure ignored")
	}
}
