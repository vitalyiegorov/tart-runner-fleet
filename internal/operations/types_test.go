package operations

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

func TestInstanceSchedulingMetadataRequiresAnAtomicCompleteIdentity(t *testing.T) {
	if !(Instance{}).SchedulingMetadataValid() {
		t.Fatal("legacy instance without scheduling metadata rejected")
	}

	valid := Instance{
		Repo:      "owner/repo",
		Platform:  domain.PlatformLinux,
		Profile:   "linux-small",
		Route:     "linux-tiered-small",
		Resources: domain.Resources{CPU: 1, MemoryMB: 2048, Slots: 1},
		Demand:    domain.DemandKey{Repo: "owner/repo", RunID: 10, Attempt: 2, JobID: 30},
	}
	if !valid.SchedulingMetadataValid() {
		t.Fatal("complete scheduling identity rejected")
	}

	mutations := map[string]func(*Instance){
		"repository":      func(instance *Instance) { instance.Repo = "" },
		"platform":        func(instance *Instance) { instance.Platform = "other" },
		"profile":         func(instance *Instance) { instance.Profile = "" },
		"route":           func(instance *Instance) { instance.Route = "" },
		"cpu":             func(instance *Instance) { instance.Resources.CPU = 0 },
		"memory":          func(instance *Instance) { instance.Resources.MemoryMB = 0 },
		"slots":           func(instance *Instance) { instance.Resources.Slots = 0 },
		"demand identity": func(instance *Instance) { instance.Demand.JobID = 0 },
		"demand repo":     func(instance *Instance) { instance.Demand.Repo = "other/repo" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			instance := valid
			mutate(&instance)
			if instance.SchedulingMetadataValid() {
				t.Fatal("partial or contradictory scheduling identity accepted")
			}
		})
	}
}

func TestStateOwnershipOperationAndConfirmationValidation(t *testing.T) {
	for _, state := range []State{StatePlanned, StateCloning, StateBooting, StateReachable, StateRegistering, StateOnlineIdle, StateAssigned, StateRunning, StateDraining, StateDeregistering, StateStopping, StateDeleted, StateFailed} {
		if !ValidState(state) {
			t.Fatalf("expected valid state %q", state)
		}
	}
	if ValidState(State("unknown")) {
		t.Fatal("unknown state is valid")
	}
	ownership := Ownership{ControllerID: "controller", ResourceID: "resource", OperationID: "operation"}
	if !ownership.Valid() || (Ownership{}).Valid() {
		t.Fatal("ownership validation mismatch")
	}
	now := time.Unix(100, 0).UTC()
	op := Operation{ID: "op", IdempotencyKey: "idem", EffectKey: "effect", Kind: "clone", ResourceID: "resource", AvailableAt: now}
	if !op.Valid() {
		t.Fatal("operation should be valid")
	}
	op.ID = ""
	if op.Valid() {
		t.Fatal("operation without ID should be invalid")
	}
	op.ID = "op"
	op.DependsOn = []string{"op"}
	if op.Valid() || op.DependenciesValid() {
		t.Fatal("self dependency accepted")
	}
	op.DependsOn = []string{"root", "root"}
	if op.DependenciesValid() {
		t.Fatal("duplicate dependency accepted")
	}
	op.DependsOn = []string{""}
	if op.DependenciesValid() {
		t.Fatal("empty dependency accepted")
	}
	confirmation := DeletionConfirmation{Fresh: true, RunnerInactive: true, JobsInactive: true, ObservedAt: now}
	if !confirmation.Safe(now.Add(time.Second), time.Minute) {
		t.Fatal("fresh confirmation rejected")
	}
	confirmation.JobsInactive = false
	if confirmation.Safe(now, time.Minute) {
		t.Fatal("active jobs accepted")
	}
	confirmation.JobsInactive = true
	if confirmation.Safe(now.Add(2*time.Minute), time.Minute) || confirmation.Safe(now.Add(-time.Second), time.Minute) {
		t.Fatal("invalid confirmation time accepted")
	}
}

