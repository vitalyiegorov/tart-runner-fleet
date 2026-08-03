package simulation_test

import (
	"context"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/sqlite"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/reconcile"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

// This file pins history. Three production incidents that already have ADRs are
// re-expressed as deterministic simulator traces, proving the harness reaches
// the state each incident reached; and FINDING 1 -- the defect this harness found
// first -- is pinned as a minimal two-tick reproduction over the real store.

// ---------------------------------------------------------------------------
// FINDING 1
// ---------------------------------------------------------------------------

// TestFindingRespawnOfALiveIncarnation is the smallest complete statement of
// FINDING 1, and it needs no simulator at all: a real store, a real
// DemandCoordinator, a real Engine, one demand, two consecutive ticks.
//
// GitHub keeps a job Available until a runner acquires it, and production
// acquires at the reachable -> registering edge, minutes after the spawn. In
// between, ActiveDemands still returns the demand as JobAvailable, the statistics
// bound still admits it, and nothing between the queue and the plan filters a
// demand a live instance already incarnates -- only the DEGRADED trickle path in
// app.Tick does, via domain.Instance.IncarnatesDemand.
//
// So the second tick re-derives the byte-identical content-addressed spawn, and
// ApplyPlan refuses it on the instances primary key. The plan is a pure function
// of its inputs, so every tick in the boot window rebuilds the same refusal and
// admits nothing else while it does.
func TestFindingRespawnOfALiveIncarnation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = store.Close() }()

	now := time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)
	profile := domain.Profile{ID: "small", Platform: domain.PlatformLinux, Route: "linux-small",
		Resources: domain.Resources{CPU: 1, MemoryMB: 2_048, Slots: 1}}
	binding := app.Binding{StoreKey: 1, ScaleSetID: 1, Scope: "sim/small",
		ScaleSetLabels: []string{"self-hosted", "linux-small"}, Profile: profile}
	if _, err := store.ApplyDemandBatch(ctx, 1, 1, []operations.DemandEvent{{
		Kind: operations.DemandJobAvailable, RunnerRequestID: 77, Owner: "a", Repository: "repo",
		WorkflowRunID: 9, JobID: "job-77", DisplayName: "build", WorkflowRef: "refs/heads/main",
		EventName: "push", Labels: []string{"self-hosted", "linux-small"}, QueueTime: now.Add(-time.Minute),
	}}); err != nil {
		t.Fatalf("apply demand batch: %v", err)
	}
	if _, err := store.PutDemandStatistics(ctx, 1, operations.DemandStatistics{MessageID: 1, Available: 1, ObservedAt: now}); err != nil {
		t.Fatalf("publish statistics: %v", err)
	}

	engine := app.Engine{Store: store, Inventory: durableInventory{store: store, now: func() time.Time { return now }},
		Config: scheduler.Config{LinuxCapacity: domain.Resources{CPU: 10, MemoryMB: 22_528, Slots: 4},
			FairnessAge: 5 * time.Minute, RepoCaps: map[string]int{"a/repo": 4},
			Profiles: map[domain.ProfileID]domain.Profile{"small": profile}},
		Bindings: []app.Binding{binding}, ControllerID: simOwner, Mode: reconcile.Authority,
		Now: func() time.Time { return now }}

	first, err := engine.Tick(ctx)
	if err != nil || !first.Applied || len(first.Plan.Operations) != 1 {
		t.Fatalf("first tick must spawn exactly once: applied=%t ops=%d err=%v", first.Applied, len(first.Plan.Operations), err)
	}
	now = now.Add(20 * time.Second)
	second, secondErr := engine.Tick(ctx)
	if secondErr == nil {
		t.Fatalf("FINDING 1 no longer reproduces: the second tick was accepted (applied=%t ops=%d)",
			second.Applied, len(second.Plan.Operations))
	}
	if second.Applied {
		t.Fatalf("a refused commit must not report applied: %#v", second.Plan)
	}
	// The characterization: an identical spawn for a demand whose instance is
	// still cloning, refused by the durable layer, reported as a rejected plan.
	if len(second.Plan.Operations) != 1 || second.Plan.Operations[0].Kind != scheduler.OperationSpawn {
		t.Fatalf("finding changed shape: %#v", second.Plan.Operations)
	}
	if reason := app.ReasonPlanCommitRejected; !containsText(secondErr.Error(), reason) {
		t.Fatalf("expected %s, got %v", reason, secondErr)
	}
	// And the property oracle names it, which is what lets the sweep tolerate it
	// deliberately instead of by accident.
	observation := tickObservation{Tick: 2, Now: now, Plan: second.Plan, Applied: second.Applied,
		Err: secondErr, Demands: second.Demands, Instances: second.Instances}
	findings := noDoubleAdmissionChecker()(&world{}, observation)
	if len(findings) != 1 || findings[0].Signature != sigRespawnLiveIncarnation {
		t.Fatalf("property (e) must name this defect, got %v", findings)
	}
}

