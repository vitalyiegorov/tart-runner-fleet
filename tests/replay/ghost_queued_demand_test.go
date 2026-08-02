package replay_test

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/sqlite"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/telemetry"
)

const (
	ghostScaleSet  = int64(2)
	ghostOwner     = "vitalyiegorov"
	ghostRepo      = "knee-doctor"
	ghostRunID     = int64(163163163)
	ghostRequestID = int64(9012)
	ghostJobID     = int64(48484848)
	ghostJobName   = "e2e"
)

var ghostQueuedAt = time.Date(2026, 8, 1, 18, 32, 52, 0, time.UTC)

type ghostClock struct{ at time.Time }

func (c *ghostClock) Now() time.Time { return c.at }

type ghostSnapshot struct {
	at   time.Time
	jobs []githubscaleset.WorkflowJob
}

func (s ghostSnapshot) ObservedAt() time.Time                    { return s.at }
func (s ghostSnapshot) QueuedJobs() []githubscaleset.WorkflowJob { return s.jobs }

func ghostBinding() app.Binding {
	return app.Binding{StoreKey: ghostScaleSet, ScaleSetID: ghostScaleSet, Scope: "knee",
		Targets: []string{ghostOwner + "/" + ghostRepo}, ScaleSetLabels: []string{"self-hosted", "large"},
		Profile: domain.Profile{ID: "large", Route: "linux-large", Platform: domain.PlatformLinux}}
}

