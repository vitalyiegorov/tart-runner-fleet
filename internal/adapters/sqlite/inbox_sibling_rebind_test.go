package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// This file is the seam regression for issue #123: GitHub's scale-set broker
// hands a registered runner a SIBLING job from the same scale set, and no VM
// incarnates that sibling, so there is nothing for ADR 0016's crossed-assignment
// swap to swap with.
//
// The durable evidence from host vitalii-mac-mini on 2026-08-02 is reproduced
// exactly: two jobs of workflow run 30740997047 dispatched 41 ms apart, a VM
// bound to "Maestro (expo)", and a JobStarted for "Maestro (bare)" naming that
// VM's runner.

const (
	siblingScaleSet = 7
	// boundRequest is "Maestro (expo)" — the demand the fleet spawned the VM for.
	boundRequest int64 = 8_670_852_748_984_054_370
	// dispatchedRequest is "Maestro (bare)" — the sibling GitHub actually ran.
	dispatchedRequest int64 = 4_961_232_247_973_546_403
	siblingRunner           = "trf-xl-25a374b60f46dafe"
	siblingRepo             = "rnw-community/rnw-community"
	siblingRunID      int64 = 30_740_997_047
)

var (
	boundQueuedAt      = time.Date(2026, 8, 2, 9, 3, 24, 19_000_000, time.UTC)
	dispatchedQueuedAt = time.Date(2026, 8, 2, 9, 3, 24, 60_000_000, time.UTC)
)

// siblingDemandEvent is one broker event for one of the two sibling jobs.
func siblingDemandEvent(kind operations.DemandEventKind, requestID int64, queuedAt time.Time, runner string) operations.DemandEvent {
	name := "Maestro (expo)"
	if requestID == dispatchedRequest {
		name = "Maestro (bare)"
	}
	return operations.DemandEvent{Kind: kind, RunnerRequestID: requestID, Owner: "rnw-community",
		Repository: "rnw-community", WorkflowRunID: siblingRunID, JobID: name, DisplayName: name,
		WorkflowRef: "refs/heads/main", EventName: "pull_request", Labels: []string{"self-hosted", "macos-maestro"},
		QueueTime: queuedAt, RunnerName: runner}
}

// siblingDemandKey is the scheduling identity of one of the two sibling jobs.
func siblingDemandKey(requestID int64) domain.DemandKey {
	return domain.DemandKey{Repo: siblingRepo, RunID: siblingRunID, Attempt: 1, JobID: requestID}
}

// siblingInstance is the VM the fleet spawned for the bound request, in a state
// that has a live GitHub runner. Ownership carries the demand key exactly as
// reconcile.Controller writes it, because that signature is what SpawnGeneration
// counts a spent incarnation by.
func siblingInstance(state operations.State, requestID int64) operations.Instance {
	return operations.Instance{
		ID: siblingRunner, Repo: siblingRepo, Platform: domain.PlatformMacOS, Profile: "maestro",
		Route: "macos-maestro", Resources: domain.Resources{CPU: 4, MemoryMB: 7_168, Slots: 1},
		Demand: siblingDemandKey(requestID), State: state,
		Ownership: operations.Ownership{ControllerID: "controller",
			ResourceID: siblingDemandKey(requestID).String(), OperationID: "spawn-expo"},
	}
}

// seedSiblingJobs makes both jobs durably queued, exactly as the incident's
// runner_demands table showed them.
func seedSiblingJobs(t *testing.T, store *Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.ApplyDemandBatch(ctx, siblingScaleSet, 1, []operations.DemandEvent{
		siblingDemandEvent(operations.DemandJobAvailable, boundRequest, boundQueuedAt, ""),
		siblingDemandEvent(operations.DemandJobAvailable, dispatchedRequest, dispatchedQueuedAt, ""),
	}); err != nil {
		t.Fatalf("seed sibling demand: %v", err)
	}
}