func TestDemandEventValidation(t *testing.T) {
	for _, kind := range []DemandEventKind{DemandJobAvailable, DemandJobAssigned, DemandJobStarted, DemandJobCompleted} {
		if !(DemandEvent{Kind: kind, RunnerRequestID: 1}).Valid() {
			t.Fatalf("valid demand event rejected: %s", kind)
		}
	}
	if (DemandEvent{Kind: "unknown", RunnerRequestID: 1}).Valid() || (DemandEvent{Kind: DemandJobAvailable}).Valid() {
		t.Fatal("invalid demand event accepted")
	}
}

// TestDemandRecordNamesOneSchedulingIdentity pins the derivation the scheduler's
// queue and any instance bound to a demand must share. They are produced in
// different packages -- app.convertDemandRecords and the durable rebind of
// ADR 0033 -- and if they ever disagreed, a demand a live instance already
// incarnates would be invisible to app.plannableDemands and admitted twice.
func TestDemandRecordNamesOneSchedulingIdentity(t *testing.T) {
	record := DemandRecord{Owner: "rnw-community", Repository: "rnw-community",
		WorkflowRunID: 30_740_997_047, RunAttempt: 2, RunnerRequestID: 8_670_852_748_984_054_370}
	if record.Repo() != "rnw-community/rnw-community" {
		t.Fatalf("repository slug = %q", record.Repo())
	}
	want := domain.DemandKey{Repo: "rnw-community/rnw-community", RunID: 30_740_997_047, Attempt: 2,
		JobID: 8_670_852_748_984_054_370}
	if got := record.DemandKey(); got != want || got.Validate() != nil {
		t.Fatalf("demand key = %#v", got)
	}
	// GitHub omits the run attempt for a first attempt, and the queue normalizes
	// it to one; the binding must normalize it identically.
	record.RunAttempt = 0
	if got := record.DemandKey(); got.Attempt != 1 {
		t.Fatalf("a missing run attempt must normalize to the first: %#v", got)
	}
	// An incomplete row cannot name an incarnation, and says so.
	if (DemandRecord{}).DemandKey().Validate() == nil {
		t.Fatal("an empty demand record produced a usable key")
	}
}

func TestCanonicalDemandObservationValidation(t *testing.T) {
	statistics := DemandStatistics{MessageID: 1}
	if !statistics.Valid() {
		t.Fatal("valid statistics rejected")
	}
	for _, mutate := range []func(*DemandStatistics){
		func(value *DemandStatistics) { value.MessageID = 0 },
		func(value *DemandStatistics) { value.Available = -1 },
		func(value *DemandStatistics) { value.Acquired = -1 },
		func(value *DemandStatistics) { value.Assigned = -1 },
		func(value *DemandStatistics) { value.Running = -1 },
		func(value *DemandStatistics) { value.Registered = -1 },
		func(value *DemandStatistics) { value.Busy = -1 },
		func(value *DemandStatistics) { value.Idle = -1 },
	} {
		candidate := statistics
		mutate(&candidate)
		if candidate.Valid() {
			t.Fatalf("invalid statistics accepted: %#v", candidate)
		}
	}
	job := GitHubJobObservation{WorkflowJobID: 1, Owner: "o", Repository: "r", WorkflowRunID: 2,
		RunAttempt: 1, DisplayName: "job", CreatedAt: time.Now()}
	if !job.Valid() {
		t.Fatal("valid GitHub job rejected")
	}
	for _, mutate := range []func(*GitHubJobObservation){
		func(value *GitHubJobObservation) { value.WorkflowJobID = 0 },
		func(value *GitHubJobObservation) { value.Owner = "" },
		func(value *GitHubJobObservation) { value.Repository = "" },
		func(value *GitHubJobObservation) { value.WorkflowRunID = 0 },
		func(value *GitHubJobObservation) { value.RunAttempt = 0 },
		func(value *GitHubJobObservation) { value.DisplayName = "" },
		func(value *GitHubJobObservation) { value.CreatedAt = time.Time{} },
	} {
		candidate := job
		mutate(&candidate)
		if candidate.Valid() {
			t.Fatalf("invalid GitHub job accepted: %#v", candidate)
		}
	}
	observed := time.Now().UTC()
	ghost := GhostDemandCriteria{ObservedAt: observed, AbsentBefore: observed.Add(-time.Hour), MinObservations: 3}
	if !ghost.Valid() {
		t.Fatal("provable ghost criteria rejected")
	}
	for _, mutate := range []func(*GhostDemandCriteria){
		func(value *GhostDemandCriteria) { value.ObservedAt = time.Time{} },
		func(value *GhostDemandCriteria) { value.AbsentBefore = time.Time{} },
		func(value *GhostDemandCriteria) { value.AbsentBefore = observed.Add(time.Second) },
		func(value *GhostDemandCriteria) { value.MinObservations = 0 },
	} {
		candidate := ghost
		mutate(&candidate)
		if candidate.Valid() {
			t.Fatalf("unprovable ghost criteria accepted: %#v", candidate)
		}
	}
}