// publishQueue renders the real fleet.v1 status document from the queue the
// coordinator derives, exactly as the daemon's tick does, and reads it back
// through the real client so the release gate sees a published document.
func publishQueue(t *testing.T, now time.Time, summary app.QueueSummary) adminapi.StatusEnvelope {
	t.Helper()
	ctx := context.Background()
	health, err := telemetry.NewHealth(&ghostClock{at: now}, telemetry.HealthConfig{Profiles: []string{"linux-large"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := health.SetQueue("linux-large", summary.Count, summary.Oldest); err != nil {
		t.Fatal(err)
	}
	server, err := telemetry.NewServer(health, telemetry.ServerConfig{ControllerVersion: "v0.1.304", ControllerMode: "authority"})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Shutdown(context.Background())
		<-served
	})
	client, err := adminapi.NewClient("http://"+listener.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := client.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}

// observe advertises the scale set's statistics the way the broker did all
// night — one acquirable job, forever — and then reports what the fleet would
// plan and publish at that instant.
func observe(t *testing.T, store *sqlite.Store, messageID int64, now time.Time) app.QueueSummary {
	t.Helper()
	ctx := context.Background()
	if _, err := store.PutDemandStatistics(ctx, ghostScaleSet, operations.DemandStatistics{MessageID: messageID,
		Available: 1, Registered: 1, Idle: 1, ObservedAt: now}); err != nil {
		t.Fatal(err)
	}
	coordinator := app.DemandCoordinator{Store: store, Now: func() time.Time { return now }}
	executable, err := coordinator.QueuedDemands(ctx, ghostBinding())
	if err != nil {
		t.Fatal(err)
	}
	summary, err := coordinator.QueueSummary(ctx, ghostBinding(), executable)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Count != len(executable) {
		t.Fatalf("queue summary %#v disagrees with %d executable demands", summary, len(executable))
	}
	return summary
}

// reconcile commits one complete REST scope observation, which is the only
// evidence that can retire demand GitHub still advertises.
func reconcile(t *testing.T, store *sqlite.Store, at time.Time, jobs ...githubscaleset.WorkflowJob) {
	t.Helper()
	coordinator := app.DemandCoordinator{Store: store, Now: func() time.Time { return at }, GhostAbsence: 15 * time.Minute}
	if _, err := coordinator.ReconcileQueuedJobs(context.Background(), []app.Binding{ghostBinding()},
		ghostSnapshot{at: at, jobs: jobs}); err != nil {
		t.Fatal(err)
	}
}

// TestGhostQueuedDemandStopsBlockingQuiescence replays the 2026-08-01 incident.
//
// A `knee-repo`/`large` job was queued at 18:32:52Z, GitHub cancelled it
// server-side when the PR branch was force-pushed, and no terminal broker event
// ever arrived. The scale-set session kept advertising one acquirable job, so
// the fleet kept the demand, kept provisioning `large` VMs that registered
// online-but-never-busy, and stayed non-quiescent for 10h35m — which made
// `fleet update` fail every 300s with `prepare update: autoupdate: fleet is not
// quiescent`, blocking the very release that improves recovery. GitHub's REST
// scope had zero non-completed workflow runs for that entire window.
func TestGhostQueuedDemandStopsBlockingQuiescence(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// 18:32:52Z — the broker advertises the job and the fleet records it.
	event := operations.DemandEvent{Kind: operations.DemandJobAvailable, RunnerRequestID: ghostRequestID,
		Owner: ghostOwner, Repository: ghostRepo, WorkflowRunID: ghostRunID, JobID: "job-uuid",
		DisplayName: ghostJobName, EventName: "pull_request", Labels: []string{"self-hosted", "large"},
		QueueTime: ghostQueuedAt}
	if _, err := store.ApplyDemandBatch(ctx, ghostScaleSet, 1, []operations.DemandEvent{event}); err != nil {
		t.Fatal(err)
	}
	job := githubscaleset.WorkflowJob{ID: ghostJobID, RunID: ghostRunID, RunAttempt: 1,
		Repository: githubscaleset.Repository{Owner: ghostOwner, Name: ghostRepo}, Name: ghostJobName,
		Status: "queued", Labels: []string{"self-hosted", "large"}, CreatedAt: ghostQueuedAt, QueueTimeExact: true}
	reconcile(t, store, ghostQueuedAt.Add(time.Minute), job)

	// The job is real: it is queued on both sources, and the fleet must defer
	// any release while it can still start.
	live := observe(t, store, 1, ghostQueuedAt.Add(2*time.Minute))
	if live.Count != 1 || !live.Oldest.Equal(ghostQueuedAt) {
		t.Fatalf("queued job = %#v", live)
	}
	if quiescent(publishQueue(t, ghostQueuedAt.Add(2*time.Minute), live)) {
		t.Fatal("a job that can still start must defer a release")
	}

	// 18:37:52Z onward — the force-push cancels the run. Every complete REST
	// observation of the scope is now empty, but GitHub keeps advertising the
	// job to the session, so the broker alone would keep it forever.
	for i, absent := range []struct {
		at        time.Time
		wantCount int
	}{
		{at: ghostQueuedAt.Add(5 * time.Minute), wantCount: 1},
		{at: ghostQueuedAt.Add(10 * time.Minute), wantCount: 1},
		{at: ghostQueuedAt.Add(15 * time.Minute), wantCount: 1},
		{at: ghostQueuedAt.Add(19 * time.Minute), wantCount: 1},
		{at: ghostQueuedAt.Add(21 * time.Minute), wantCount: 0},
	} {
		reconcile(t, store, absent.at)
		got := observe(t, store, int64(i+2), absent.at)
		if got.Count != absent.wantCount {
			t.Fatalf("%v after cancellation: queue = %#v, want %d", absent.at.Sub(ghostQueuedAt), got, absent.wantCount)
		}
	}

	// The fleet is quiescent again without an operator restarting the daemon,
	// so the deferred release can finally activate.
	settled := ghostQueuedAt.Add(21 * time.Minute)
	if !quiescent(publishQueue(t, settled, observe(t, store, 99, settled))) {
		t.Fatal("the fleet is still not quiescent after the ghost was proven absent")
	}

	// Evidence, not a timer: the moment REST sees the job queued again the
	// demand returns untouched, with its original age intact.
	reconcile(t, store, settled.Add(time.Minute), job)
	revived := observe(t, store, 100, settled.Add(2*time.Minute))
	if revived.Count != 1 || !revived.Oldest.Equal(ghostQueuedAt) {
		t.Fatalf("re-observed job did not return: %#v", revived)
	}
}