// TestSiblingHandoffRebindsTheRunnerToTheJobGitHubDispatched is the whole defect
// and its repair in one statement.
//
// Before the repair the JobStarted naming this runner was refused as uncertain
// forever: the instance stayed Assigned holding a demand it would never execute,
// `JobActive` (bound demand) said idle while `RunnerActiveJob` (runner name)
// said busy, and every idle-runner deadline planned a drain the executor then
// aborted. The released demand could never be respawned either, because its own
// instance still incarnated it.
func TestSiblingHandoffRebindsTheRunnerToTheJobGitHubDispatched(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	seedSiblingJobs(t, store)
	if err := store.CreateInstance(ctx, siblingInstance(operations.StateAssigned, boundRequest)); err != nil {
		t.Fatalf("create bound instance: %v", err)
	}

	started := siblingDemandEvent(operations.DemandJobStarted, dispatchedRequest, dispatchedQueuedAt, siblingRunner)
	if _, err := store.ApplyDemandBatch(ctx, siblingScaleSet, 2, []operations.DemandEvent{started}); err != nil {
		t.Fatalf("apply sibling start: %v", err)
	}
	if err := store.ProjectDemandEvent(ctx, siblingScaleSet, started); err != nil {
		t.Fatalf("the sibling handoff must be projected, not refused: %v", err)
	}

	rebound, err := store.Instance(ctx, siblingRunner)
	if err != nil {
		t.Fatalf("read rebound instance: %v", err)
	}
	// GitHub is the single source of truth for what a runner is executing.
	if rebound.Demand.JobID != dispatchedRequest {
		t.Fatalf("the binding still names a job GitHub did not dispatch: %#v", rebound.Demand)
	}
	dispatched, err := store.DemandRecord(ctx, siblingScaleSet, dispatchedRequest)
	if err != nil {
		t.Fatalf("read dispatched demand: %v", err)
	}
	if rebound.Demand != dispatched.DemandKey() {
		t.Fatalf("the rebound key %s is not the queue's key %s for the same job", rebound.Demand, dispatched.DemandKey())
	}
	// The FSM followed the event it could finally attribute.
	if rebound.State != operations.StateRunning {
		t.Fatalf("a started job must advance the instance: %s", rebound.State)
	}
	// Provenance is immutable: ADR 0016's rule that ownership signatures are never
	// rewritten holds for a rebind exactly as it does for a swap.
	if rebound.Ownership.ResourceID != siblingDemandKey(boundRequest).String() || rebound.Ownership.OperationID != "spawn-expo" {
		t.Fatalf("rebind rewrote immutable ownership: %#v", rebound.Ownership)
	}

	// The two predicates that disagreed by construction now read one fact.
	busy, err := store.RunnerActiveJob(ctx, siblingScaleSet, siblingRunner)
	if err != nil || !busy {
		t.Fatalf("runner-keyed evidence = %v, %v", busy, err)
	}
	bound, err := store.DemandRecord(ctx, siblingScaleSet, rebound.Demand.JobID)
	if err != nil || bound.Status != operations.DemandJobStarted {
		t.Fatalf("demand-keyed evidence = %#v, %v", bound, err)
	}

	// The released demand is an ordinary queued demand again, with the queue time
	// GitHub gave it: it has genuinely never run, so ADR 0033 keeps its age
	// rather than resetting it at the rebind instant.
	released, err := store.DemandRecord(ctx, siblingScaleSet, boundRequest)
	if err != nil {
		t.Fatalf("read released demand: %v", err)
	}
	if released.Status != operations.DemandJobAvailable {
		t.Fatalf("the released demand changed status: %s", released.Status)
	}
	if !released.FirstQueueTime.Equal(boundQueuedAt) {
		t.Fatalf("the released demand lost its queue age: %s want %s", released.FirstQueueTime, boundQueuedAt)
	}
	active, err := store.ActiveDemands(ctx, siblingScaleSet)
	if err != nil {
		t.Fatalf("read active demands: %v", err)
	}
	queued := make([]int64, 0, len(active))
	for _, record := range active {
		if record.Status == operations.DemandJobAvailable {
			queued = append(queued, record.RunnerRequestID)
		}
	}
	if len(queued) != 1 || queued[0] != boundRequest {
		t.Fatalf("the released demand is not back in the queue: %#v", queued)
	}
	// And nothing incarnates it any more, which is what lets the scheduler spawn
	// a fresh VM for it (app.plannableDemands keys on exactly this).
	live, err := store.LiveInstances(ctx)
	if err != nil {
		t.Fatalf("read live instances: %v", err)
	}
	for _, instance := range live {
		if instance.Demand.JobID == boundRequest {
			t.Fatalf("instance %s still incarnates the released demand", instance.ID)
		}
	}
}