func TestRetryPolicy(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	policy := RetryPolicy{Initial: time.Second, Maximum: 4 * time.Second, MaxAttempts: 4}
	for attempt, want := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second} {
		got, ok := policy.Next(attempt, now)
		if !ok || got.Sub(now) != want {
			t.Fatalf("attempt %d: got %v %v", attempt, got.Sub(now), ok)
		}
	}
	if _, ok := policy.Next(4, now); ok {
		t.Fatal("max attempts should stop")
	}
	got, ok := (RetryPolicy{}).Next(1, now)
	if !ok || got.Sub(now) != time.Second {
		t.Fatal("default retry delay mismatch")
	}
	got, ok = (RetryPolicy{Initial: time.Second, Maximum: 2 * time.Second, Jitter: func(int, time.Duration) time.Duration { return 5 * time.Second }}).Next(1, now)
	if !ok || got.Sub(now) != 2*time.Second {
		t.Fatal("positive jitter was not capped")
	}
	got, ok = (RetryPolicy{Initial: time.Second, Jitter: func(int, time.Duration) time.Duration { return -2 * time.Second }}).Next(1, now)
	if !ok || got != now {
		t.Fatal("negative jitter was not clamped")
	}
	got, ok = (RetryPolicy{Initial: 3 * time.Second, Maximum: 2 * time.Second}).Next(1, now)
	if !ok || got.Sub(now) != 2*time.Second {
		t.Fatal("initial retry delay was not capped")
	}
}

func TestPlanDependencyValidationAndDigest(t *testing.T) {
	now := time.Unix(123, 0).UTC()
	op := func(id string, dependencies ...string) Operation {
		return Operation{ID: id, IdempotencyKey: id, EffectKey: id, Kind: "test", ResourceID: "vm", AvailableAt: now, DependsOn: dependencies}
	}
	plan := Plan{ID: "plan", CreatedAt: now, Scheduler: SchedulerState{Version: 1}, Operations: []Operation{op("a"), op("b", "a")}}
	if !plan.Valid() {
		t.Fatal("valid dependency DAG rejected")
	}
	first, err := plan.Digest()
	if err != nil {
		t.Fatal(err)
	}
	second, _ := plan.Digest()
	if first != second {
		t.Fatal("plan digest is not deterministic")
	}
	for name, operations := range map[string][]Operation{
		"self":      {op("a", "a")},
		"duplicate": {op("a"), op("b", "a", "a")},
		"cycle":     {op("a", "b"), op("b", "a")},
		"empty":     {op("a", "")},
	} {
		t.Run(name, func(t *testing.T) {
			invalid := plan
			invalid.Operations = operations
			if invalid.Valid() {
				t.Fatal("invalid dependency graph accepted")
			}
		})
	}
	shared := plan
	shared.Operations = []Operation{op("a", "c"), op("b", "c"), op("c", "external")}
	for range 20 {
		if shared.hasDependencyCycleOrSelf() {
			t.Fatal("shared dependency reported as a cycle")
		}
	}
	duplicate := plan
	duplicate.Operations = []Operation{op("a", "external", "external")}
	if !duplicate.hasDependencyCycleOrSelf() {
		t.Fatal("duplicate dependency was not rejected by graph validation")
	}
}

