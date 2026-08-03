package simulation_test

import (
	"context"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/sqlite"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/lifecycle"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/reconcile"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

// This file pins history. Three production incidents that already have ADRs are
// re-expressed as deterministic simulator traces, proving the harness reaches
// the state each incident reached; and FINDING 1 -- the defect this harness found
// first -- is pinned as a minimal three-tick regression over the real store.

// ---------------------------------------------------------------------------
// FINDING 1 -- fixed 2026-08-03, ADR 0027's cross-tick amendment
// ---------------------------------------------------------------------------

// TestFindingRespawnOfALiveIncarnation is the smallest complete statement of
// FINDING 1, and it needs no simulator at all: a real store, a real
// DemandCoordinator, a real Engine, one demand, three consecutive ticks.
//
// GitHub keeps a job Available until a runner acquires it, and production
// acquires at the reachable -> registering edge, minutes after the spawn. In
// between, ActiveDemands still returns the demand as JobAvailable and the
// statistics bound still admits it. Before the fix nothing between the queue and
// the plan filtered a demand a live instance already incarnates -- only the
// degraded trickle path did -- so the second tick re-derived the byte-identical
// content-addressed spawn, ApplyPlan refused it on the instances primary key,
// and because a refused plan is discarded whole the tick admitted nothing else.
// The plan is a pure function of its inputs, so every tick of the boot window
// rebuilt the same refusal.
//
// app.plannableDemands is now the one seam that assembles the scheduler's queue,
// so the second tick plans nothing. The third tick proves the other half of the
// rule: a TERMINAL incarnation releases its demand, which is how an instance
// failure is retried.
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
	spawned := firstInstance(t, store)

	now = now.Add(20 * time.Second)
	second, secondErr := engine.Tick(ctx)
	if secondErr != nil {
		t.Fatalf("FINDING 1 is back: the boot window refused a tick: %v", secondErr)
	}
	if len(second.Plan.Operations) != 0 {
		t.Fatalf("a demand its own VM is already booting must not be planned again: %#v", second.Plan.Operations)
	}
	if len(second.Demands) != 0 {
		t.Fatalf("the plannable queue must not carry an incarnated demand: %#v", second.Demands)
	}
	// The queue itself stays fully visible: filtering the plannable queue is not
	// hiding durable demand from the SLO monitor.
	if second.Queues["small"].Count != 1 {
		t.Fatalf("queue visibility lost during the boot window: %#v", second.Queues)
	}
	// And the property (e) oracle -- the one that named this defect -- is clean.
	observation := tickObservation{Tick: 2, Now: now, Plan: second.Plan, Applied: second.Applied,
		Err: secondErr, Demands: second.Demands, Instances: second.Instances}
	if findings := noDoubleAdmissionChecker()(&world{}, observation); len(findings) != 0 {
		t.Fatalf("property (e) must be clean, got %v", findings)
	}

	// The lifecycle edge the fix must not break: the VM fails, releasing the
	// demand, and the next tick retries it as a fresh attempt (ADR 0028).
	failInstance(t, store, spawned)
	now = now.Add(20 * time.Second)
	third, thirdErr := engine.Tick(ctx)
	if thirdErr != nil || !third.Applied {
		t.Fatalf("a failed incarnation must be retried: applied=%t err=%v", third.Applied, thirdErr)
	}
	if len(third.Plan.Operations) != 1 || third.Plan.Operations[0].Kind != scheduler.OperationSpawn {
		t.Fatalf("the retry must be exactly one spawn: %#v", third.Plan.Operations)
	}
	if retried := firstInstance(t, store); retried == spawned {
		t.Fatalf("the retry reused the failed identity %q", spawned)
	}
}

// firstInstance is the identity of the sole non-terminal instance the store
// holds, so the regression can name the incarnation it drives.
func firstInstance(t *testing.T, store *sqlite.Store) string {
	t.Helper()
	live, err := store.LiveInstances(context.Background())
	if err != nil {
		t.Fatalf("read live instances: %v", err)
	}
	for _, instance := range live {
		if instance.State != operations.StateFailed {
			return instance.ID
		}
	}
	t.Fatalf("no live instance: %#v", live)
	return ""
}

