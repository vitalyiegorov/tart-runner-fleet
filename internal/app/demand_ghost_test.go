package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

type ghostExpiryCall struct {
	scaleSetID int64
	criteria   operations.GhostDemandCriteria
}

type ghostDemandStore struct {
	*fakeDemandStore
	calls     []ghostExpiryCall
	expired   map[int64]int64
	expireErr error
}

func (g *ghostDemandStore) ExpireGhostDemands(_ context.Context, scaleSetID int64,
	criteria operations.GhostDemandCriteria,
) (int64, error) {
	g.calls = append(g.calls, ghostExpiryCall{scaleSetID: scaleSetID, criteria: criteria})
	if g.expireErr != nil {
		return 0, g.expireErr
	}
	return g.expired[scaleSetID], nil
}

// Issue #113: an empty REST scope is the evidence that retires demand GitHub
// still advertises, and it must reach every scale set the scope covers.
func TestEmptyRESTScopeExpiresGhostDemandPerScaleSet(t *testing.T) {
	observed := time.Date(2026, 8, 2, 5, 8, 0, 0, time.UTC)
	bindings := []Binding{
		{StoreKey: 201, ScaleSetID: 1, Scope: "knee", Targets: []string{"owner/knee-repo"},
			ScaleSetLabels: []string{"self-hosted", "large"},
			Profile:        domain.Profile{ID: "large", Route: "linux-large", Platform: domain.PlatformLinux}},
		{StoreKey: 202, ScaleSetID: 2, Scope: "knee", Targets: []string{"owner/knee-repo"},
			ScaleSetLabels: []string{"self-hosted", "small"},
			Profile:        domain.Profile{ID: "small", Route: "linux-small", Platform: domain.PlatformLinux}},
	}
	for _, test := range []struct {
		name        string
		absence     time.Duration
		expired     map[int64]int64
		wantWindow  time.Duration
		wantChanged bool
	}{
		{name: "default window retires the ghost", expired: map[int64]int64{201: 1}, wantWindow: 15 * time.Minute, wantChanged: true},
		{name: "configured window", absence: 40 * time.Minute, expired: map[int64]int64{202: 2}, wantWindow: 40 * time.Minute, wantChanged: true},
		{name: "nothing provably absent", wantWindow: 15 * time.Minute},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &ghostDemandStore{fakeDemandStore: &fakeDemandStore{}, expired: test.expired}
			coordinator := DemandCoordinator{Store: store, GhostAbsence: test.absence}
			changed, err := coordinator.ReconcileQueuedJobs(context.Background(), bindings, fakeQueueSnapshot{at: observed})
			if err != nil || changed != test.wantChanged {
				t.Fatalf("reconcile = %v, %v; want changed=%v", changed, err, test.wantChanged)
			}
			if len(store.calls) != len(bindings) {
				t.Fatalf("expiry calls = %#v", store.calls)
			}
			for i, call := range store.calls {
				want := operations.GhostDemandCriteria{ObservedAt: observed, AbsentBefore: observed.Add(-test.wantWindow),
					MinObservations: minGhostObservations}
				if call.scaleSetID != bindings[i].durableKey() || call.criteria != want || !call.criteria.Valid() {
					t.Fatalf("expiry criteria = %#v; want %d %#v", call, bindings[i].durableKey(), want)
				}
			}
		})
	}
}

func TestGhostExpiryFailureFailsTheReconciliation(t *testing.T) {
	observed := time.Date(2026, 8, 2, 5, 8, 0, 0, time.UTC)
	binding := Binding{StoreKey: 201, ScaleSetID: 1, Targets: []string{"owner/repo"},
		Profile: domain.Profile{ID: "large", Route: "linux-large", Platform: domain.PlatformLinux}}
	store := &ghostDemandStore{fakeDemandStore: &fakeDemandStore{}, expireErr: errors.New("expire failed")}
	changed, err := (DemandCoordinator{Store: store}).ReconcileQueuedJobs(context.Background(),
		[]Binding{binding}, fakeQueueSnapshot{at: observed})
	if err == nil || changed {
		t.Fatalf("expiry failure = %v, %v", changed, err)
	}
	plain := &fakeDemandStore{}
	if changed, err := (DemandCoordinator{Store: plain}).ReconcileQueuedJobs(context.Background(),
		[]Binding{binding}, fakeQueueSnapshot{at: observed}); err != nil || changed {
		t.Fatalf("store without ghost expiry = %v, %v", changed, err)
	}
}
