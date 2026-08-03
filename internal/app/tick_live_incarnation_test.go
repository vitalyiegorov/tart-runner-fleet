package app

import (
	"context"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/reconcile"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

// liveIncarnationKey is the demand every case in this file plans, or refuses to
// plan, depending on what a prior incarnation of it is doing.
var liveIncarnationKey = domain.DemandKey{Repo: "owner/repo", RunID: 8, Attempt: 1, JobID: 11}

func liveIncarnationConfig() scheduler.Config {
	return scheduler.Config{LinuxCapacity: domain.Resources{CPU: 4, MemoryMB: 8192, Slots: 2},
		FairnessAge: 5 * time.Minute, RepoCaps: map[string]int{"owner/repo": 2},
		Profiles: map[domain.ProfileID]domain.Profile{
			"small": {ID: "small", Route: "tiered", Platform: domain.PlatformLinux,
				Resources: domain.Resources{CPU: 2, MemoryMB: 4096, Slots: 1}},
		}}
}

// liveIncarnationEngine is one binding, one JobAvailable demand, fresh
// statistics, and whatever inventory the case describes.
func liveIncarnationEngine(now time.Time, instances []domain.Instance) Engine {
	config := liveIncarnationConfig()
	store := &tickStore{fakeDemandStore: fakeDemandStore{
		statistics: operations.DemandStatistics{MessageID: 1, Available: 1, ObservedAt: now},
		records: []operations.DemandRecord{{Status: operations.DemandJobAvailable, RunnerRequestID: 11,
			Owner: "owner", Repository: "repo", WorkflowRunID: 8, QueueTime: now.Add(-time.Minute)}}}}
	host := domain.Host{Available: config.LinuxCapacity,
		Pressure: domain.HostPressure{FreeDiskGB: 200, AdmissionAllowed: true}}
	return Engine{Store: store, Demand: DemandCoordinator{Store: store},
		Inventory: fakeInventory{instances: domain.Fresh(instances, now), host: domain.Fresh(host, now)},
		Config:    config, Bindings: []Binding{{ScaleSetID: 1, Profile: config.Profiles["small"]}},
		ControllerID: "controller", Mode: reconcile.Authority, Now: func() time.Time { return now }}
}

func incarnation(state domain.InstanceState) domain.Instance {
	return domain.Instance{ID: "trf-small-live11", Repo: "owner/repo", Demand: liveIncarnationKey,
		Platform: domain.PlatformLinux, Profile: "small", Route: "tiered",
		Resources: domain.Resources{CPU: 2, MemoryMB: 4096, Slots: 1},
		State:     state, Power: domain.InstancePowerRunning}
}

// TestEngineTickNeverReplansALiveIncarnation is the whole of FINDING 1 (ADR
// 0031) and needs no simulator: GitHub keeps a job Available until a runner
// acquires it, so between the spawn and that acquisition the demand is still
// queued while its VM clones, boots, and registers. Re-planning it there emits
// the byte-identical content-addressed spawn, ApplyPlan refuses it on the
// instances primary key, and the refusal discards the whole tick.
//
// The lifecycle is the table, because the rule is a lifecycle rule: every
// non-terminal incarnation owns its demand, and only a terminal one — deleted or
// failed — has released it, which is exactly how an instance failure is retried.
func TestEngineTickNeverReplansALiveIncarnation(t *testing.T) {
	now := time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)
	for _, testCase := range []struct {
		name      string
		instances []domain.Instance
		wantSpawn bool
	}{
		{name: "no incarnation", wantSpawn: true},
		{name: "planned", instances: []domain.Instance{incarnation(domain.InstancePlanned)}},
		{name: "cloning", instances: []domain.Instance{incarnation(domain.InstanceCloning)}},
		{name: "booting", instances: []domain.Instance{incarnation(domain.InstanceBooting)}},
		{name: "reachable", instances: []domain.Instance{incarnation(domain.InstanceReachable)}},
		{name: "registering", instances: []domain.Instance{incarnation(domain.InstanceRegistering)}},
		{name: "online idle", instances: []domain.Instance{incarnation(domain.InstanceOnlineIdle)}},
		{name: "assigned", instances: []domain.Instance{incarnation(domain.InstanceAssigned)}},
		{name: "running", instances: []domain.Instance{incarnation(domain.InstanceRunning)}},
		{name: "draining", instances: []domain.Instance{incarnation(domain.InstanceDraining)}},
		{name: "deregistering", instances: []domain.Instance{incarnation(domain.InstanceDeregistering)}},
		{name: "stopping", instances: []domain.Instance{incarnation(domain.InstanceStopping)}},
		// Terminal: the demand is released and must be planned again, or an
		// instance failure would never be retried and a reaped VM's job would
		// never be re-run.
		{name: "failed retries", instances: []domain.Instance{incarnation(domain.InstanceFailed)}, wantSpawn: true},
		{name: "deleted tombstone retries", instances: []domain.Instance{incarnation(domain.InstanceDeleted)}, wantSpawn: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			result, err := liveIncarnationEngine(now, testCase.instances).Tick(context.Background())
			if err != nil {
				t.Fatalf("tick failed: %v", err)
			}
			spawns := 0
			for _, operation := range result.Plan.Operations {
				if operation.Kind == scheduler.OperationSpawn && operation.Demand == liveIncarnationKey {
					spawns++
				}
			}
			want := 0
			if testCase.wantSpawn {
				want = 1
			}
			if spawns != want {
				t.Fatalf("spawns for a demand with a %s incarnation = %d, want %d: %#v",
					testCase.name, spawns, want, result.Plan.Operations)
			}
			// The queue stays fully visible either way: filtering the plannable
			// queue must never hide durable demand from the SLO monitor.
			if result.Queues["small"].Count != 1 {
				t.Fatalf("queue visibility lost: %#v", result.Queues)
			}
		})
	}
}