func containsText(haystack, needle string) bool {
	for index := 0; index+len(needle) <= len(haystack); index++ {
		if haystack[index:index+len(needle)] == needle {
			return true
		}
	}
	return false
}

// durableInventory is the smallest honest inventory: the durable rows, powered
// on, against a fixed envelope. FINDING 1 needs no host or Tart nondeterminism to
// reproduce, and leaving them out is what makes the reproduction unarguable.
type durableInventory struct {
	store *sqlite.Store
	now   func() time.Time
}

func (d durableInventory) Observe(ctx context.Context) (domain.Observation[[]domain.Instance], domain.Observation[domain.Host]) {
	stored, err := d.store.LiveInstances(ctx)
	if err != nil {
		return domain.Unavailable[[]domain.Instance]("durable instance inventory unavailable"),
			domain.Unavailable[domain.Host]("durable instance inventory unavailable")
	}
	instances := make([]domain.Instance, 0, len(stored))
	for _, instance := range stored {
		instances = append(instances, domain.Instance{ID: instance.ID, Repo: instance.Repo, Demand: instance.Demand,
			Platform: instance.Platform, Profile: instance.Profile, Route: instance.Route,
			Resources: instance.Resources, State: instance.State, Power: domain.InstancePowerRunning})
	}
	host := domain.Host{Available: domain.Resources{CPU: 10, MemoryMB: 22_528, Slots: 4}}
	return domain.Fresh(instances, d.now()), domain.Fresh(host, d.now())
}

// ---------------------------------------------------------------------------
// Incident 2026-08-01 -- ADR 0026, the ghost queued demand
// ---------------------------------------------------------------------------

// TestGhostQueuedDemandIncidentReplaysThroughTheSimulator drives the whole
// 2026-08-01 incident through the harness: a run cancelled server side that the
// broker keeps advertising, a VM spawned for it that registers and never goes
// busy, a queue that never reaches zero, and a fleet that cannot become
// quiescent -- which is what blocked every production release that night.
//
// The trace is one arrival and one silent cancellation. Everything else is the
// composition: complete REST snapshots accrue absence, both of ADR 0026's bounds
// close, the demand is retired as a revocable conclusion, and the fleet settles.
func TestGhostQueuedDemandIncidentReplaysThroughTheSimulator(t *testing.T) {
	t.Parallel()
	cfg := defaultWorld()
	trace := simTrace{Seed: 20_260_801, Ticks: 80, Config: cfg.Name, Events: []simEvent{
		{Tick: 2, Kind: eventArrive, Repo: "a/repo", Profile: "large", Event: "push"},
		{Tick: 3, Kind: eventSilentCancel},
		{Tick: 4, Kind: eventStopArrivals},
	}}
	w := newWorld(t, cfg, trace)
	defer w.close()
	if findings := w.run(); len(findings) > 0 {
		t.Fatalf("the ghost incident must no longer violate a property: %s", findings[0])
	}
	// While the ghost is durable the queue is non-empty and a runner exists for
	// it: the fleet is doing exactly what the incident describes.
	if !w.observationsShowGhost() {
		t.Fatal("the harness never reproduced the ghost: no tick carried queued demand for the cancelled run")
	}
	// And the REST evidence eventually retires it, which is the ADR 0026 repair.
	final := w.observations[len(w.observations)-1]
	if len(final.Demands) != 0 {
		t.Fatalf("proven-absent demand was never retired: %d still queued", len(final.Demands))
	}
	if live := w.liveInstanceCount(); live != 0 {
		t.Fatalf("the fleet never settled: %d live instances", live)
	}
	if pending := w.pendingOperations(); pending != 0 {
		t.Fatalf("the outbox never drained: %d operations", pending)
	}
}