// failInstance drives one instance to the terminal failed state through the real
// durable transition, which is what releases its demand for a retry.
func failInstance(t *testing.T, store *sqlite.Store, id string) {
	t.Helper()
	ctx := context.Background()
	instance, err := store.Instance(ctx, id)
	if err != nil {
		t.Fatalf("read instance %s: %v", id, err)
	}
	if _, err := store.Advance(ctx, lifecycle.StateChange{InstanceID: id, ExpectedState: instance.State,
		ExpectedVersion: instance.Version, NextState: operations.StateFailed, FailureCode: "clone_failed"}); err != nil {
		t.Fatalf("fail instance %s: %v", id, err)
	}
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
	if final.Queued != 0 {
		t.Fatalf("proven-absent demand was never retired: %d still queued", final.Queued)
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
//
// It reads the durable queue depth rather than the plannable one: the ghost's
// own runner is live for most of the window, so the plannable queue correctly
// excludes it, while the queue the incident was reported from -- and the one the
// SLO monitor watches -- still carries it.
func (w *world) observationsShowGhost() bool {
	for _, observation := range w.observations {
		if observation.Tick > 6 && observation.Queued > 0 {
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
// Incident 2026-08-02 -- ADR 0033, the sibling handoff (issue #123)
// ---------------------------------------------------------------------------

// TestCrossedSiblingHandoffIncidentReplaysThroughTheSimulator drives the whole
// 2026-08-02 churn end to end.
//
// The trace is three events. Two builder jobs of one repository arrive, and
// MaxActive is one, so exactly one VM is spawned and the second job stays
// queued -- the sibling. The broker then hands that queued sibling to the VM's
// runner and returns the VM's own request to the queue, which is exactly what
// `runner_demands` recorded for `trf-xl-25a374b60f46dafe`: one request
// JobStarted on the runner, its sibling still JobAvailable. The sibling is a
// long job, because the whole incident turns on work that outlives a recovery
// deadline.
//
// Before ADR 0033 this produced, deterministically:
//
//   - a binding that named a job GitHub had given away, so `JobActive` (bound
//     demand) reported idle while `RunnerActiveJob` (runner name) reported busy;
//   - a stalled-assignment drain at the assignment deadline, aborted by GitHub's
//     refusal to deregister a busy runner, then a lingering-runner drain one
//     idle-runner deadline later, aborted the same way -- the 09:44:48Z and
//     09:59:51Z operations of the incident;
//   - a released request nobody could ever spawn for, whose queue age grew for
//     as long as the fleet stayed up.
//
// After it, the binding follows GitHub, both jobs run, and the fleet settles.
func TestCrossedSiblingHandoffIncidentReplaysThroughTheSimulator(t *testing.T) {
	t.Parallel()
	cfg := defaultWorld()
	trace := simTrace{Seed: 20_260_802, Ticks: 200, Config: cfg.Name, Events: []simEvent{
		{Tick: 1, Kind: eventLongJob, Count: 14},
		{Tick: 2, Kind: eventArrive, Repo: "a/repo", Profile: "builder", Event: "pull_request"},
		{Tick: 3, Kind: eventArrive, Repo: "a/repo", Profile: "builder", Event: "pull_request"},
		{Tick: 4, Kind: eventSiblingSubstitute},
		{Tick: 5, Kind: eventStopArrivals},
	}}
	w := newWorld(t, cfg, trace)
	defer w.close()
	if findings := w.run(); len(findings) > 0 {
		t.Fatalf("the sibling-handoff incident must no longer violate a property: %s", findings[0])
	}
	// The harness really did reach the incident state; a green run over a trace
	// that never handed a sibling over would prove nothing.
	if w.substitutions != 1 {
		t.Fatalf("the harness never reproduced the handoff: %d substitutions", w.substitutions)
	}
	// The runner that was given the sibling is never drained while it works: no
	// recovery drain is planned for it at all, so none has to be aborted.
	if len(w.drainAborts) != 0 {
		t.Fatalf("the churn is back: %v", w.drainAborts)
	}
	// Both sibling jobs ran. The released one is the whole point -- before the
	// repair it could neither be respawned nor executed.
	for _, job := range w.jobs {
		if job.status != jobDone {
			t.Fatalf("job %d never completed: status %s runner %q", job.requestID, job.status, job.runner)
		}
	}
	// jobs are recorded in arrival order, and the substitution released the first
	// one -- the request whose VM was spawned -- in favour of the queued second.
	released, sibling := w.jobs[0], w.jobs[1]
	// The released demand waited for CAPACITY and nothing else: the only builder
	// slot was held by the long sibling, and the fleet served it within a boot of
	// that slot coming free. Before the repair it waited for the whole run.
	if wait := released.startedAt.Sub(sibling.finishedAt); wait <= 0 || wait > 8*simTick {
		t.Fatalf("the released demand started %s after capacity freed, not promptly", wait)
	}
	// Its queue age is GitHub's, untouched by the rebind (ADR 0033). The
	// scheduler therefore ranks it by how long the job has really been waiting,
	// which is what keeps aging an honest starvation guard.
	if !w.queueCarried(released.requestID, released.queuedAt) {
		t.Fatal("the released demand never re-entered the plannable queue with its original queue time")
	}
	if live := w.liveInstanceCount(); live != 0 {
		t.Fatalf("the fleet never settled: %d live instances", live)
	}
	final := w.observations[len(w.observations)-1]
	if final.Queued != 0 {
		t.Fatalf("the released demand never drained: %d still queued", final.Queued)
	}
}

// queueCarried reports whether any observed tick offered the scheduler this
// request with exactly the queue time GitHub gave it.
func (w *world) queueCarried(requestID int64, queuedAt time.Time) bool {
	for _, observation := range w.observations {
		for _, demand := range observation.Demands {
			if demand.Key.JobID == requestID && demand.CreatedAt.Equal(queuedAt) {
				return true
			}
		}
	}
	return false
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