func TestPlanValidationRejectsEveryInvalidIntent(t *testing.T) {
	now := time.Unix(124, 0).UTC()
	ownership := Ownership{ControllerID: "c", ResourceID: "r", OperationID: "o"}
	valid := Plan{ID: "plan", CreatedAt: now, Scheduler: SchedulerState{Version: 1}, Instances: []InstanceIntent{{ExpectedVersion: -1, Instance: Instance{ID: "vm", State: StatePlanned, Ownership: ownership}}}}
	mutations := map[string]func(*Plan){
		"id":                func(p *Plan) { p.ID = "" },
		"created":           func(p *Plan) { p.CreatedAt = time.Time{} },
		"scheduler version": func(p *Plan) { p.Scheduler.Version = 2 },
		"instance id":       func(p *Plan) { p.Instances[0].Instance.ID = "" },
		"state":             func(p *Plan) { p.Instances[0].Instance.State = State("bogus") },
		"ownership":         func(p *Plan) { p.Instances[0].Instance.Ownership = Ownership{} },
		"expected state": func(p *Plan) {
			p.Instances[0].ExpectedVersion = 0
			p.Instances[0].ExpectedState = State("bogus")
		},
		"transition": func(p *Plan) {
			p.Instances[0].ExpectedVersion = 0
			p.Instances[0].ExpectedState = StateDeleted
		},
		"operation": func(p *Plan) { p.Operations = []Operation{{}} },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			plan := valid
			plan.Instances = append([]InstanceIntent(nil), valid.Instances...)
			mutate(&plan)
			if plan.Valid() {
				t.Fatal("invalid plan accepted")
			}
		})
	}
	invalidJSON := valid
	invalidJSON.Scheduler.Data = json.RawMessage(`{`)
	if _, err := invalidJSON.Digest(); err == nil {
		t.Fatal("invalid JSON unexpectedly produced digest")
	}
}

type drainerStore struct {
	Store
	transition Transition
}

func (s *drainerStore) Transition(_ context.Context, transition Transition) (Instance, Operation, error) {
	s.transition = transition
	return Instance{ID: transition.InstanceID, State: transition.NextState, DrainPhase: transition.DrainPhase}, transition.Operation, nil
}

type confirmer struct {
	confirmation DeletionConfirmation
	err          error
}

func (c confirmer) ConfirmDeletion(context.Context, string) (DeletionConfirmation, error) {
	return c.confirmation, c.err
}

func TestDrainerRequiresTwoPhaseFreshConfirmation(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	store := &drainerStore{}
	drainer := Drainer{Store: store, Confirmer: confirmer{confirmation: DeletionConfirmation{Fresh: true, RunnerInactive: true, JobsInactive: true, ObservedAt: now}}, ConfirmationMaxAge: time.Minute, Now: func() time.Time { return now }}
	instance := Instance{ID: "vm", State: StateDraining, Version: 2, DrainPhase: 1}
	op := Operation{ID: "delete", IdempotencyKey: "delete-vm", EffectKey: "delete-vm", Kind: "delete", ResourceID: "vm"}
	got, _, err := drainer.Confirm(context.Background(), instance, op)
	if err != nil || got.State != StateDeregistering || got.DrainPhase != 2 || store.transition.ExpectedVersion != 2 {
		t.Fatalf("confirm drain: %#v %v", got, err)
	}
	instance.State = StateOnlineIdle
	if _, _, err := drainer.Confirm(context.Background(), instance, op); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
	instance.State = StateDraining
	drainer.Confirmer = confirmer{err: errors.New("api down")}
	if _, _, err := drainer.Confirm(context.Background(), instance, op); err == nil {
		t.Fatal("confirmation error ignored")
	}
	drainer.Confirmer = confirmer{confirmation: DeletionConfirmation{Fresh: false}}
	if _, _, err := drainer.Confirm(context.Background(), instance, op); !errors.Is(err, ErrUncertain) {
		t.Fatalf("expected uncertainty, got %v", err)
	}
	drainer.Now = nil
	drainer.Confirmer = confirmer{confirmation: DeletionConfirmation{Fresh: true, RunnerInactive: true, JobsInactive: true, ObservedAt: time.Now()}}
	if _, _, err := drainer.Confirm(context.Background(), instance, op); err != nil {
		t.Fatalf("default clock: %v", err)
	}
}