// observationsShowGhost reports whether any tick saw the cancelled run still
// queued, which is the incident state the harness must be able to reach.
func (w *world) observationsShowGhost() bool {
	for _, observation := range w.observations {
		if observation.Tick > 6 && len(observation.Demands) > 0 {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Incident 2026-08-02 -- ADR 0027, one tick admitting a demand twice
// ---------------------------------------------------------------------------

// TestDoubleAdmissionIncidentIsPinnedByPropertyE re-expresses the 2026-08-02
// wedge. The incident's own artifact was a single plan carrying two operations
// with one content address and two instance intents with one instance name, so
// this pins both halves: the oracle names that artifact, and today's scheduler in
// the incident's exact topology emits the demand once.
func TestDoubleAdmissionIncidentIsPinnedByPropertyE(t *testing.T) {
	t.Parallel()
	// The incident topology: a busy xl Linux runner holding six of ten cores, an
	// aged macOS builder heading the global FIFO that cannot fit beside it, and an
	// aged smallest-tier Linux job that fits the residual. The historical plan
	// admitted that Linux job twice -- once as bounded handoff backfill, once
	// through the complementary remainder pass.
	config := findingConfig(nil)
	live := []domain.Instance{findingInstance("trf-xl-live", "a/repo", "xl", domain.InstanceRunning)}
	head := findingDemand("b/repo", 51, 80*time.Minute, "builder")
	contested := findingDemand("a/repo", 52, 70*time.Minute, "small")

	plan := scheduler.PlanTick(scheduler.Input{Now: findingNow, Config: config,
		Demands:   domain.Fresh([]domain.Demand{head, contested}, findingNow),
		Instances: domain.Fresh(live, findingNow),
		Host:      domain.Fresh(domain.Host{Available: domain.Resources{CPU: 4, MemoryMB: 10_240, Slots: 3}}, findingNow)})
	admitted := spawnedKeys(plan)
	if len(admitted) != 1 || admitted[0] != contested.Key {
		t.Fatalf("ADR 0027 regressed: the contested demand must be admitted exactly once, got %v", admitted)
	}

	// The historical artifact, replayed against the oracle. This is what the
	// harness would have reported on 2026-08-02 at 09:59:51Z.
	historical := plan
	historical.Operations = append(append([]scheduler.Operation(nil), plan.Operations...), plan.Operations[0])
	observation := tickObservation{Tick: 1, Now: findingNow, Plan: historical,
		Demands: []domain.Demand{head, contested}, Instances: live}
	findings := noDoubleAdmissionChecker()(&world{}, observation)
	if len(findings) != 1 || findings[0].Kind != findingDoubleAdmit || findings[0].Signature != "" {
		t.Fatalf("property (e) must flag the historical plan as a hard violation, got %v", findings)
	}
	// A duplicated operation is also a duplicated identity, so property (d) is a
	// second, independent witness of the same incident.
	if identity := identityUniquenessChecker()(&world{}, observation); len(identity) != 1 {
		t.Fatalf("property (d) must flag the repeated operation identity, got %v", identity)
	}
}

// ---------------------------------------------------------------------------
// Incident 2026-07-25 -- ADR 0017, the stranded residual behind a wedged drain
// ---------------------------------------------------------------------------

// TestStrandedResidualIncidentReplaysThroughTheSimulator drives the 2026-07-25
// shape end to end: a drain GitHub will not let finish holds a large vector, an
// aged head cannot fit what remains, and the residual is big enough for smaller
// work. Before ADR 0017 the fleet admitted nothing for forty-five minutes on a
// host reporting 62-77% CPU idle; property (a) is exactly that measurement.
func TestStrandedResidualIncidentReplaysThroughTheSimulator(t *testing.T) {
	t.Parallel()
	cfg := defaultWorld()
	trace := simTrace{Seed: 20_260_725, Ticks: 120, Config: cfg.Name, Events: []simEvent{
		{Tick: 2, Kind: eventArrive, Repo: "a/repo", Profile: "builder", Event: "push"},
		{Tick: 3, Kind: eventArrive, Repo: "a/repo", Profile: "xl", Event: "push"},
		{Tick: 4, Kind: eventArrive, Repo: "b/repo", Profile: "small", Event: "pull_request"},
		{Tick: 6, Kind: eventArrive, Repo: "b/repo", Profile: "medium", Event: "pull_request"},
		{Tick: 8, Kind: eventArrive, Repo: "c/repo", Profile: "small", Event: "push"},
		// GitHub refuses to deregister a runner it has brokered another job to, so
		// the drain sits in draining for twenty virtual minutes holding its vector.
		{Tick: 20, Kind: eventWedgedDrain, Count: 40},
		{Tick: 24, Kind: eventArrive, Repo: "c/repo", Profile: "medium", Event: "push"},
		{Tick: 30, Kind: eventArrive, Repo: "b/repo", Profile: "small", Event: "push"},
		{Tick: 60, Kind: eventStopArrivals},
	}}
	w := newWorld(t, cfg, trace)
	defer w.close()
	if findings := w.run(); len(findings) > 0 {
		t.Fatalf("the stranded-residual incident must no longer violate a property: %s", findings[0])
	}
	if !w.wedgedADrain() {
		t.Fatal("the harness never reproduced the incident: no drain was ever held open")
	}
	if admissions := w.admissionsAfter(20); admissions == 0 {
		t.Fatal("no work was admitted while a drain was wedged: the 2026-07-25 wedge is back")
	}
}

// wedgedADrain reports whether any observed tick carried an instance held in the
// draining state, which is the incident's defining condition.
func (w *world) wedgedADrain() bool {
	for _, observation := range w.observations {
		for _, instance := range observation.Instances {
			if instance.State == domain.InstanceDraining {
				return true
			}
		}
	}
	return false
}

// admissionsAfter counts spawns planned from the given tick onward.
func (w *world) admissionsAfter(tick int) int {
	admissions := 0
	for _, observation := range w.observations {
		if observation.Tick >= tick {
			admissions += len(observation.spawns())
		}
	}
	return admissions
}