// TestSiblingHandoffRebindCoversEveryStateWithARunner pins the lower boundary of
// the rebindable set, which is load-bearing rather than cosmetic.
//
// ProvisionExecutor acquires the job on the reachable -> registering edge, so the
// runner exists while the row still says `reachable`, and a broker message naming
// it arrives inside that window. The deterministic simulation hit exactly that
// tick; a rebind refused there is never retried, because the event is not
// redelivered once its batch has been committed.
func TestSiblingHandoffRebindCoversEveryStateWithARunner(t *testing.T) {
	for _, state := range []operations.State{operations.StateReachable, operations.StateRegistering,
		operations.StateOnlineIdle, operations.StateAssigned, operations.StateRunning} {
		t.Run(string(state), func(t *testing.T) {
			ctx := context.Background()
			store := testStore(t)
			seedSiblingJobs(t, store)
			if err := store.CreateInstance(ctx, siblingInstance(state, boundRequest)); err != nil {
				t.Fatalf("create instance: %v", err)
			}
			record, err := store.DemandRecord(ctx, siblingScaleSet, dispatchedRequest)
			if err != nil {
				t.Fatalf("read dispatched demand: %v", err)
			}
			stored, err := store.Instance(ctx, siblingRunner)
			if err != nil {
				t.Fatalf("read instance: %v", err)
			}
			rebound, err := store.rebindRunnerDemand(ctx, stored, record)
			if err != nil {
				t.Fatalf("rebind from %s: %v", state, err)
			}
			if rebound.Demand.JobID != dispatchedRequest || rebound.State != state {
				t.Fatalf("rebind from %s produced %#v", state, rebound)
			}
		})
	}
}

// TestSiblingHandoffRebindFailsClosed pins every refusal. Each row keeps the
// pre-#123 answer — the event stays uncertain and is redelivered — because a
// rebind that is not provably safe must never move a binding.
func TestSiblingHandoffRebindFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name  string
		state operations.State
		// boundStatus advances the released demand before the handoff arrives, and
		// boundRunner names the runner GitHub says served it.
		boundStatus operations.DemandEventKind
		boundRunner string
	}{
		{name: "planned has no runner yet", state: operations.StatePlanned},
		{name: "cloning has no runner yet", state: operations.StateCloning},
		{name: "booting has no runner yet", state: operations.StateBooting},
		{name: "draining is already being reclaimed", state: operations.StateDraining},
		{name: "deregistering is already being reclaimed", state: operations.StateDeregistering},
		{name: "stopping is already being reclaimed", state: operations.StateStopping},
		{name: "failed is terminal", state: operations.StateFailed},
		{name: "the released demand already started here", state: operations.StateRunning,
			boundStatus: operations.DemandJobStarted, boundRunner: siblingRunner},
		{name: "the released demand already completed here", state: operations.StateRunning,
			boundStatus: operations.DemandJobCompleted, boundRunner: siblingRunner},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store := testStore(t)
			seedSiblingJobs(t, store)
			instance := siblingInstance(test.state, boundRequest)
			if err := store.CreateInstance(ctx, instance); err != nil {
				t.Fatalf("create instance: %v", err)
			}
			if test.boundStatus != "" {
				bound := siblingDemandEvent(test.boundStatus, boundRequest, boundQueuedAt, test.boundRunner)
				if _, err := store.ApplyDemandBatch(ctx, siblingScaleSet, 2, []operations.DemandEvent{bound}); err != nil {
					t.Fatalf("advance bound demand: %v", err)
				}
			}
			started := siblingDemandEvent(operations.DemandJobStarted, dispatchedRequest, dispatchedQueuedAt, siblingRunner)
			if _, err := store.ApplyDemandBatch(ctx, siblingScaleSet, 3, []operations.DemandEvent{started}); err != nil {
				t.Fatalf("apply sibling start: %v", err)
			}
			if err := store.ProjectDemandEvent(ctx, siblingScaleSet, started); !errors.Is(err, operations.ErrUncertain) {
				t.Fatalf("rebind must fail closed here, got %v", err)
			}
			after, err := store.Instance(ctx, siblingRunner)
			if err != nil || after.Demand.JobID != boundRequest || after.State != test.state {
				t.Fatalf("a refused rebind moved the instance: %#v, %v", after, err)
			}
		})
	}
}

