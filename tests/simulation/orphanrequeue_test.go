package simulation_test

import (
	"context"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

type orphanBrokerRecovery struct {
	world *world
	calls int
}

func (b *orphanBrokerRecovery) RequeueOrphan(_ context.Context, _ githubscaleset.WorkflowJob, excluded func(string, []string) bool, act bool) (string, error) {
	if excluded("build", []string{"large"}) {
		return "held", nil
	}
	if !act {
		return "would_requeue", nil
	}
	b.calls++
	if b.world.arrive("a/repo", "large", "push") == nil {
		return "failed", nil
	}
	return "rerun", nil
}

// The broker's old arrival is not automatically retried at rejoin. Feed that
// missing REST job through production recovery and model rerun as a new offer.
func TestIssue520AutomaticRequeueReoffersAllGatedArrival(t *testing.T) {
	cfg := gatedWorld()
	cfg.SiblingsFull = true
	trace := simTrace{Seed: 5201, Ticks: 40, Config: cfg.Name, Events: []simEvent{{Tick: 1, Kind: eventHostTenant, Count: gatedTenant}, {Tick: 4, Kind: eventArrive, Repo: "a/repo", Profile: "large", Event: "push"}, {Tick: 20, Kind: eventHostTenant}}}
	w := newWorld(t, cfg, trace)
	defer w.close()
	if findings := w.run(); len(findings) > 0 {
		t.Fatal(findings)
	}
	if w.unassigned != 1 {
		t.Fatal(w.unassigned)
	}
	var binding app.Binding
	for _, b := range cfg.Bindings {
		if b.Profile.ID == "large" {
			binding = b
		}
	}
	job := githubscaleset.WorkflowJob{ID: 520, RunID: 520, RunAttempt: 1, Repository: githubscaleset.Repository{Owner: "a", Name: "repo"}, Name: "large", Status: "queued", Labels: binding.ScaleSetLabels, CreatedAt: simEpoch.Add(4 * simTick), QueueTimeExact: true}
	snapshot := simSnapshotView{snapshot: &simSnapshot{at: w.now, jobs: []githubscaleset.WorkflowJob{job}}}
	coordinator := app.DemandCoordinator{Store: w.store, Now: func() time.Time { return w.now }, AuditedAssignments: []app.ScaleSetAssignment{{Scope: binding.Scope, ID: binding.ScaleSetID, Labels: binding.ScaleSetLabels, Fresh: true}}}
	broker := &orphanBrokerRecovery{world: w}
	recovery := app.OrphanRecovery{Config: config.OrphanRequeue{Mode: "act"}, Store: w.store, API: broker}
	if _, err := recovery.Reconcile(w.ctx, coordinator, []app.Binding{binding}, snapshot, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := recovery.Reconcile(w.ctx, coordinator, []app.Binding{binding}, snapshot, time.Minute); err != nil {
		t.Fatal(err)
	}
	if broker.calls != 1 || len(w.jobs) != 1 {
		t.Fatalf("calls=%d jobs=%d", broker.calls, len(w.jobs))
	}
	for range 20 {
		w.tick++
		w.now = simEpoch.Add(time.Duration(w.tick) * simTick)
		w.advancePhysics()
		w.produceMessages()
		w.deliverMessages()
		w.deliverSnapshots()
		observation := w.reconcile()
		w.executeOperations()
		if w.check(observation) {
			t.Fatal(w.findings)
		}
	}
	if w.jobs[0].startedAt.IsZero() {
		t.Fatal("rerun offer never started")
	}
}

// A cancelled run disappears from REST queued inventory. Recovery must still
// generate a fresh offer from its durable reservation after a process restart.
func TestOrphanCancelledReservationReoffersWithoutInventory(t *testing.T) {
	cfg := gatedWorld()
	w := newWorld(t, cfg, simTrace{Seed: 5202, Ticks: 1, Config: cfg.Name})
	defer w.close()
	var binding app.Binding
	for _, b := range cfg.Bindings {
		if b.Profile.ID == "large" {
			binding = b
		}
	}
	r := operations.OrphanRequeue{Scope: binding.Scope, Repository: "a/repo", RunID: 521, RunAttempt: 1, At: w.now.Add(-time.Hour), Outcome: "cancelled_not_rerun"}
	ok, err := w.store.ClaimOrphanRequeue(w.ctx, r, 30*time.Minute, 3)
	if err != nil || !ok {
		t.Fatalf("claim=%v %v", ok, err)
	}
	if err := w.store.FinishOrphanRequeue(w.ctx, r); err != nil {
		t.Fatal(err)
	}
	broker := &orphanBrokerRecovery{world: w}
	recovery := app.OrphanRecovery{Config: config.OrphanRequeue{Mode: "act"}, Store: w.store, API: broker}
	if _, err := recovery.Reconcile(w.ctx, app.DemandCoordinator{Now: func() time.Time { return w.now }}, []app.Binding{binding}, nil, time.Minute); err != nil {
		t.Fatal(err)
	}
	if broker.calls != 1 || len(w.jobs) != 1 {
		t.Fatalf("calls=%d jobs=%d", broker.calls, len(w.jobs))
	}
}
