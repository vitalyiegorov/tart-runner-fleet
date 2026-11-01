package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/sqlite"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/telemetry"
)

// TestIssue458ReconcileFailureCarriesTheObservationReason replays the Studio
// flap where a REST snapshot that could not be reconciled marked the
// observation queue_reconcile_failed while the ingest loop failure reached
// stderr with no reason at all. The loop failure must name the same closed
// reason the observation records, and keep the concrete cause reachable to
// errors.Is/As without rendering it.
func TestIssue458ReconcileFailureCarriesTheObservationReason(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 1, 17, 16, 0, 0, time.UTC)
	health, err := telemetry.NewHealth(wallClock{}, telemetry.HealthConfig{Profiles: []string{"maestro"}, CriticalObservations: []string{"rest"}})
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	// A store that can no longer commit makes ReconcileQueuedJobs fail after the
	// observation itself succeeded, which is exactly the reconcile phase.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	observer := oneRunObserver(t, now, githubscaleset.Repository{Owner: "rnw-community", Name: "rnw-community"}, "2026-11-01T16:40:00Z",
		`[{"id":201,"name":"maestro","status":"queued","created_at":"2026-11-01T16:40:00Z","labels":["self-hosted","macOS","ARM64","macos-maestro"]}]`)
	binding := app.Binding{Scope: "rnw-repo", ScaleSetID: 9, Targets: []string{"rnw-community/rnw-community"},
		ScaleSetLabels: []string{"self-hosted", "macOS", "ARM64", "macos-maestro"},
		Profile:        domain.Profile{ID: "maestro", Route: "macos-maestro", Platform: domain.PlatformMacOS}}
	ingester := restQueueIngester{coordinator: app.DemandCoordinator{Store: store}, bindings: []app.Binding{binding},
		observer: observer, health: health, observation: "rest", interval: time.Minute, now: func() time.Time { return now }}

	_, err = ingester.IngestChanged(ctx)
	if err == nil {
		t.Fatal("reconcile against a closed store must fail")
	}
	observation := health.Snapshot().Observations["rest"]
	if observation.Freshness != telemetry.ObservationUnavailable || observation.Detail != githubscaleset.ReasonQueueReconcileFailed {
		t.Fatalf("observation = %+v, want unavailable/%s", observation, githubscaleset.ReasonQueueReconcileFailed)
	}
	if got := reasonOf(err); got != observation.Detail {
		t.Fatalf("loop failure reason = %q, want the observation's %q", got, observation.Detail)
	}
	if described := githubscaleset.DescribeIngestFailure(err); described.Phase != githubscaleset.PhaseObserve {
		t.Fatalf("described failure = %+v, want the observe phase", described)
	}
}

// TestIssue458ReconcileFailureKeepsAnExistingReason leaves an already
// classified reconcile error alone, so a demand commit conflict is not
// relabelled as a generic reconcile failure, and keeps success nil.
func TestIssue458ReconcileFailureKeepsAnExistingReason(t *testing.T) {
	if classifyReconcileFailure(nil) != nil {
		t.Fatal("a successful reconcile must stay nil")
	}
	conflict := &githubscaleset.SessionFailure{Reason: githubscaleset.ReasonDemandCommitConflict}
	if got := classifyReconcileFailure(conflict); !errors.Is(got, conflict) || reasonOf(got) != githubscaleset.ReasonDemandCommitConflict {
		t.Fatalf("classified conflict = %v (%q)", got, reasonOf(got))
	}
	cause := errors.New("database is closed")
	wrapped := classifyReconcileFailure(cause)
	if reasonOf(wrapped) != githubscaleset.ReasonQueueReconcileFailed || !errors.Is(wrapped, cause) {
		t.Fatalf("wrapped = %v, want queue_reconcile_failed over the cause", wrapped)
	}
	if strings.Contains(wrapped.Error(), cause.Error()) {
		t.Fatalf("rendered failure leaked the concrete cause: %q", wrapped.Error())
	}
}