// TestSiblingHandoffRebindFollowsGitHubAcrossRepositories pins the widest form
// of the handoff, which the deterministic simulation reaches by composing a
// crossed assignment with a substitution: one scale set serves several
// repositories, so the sibling GitHub dispatches need not come from the runner's
// own repository — or even its own workflow run.
//
// The instance's repository moves with its binding. It has to: scheduling
// metadata is only valid when the two agree, repository caps must charge the
// work actually being done, and control routing is keyed on the pair. It is safe
// because the dispatched row arrived through this instance's own scale set, so
// its repository is one the fleet already spawns instances into.
func TestSiblingHandoffRebindFollowsGitHubAcrossRepositories(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	foreign := operations.DemandEvent{Kind: operations.DemandJobAvailable, RunnerRequestID: dispatchedRequest,
		Owner: "rnw-community", Repository: "other-app", WorkflowRunID: 88, JobID: "Detox",
		DisplayName: "Detox", WorkflowRef: "refs/heads/main", EventName: "push",
		Labels: []string{"self-hosted", "macos-maestro"}, QueueTime: dispatchedQueuedAt}
	if _, err := store.ApplyDemandBatch(ctx, siblingScaleSet, 1, []operations.DemandEvent{
		siblingDemandEvent(operations.DemandJobAvailable, boundRequest, boundQueuedAt, ""), foreign,
	}); err != nil {
		t.Fatalf("seed demand: %v", err)
	}
	if err := store.CreateInstance(ctx, siblingInstance(operations.StateAssigned, boundRequest)); err != nil {
		t.Fatalf("create bound instance: %v", err)
	}
	started := foreign
	started.Kind, started.RunnerName = operations.DemandJobStarted, siblingRunner
	if _, err := store.ApplyDemandBatch(ctx, siblingScaleSet, 2, []operations.DemandEvent{started}); err != nil {
		t.Fatalf("apply foreign start: %v", err)
	}
	if err := store.ProjectDemandEvent(ctx, siblingScaleSet, started); err != nil {
		t.Fatalf("a cross-repository handoff must rebind: %v", err)
	}
	rebound, err := store.Instance(ctx, siblingRunner)
	if err != nil {
		t.Fatalf("read rebound instance: %v", err)
	}
	if rebound.Repo != "rnw-community/other-app" || rebound.Demand.Repo != rebound.Repo ||
		rebound.Demand.JobID != dispatchedRequest || rebound.Demand.RunID != 88 {
		t.Fatalf("the binding did not follow GitHub across repositories: %#v", rebound)
	}
	// The released demand is untouched and back in the queue with its own age.
	released, err := store.DemandRecord(ctx, siblingScaleSet, boundRequest)
	if err != nil || released.Status != operations.DemandJobAvailable || !released.FirstQueueTime.Equal(boundQueuedAt) {
		t.Fatalf("released demand = %#v, %v", released, err)
	}
	// And the next spawn for it gets a fresh identity rather than colliding with
	// the incarnation that walked away.
	generation, err := store.SpawnGeneration(ctx, siblingDemandKey(boundRequest))
	if err != nil || generation != 1 {
		t.Fatalf("a rebound incarnation must supersede: generation = %d, %v", generation, err)
	}
}