// TestEngineTickAdmitsOtherWorkDuringABootWindow is the consequence that made
// FINDING 1 a P0 rather than a cosmetic duplicate: a refused plan is discarded
// whole, so while one demand's VM booted the fleet admitted NOTHING. The second
// demand must be admitted on the same tick the first is skipped.
func TestEngineTickAdmitsOtherWorkDuringABootWindow(t *testing.T) {
	now := time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)
	config := liveIncarnationConfig()
	store := &tickStore{fakeDemandStore: fakeDemandStore{
		statistics: operations.DemandStatistics{MessageID: 1, Available: 2, ObservedAt: now},
		records: []operations.DemandRecord{
			{Status: operations.DemandJobAvailable, RunnerRequestID: 11, Owner: "owner", Repository: "repo",
				WorkflowRunID: 8, QueueTime: now.Add(-2 * time.Minute)},
			{Status: operations.DemandJobAvailable, RunnerRequestID: 12, Owner: "owner", Repository: "repo",
				WorkflowRunID: 8, QueueTime: now.Add(-time.Minute)}}}}
	host := domain.Host{Available: config.LinuxCapacity,
		Pressure: domain.HostPressure{FreeDiskGB: 200, AdmissionAllowed: true}}
	engine := Engine{Store: store, Demand: DemandCoordinator{Store: store},
		Inventory: fakeInventory{instances: domain.Fresh([]domain.Instance{incarnation(domain.InstanceBooting)}, now),
			host: domain.Fresh(host, now)},
		Config: config, Bindings: []Binding{{ScaleSetID: 1, Profile: config.Profiles["small"]}},
		ControllerID: "controller", Mode: reconcile.Authority, Now: func() time.Time { return now }}

	result, err := engine.Tick(context.Background())
	if err != nil || !result.Applied {
		t.Fatalf("the boot window must not wedge the tick: applied=%t err=%v", result.Applied, err)
	}
	spawned := make([]domain.DemandKey, 0, len(result.Plan.Operations))
	for _, operation := range result.Plan.Operations {
		if operation.Kind == scheduler.OperationSpawn {
			spawned = append(spawned, operation.Demand)
		}
	}
	want := domain.DemandKey{Repo: "owner/repo", RunID: 8, Attempt: 1, JobID: 12}
	if len(spawned) != 1 || spawned[0] != want {
		t.Fatalf("the tick must admit the queued sibling and nothing else: %v", spawned)
	}
}

// TestPlannableDemandsDropsOnlyLiveIncarnations pins the seam itself across its
// branches, so the rule cannot be re-narrowed to one admission path again.
func TestPlannableDemandsDropsOnlyLiveIncarnations(t *testing.T) {
	live := domain.Demand{Key: liveIncarnationKey}
	free := domain.Demand{Key: domain.DemandKey{Repo: "owner/repo", RunID: 8, Attempt: 1, JobID: 12}}
	for _, testCase := range []struct {
		name   string
		queued []domain.Demand
		live   map[domain.DemandKey]bool
		want   []domain.DemandKey
	}{
		{name: "empty queue", live: map[domain.DemandKey]bool{liveIncarnationKey: true}},
		{name: "nothing incarnated", queued: []domain.Demand{live, free},
			want: []domain.DemandKey{liveIncarnationKey, free.Key}},
		{name: "head incarnated", queued: []domain.Demand{live, free}, live: map[domain.DemandKey]bool{liveIncarnationKey: true},
			want: []domain.DemandKey{free.Key}},
		{name: "all incarnated", queued: []domain.Demand{live, free},
			live: map[domain.DemandKey]bool{liveIncarnationKey: true, free.Key: true}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got := plannableDemands(testCase.queued, testCase.live)
			if len(got) != len(testCase.want) {
				t.Fatalf("plannableDemands() = %#v, want %v", got, testCase.want)
			}
			for index, key := range testCase.want {
				if got[index].Key != key {
					t.Fatalf("plannableDemands()[%d] = %v, want %v", index, got[index].Key, key)
				}
			}
		})
	}
}