// TestSiblingHandoffRebindLosesToAConcurrentWriter proves the compare-and-set is
// the whole safety story: a row that moved between the read and the write keeps
// its binding and the at-least-once redelivery retries.
func TestSiblingHandoffRebindLosesToAConcurrentWriter(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	seedSiblingJobs(t, store)
	if err := store.CreateInstance(ctx, siblingInstance(operations.StateAssigned, boundRequest)); err != nil {
		t.Fatalf("create bound instance: %v", err)
	}
	stale, err := store.Instance(ctx, siblingRunner)
	if err != nil {
		t.Fatalf("read instance: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE instances SET version=version+1 WHERE id=?`, siblingRunner); err != nil {
		t.Fatalf("simulate a concurrent writer: %v", err)
	}
	record, err := store.DemandRecord(ctx, siblingScaleSet, dispatchedRequest)
	if err != nil {
		t.Fatalf("read dispatched demand: %v", err)
	}
	if _, err := store.rebindRunnerDemand(ctx, stale, record); !errors.Is(err, operations.ErrConflict) {
		t.Fatalf("stale rebind error = %v", err)
	}
	after, err := store.Instance(ctx, siblingRunner)
	if err != nil || after.Demand.JobID != boundRequest {
		t.Fatalf("a lost compare-and-set still rewrote the binding: %#v, %v", after, err)
	}
}

// TestSiblingHandoffRebindFailsClosedOnUnreadableEvidence pins the two durable
// failures the rebind can meet. Neither may guess: an unreadable released demand
// cannot clear the "did work already start here" guard, and a refused write must
// surface rather than be reported as a rebind that did not happen.
func TestSiblingHandoffRebindFailsClosedOnUnreadableEvidence(t *testing.T) {
	for _, point := range []string{"inbox.demand.lookup", "runner-demand.rebind"} {
		t.Run(point, func(t *testing.T) {
			ctx := context.Background()
			store := testStore(t)
			seedSiblingJobs(t, store)
			if err := store.CreateInstance(ctx, siblingInstance(operations.StateRunning, boundRequest)); err != nil {
				t.Fatalf("create instance: %v", err)
			}
			record, err := store.DemandRecord(ctx, siblingScaleSet, dispatchedRequest)
			if err != nil {
				t.Fatalf("read dispatched demand: %v", err)
			}
			stored, err := store.Instance(ctx, siblingRunner)
			if err != nil {
				t.Fatalf("read instance: %v", err)
			}
			store.injectFault = func(candidate string) error {
				if candidate == point {
					return errors.New("injected")
				}
				return nil
			}
			if _, err := store.rebindRunnerDemand(ctx, stored, record); err == nil {
				t.Fatalf("fault %s was ignored", point)
			}
			store.injectFault = nil
			after, err := store.Instance(ctx, siblingRunner)
			if err != nil || after.Demand.JobID != boundRequest {
				t.Fatalf("a refused rebind moved the binding: %#v, %v", after, err)
			}
		})
	}
}

// TestSiblingHandoffRebindRejectsAnUnusableDispatchedRow keeps the rebind from
// ever writing scheduling metadata the durable layer would refuse: a demand row
// without a workflow run cannot name a demand key at all.
func TestSiblingHandoffRebindRejectsAnUnusableDispatchedRow(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	instance := siblingInstance(operations.StateRunning, boundRequest)
	if err := store.CreateInstance(ctx, instance); err != nil {
		t.Fatalf("create instance: %v", err)
	}
	stored, err := store.Instance(ctx, siblingRunner)
	if err != nil {
		t.Fatalf("read instance: %v", err)
	}
	unusable := operations.DemandRecord{ScaleSetID: siblingScaleSet, RunnerRequestID: dispatchedRequest,
		Owner: "rnw-community", Repository: "rnw-community"}
	if _, err := store.rebindRunnerDemand(ctx, stored, unusable); !errors.Is(err, operations.ErrUncertain) {
		t.Fatalf("unusable dispatched row error = %v", err)
	}
	// The runner's own request re-delivered is not a rebind at all.
	own := operations.DemandRecord{ScaleSetID: siblingScaleSet, RunnerRequestID: boundRequest,
		Owner: "rnw-community", Repository: "rnw-community", WorkflowRunID: siblingRunID, RunAttempt: 1}
	if _, err := store.rebindRunnerDemand(ctx, stored, own); !errors.Is(err, operations.ErrUncertain) {
		t.Fatalf("self rebind error = %v", err)
	}
}
