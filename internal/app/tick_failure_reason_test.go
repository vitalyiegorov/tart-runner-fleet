package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/reconcile"
)

// The scheduler loop reports a failure through OnFailure, whose reason comes
// from failureReason(err): an error must implement FailureReason to say anything
// at all. Engine.Tick returned bare store, demand, and commit errors, so every
// scheduler failure logged `component=scheduler` with no reason.
//
// Measured on the production host over seven days: 271 rate-limited scheduler
// warnings, every one of them reasonless, against zero reasonless ingest
// warnings once the ingest path classified its own failures. The rate limiter
// keys on component and reason together, so a reasonless component also collapses
// every distinct cause into one suppressed bucket per minute. The daemon does
// record the plan reason into transient health detail, but that is gone the moment
// the tick recovers: `observations` reports fresh and `health` retains nothing.
// An operator therefore cannot tell a corrupt scheduler row from a dead database
// from a broker outage.

func reasonEngine(store EngineStore, demand DemandCoordinator, bindings []Binding, now time.Time) Engine {
	inventory := fakeInventory{instances: domain.Fresh([]domain.Instance(nil), now),
		host: domain.Fresh(domain.Host{Available: tickConfig().LinuxCapacity,
			Pressure: domain.HostPressure{FreeDiskGB: 200, AdmissionAllowed: true}}, now)}
	return Engine{Store: store, Demand: demand, Inventory: inventory, Config: tickConfig(),
		Bindings: bindings, ControllerID: "controller", Mode: reconcile.Authority,
		Now: func() time.Time { return now }}
}

// TestEngineTickFailuresCarryABoundedReason is the RED-first case: every error
// Engine.Tick can return must name its cause in the closed vocabulary the
// failure hook publishes, so the scheduler warning is diagnosable.
func TestEngineTickFailuresCarryABoundedReason(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	down := errors.New("down")
	binding := []Binding{{ScaleSetID: 1, Profile: tickConfig().Profiles["small"]}}

	tests := []struct {
		name   string
		engine Engine
		want   string
	}{
		{
			name:   "invalid engine wiring",
			engine: Engine{Inventory: fakeInventory{}, Config: tickConfig(), ControllerID: "c", Mode: reconcile.Observe},
			want:   ReasonEngineInvalid,
		},
		{
			name: "unreadable scheduler state",
			engine: reasonEngine(&tickStore{stateErr: down}, DemandCoordinator{Store: &tickStore{}},
				nil, now),
			want: ReasonSchedulerStateUnreadable,
		},
		{
			name: "scheduler state reseed refused",
			engine: reasonEngine(&tickStore{stateErr: operations.ErrSchedulerStateMissing, reseedErr: down},
				DemandCoordinator{Store: &tickStore{}}, nil, now),
			want: ReasonSchedulerStateReseedFailed,
		},
		{
			name: "corrupt scheduler state",
			engine: reasonEngine(&tickStore{state: operations.SchedulerState{Data: []byte("{")}},
				DemandCoordinator{Store: &tickStore{}}, nil, now),
			want: ReasonSchedulerStateCorrupt,
		},
		{
			name: "unreadable durable demand",
			engine: func() Engine {
				store := &tickStore{fakeDemandStore: fakeDemandStore{err: down}}
				return reasonEngine(store, DemandCoordinator{Store: store}, binding, now)
			}(),
			want: ReasonDemandUnreadable,
		},
		{
			name: "unreadable queue summary",
			engine: reasonEngine(&tickStore{},
				DemandCoordinator{Store: queueSummaryErrorStore{fakeDemandStore: &fakeDemandStore{}}}, binding, now),
			want: ReasonQueueSummaryUnreadable,
		},
		{
			name: "plan commit refused",
			engine: func() Engine {
				store := &tickStore{applyErr: down}
				return reasonEngine(store, DemandCoordinator{Store: store}, nil, now)
			}(),
			want: ReasonPlanCommitFailed,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := test.engine.Tick(context.Background())
			if err == nil {
				t.Fatal("Tick() unexpectedly succeeded; the failure case is not being exercised")
			}
			if got := failureReason(err); got != test.want {
				t.Fatalf("failureReason(err) = %q, want %q (err=%v)", got, test.want, err)
			}
		})
	}
}

// TestEngineTickFailuresRemainUnwrappable proves classification is additive:
// wrapping must not break the errors.Is checks callers and existing tests rely
// on, so sentinel identity survives alongside the new reason.
func TestEngineTickFailuresRemainUnwrappable(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	down := errors.New("down")

	invalid := Engine{Inventory: fakeInventory{}, Config: tickConfig(), ControllerID: "c", Mode: reconcile.Observe}
	if _, err := invalid.Tick(context.Background()); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("invalid wiring lost ErrInvalid identity: %v", err)
	}

	reseed := reasonEngine(&tickStore{stateErr: operations.ErrSchedulerStateMissing, reseedErr: down},
		DemandCoordinator{Store: &tickStore{}}, nil, now)
	if _, err := reseed.Tick(context.Background()); !errors.Is(err, down) {
		t.Fatalf("reseed failure lost the underlying cause: %v", err)
	}
}

// TestTickFailureReasonVocabularyIsClosed proves the reason never carries
// upstream text. A token outside the closed set is withheld rather than echoed,
// so no store error string, credential, or JIT payload can reach the logs
// through this path.
func TestTickFailureReasonVocabularyIsClosed(t *testing.T) {
	leaky := tickFailure{reason: "ghs_secret_token_leaked_here", err: errors.New("boom")}
	if got := leaky.FailureReason(); got != "" {
		t.Fatalf("FailureReason() = %q, want \"\" for a token outside the closed vocabulary", got)
	}
	for _, reason := range []string{ReasonEngineInvalid, ReasonSchedulerStateUnreadable,
		ReasonSchedulerStateReseedFailed, ReasonSchedulerStateCorrupt, ReasonDemandUnreadable,
		ReasonQueueSummaryUnreadable, ReasonPlanCommitFailed} {
		if got := (tickFailure{reason: reason, err: errors.New("x")}).FailureReason(); got != reason {
			t.Fatalf("FailureReason() withheld a vocabulary member %q, got %q", reason, got)
		}
	}
}

// TestEngineTickSeparatesAnInvalidPlanFromARefusedWrite proves the reason
// distinguishes two situations Commit reports through one error path. A plan the
// scheduler could not form (an instance platform it does not recognize, which
// surfaces as PlanInvalidObservation) is a domain problem needing inventory
// repair. A refused durable write is a database problem. Reporting both as
// plan_commit_failed sends an operator to the wrong subsystem.
//
// Observed in production on 2026-07-29: every classified scheduler warning read
// `reason=plan_commit_failed`, roughly one to three per hour, with no way to tell
// which of the two it was.
func TestEngineTickSeparatesAnInvalidPlanFromARefusedWrite(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	// A live instance with an unrecognized platform makes the plan invalid; the
	// durable store is healthy.
	unknown := domain.Instance{ID: "trf-weird", Repo: "a/repo", Platform: domain.Platform("plan9"),
		Profile: "small", Route: "tiered", Resources: domain.Resources{CPU: 1, MemoryMB: 1024, Slots: 1},
		State: domain.InstanceRunning, Power: domain.InstancePowerRunning}
	invalidStore := &tickStore{}
	invalidPlan := Engine{Store: invalidStore, Demand: DemandCoordinator{Store: invalidStore},
		Inventory: fakeInventory{instances: domain.Fresh([]domain.Instance{unknown}, now),
			host: domain.Fresh(domain.Host{Available: tickConfig().LinuxCapacity}, now)},
		Config: tickConfig(), ControllerID: "c", Mode: reconcile.Authority,
		Now: func() time.Time { return now }}
	_, err := invalidPlan.Tick(context.Background())
	if err == nil {
		t.Fatal("an unrecognized instance platform must not produce a committed plan")
	}
	if got := failureReason(err); got != ReasonPlanInvalid {
		t.Fatalf("invalid plan reason = %q, want %q", got, ReasonPlanInvalid)
	}

	// A healthy plan whose durable write is refused keeps the commit reason.
	writeStore := &tickStore{applyErr: errors.New("database is locked")}
	refusedWrite := Engine{Store: writeStore, Demand: DemandCoordinator{Store: writeStore},
		Inventory: fakeInventory{instances: domain.Fresh([]domain.Instance(nil), now),
			host: domain.Fresh(domain.Host{Available: tickConfig().LinuxCapacity}, now)},
		Config: tickConfig(), ControllerID: "c", Mode: reconcile.Authority,
		Now: func() time.Time { return now }}
	_, err = refusedWrite.Tick(context.Background())
	if err == nil {
		t.Fatal("a refused ApplyPlan must surface as an error")
	}
	if got := failureReason(err); got != ReasonPlanCommitFailed {
		t.Fatalf("refused write reason = %q, want %q", got, ReasonPlanCommitFailed)
	}
}

// TestEngineTickSeparatesASupersededPlanFromARefusedWrite proves a lost
// optimistic race is named as what it is. ApplyPlan guards every instance write
// with WHERE version=? and the scheduler row with an expected version; a
// lifecycle worker advancing an instance between the tick's snapshot and its
// commit makes those guards match zero rows, ApplyPlan returns ErrConflict, and
// the next tick re-plans from fresh state. That is snapshot concurrency working
// as designed -- at-least-once, idempotent -- yet it was reported as
// plan_commit_failed, indistinguishable from a genuinely refused write.
//
// Observed on the production host: ~45 plan_commit_failed warnings per day,
// every one unexplained, clustering with VM lifecycle churn. Once this reason
// deploys, production itself settles the hypothesis: conflicts that rename
// themselves plan_commit_superseded are benign; anything still reporting
// plan_commit_failed is a real store fault worth chasing.
func TestEngineTickSeparatesASupersededPlanFromARefusedWrite(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	superseded := reasonEngine(&tickStore{applyErr: operations.ErrConflict},
		DemandCoordinator{Store: &tickStore{}}, nil, now)
	_, err := superseded.Tick(context.Background())
	if err == nil {
		t.Fatal("a conflicted ApplyPlan must surface as an error so the tick retries")
	}
	if got := failureReason(err); got != ReasonPlanCommitSuperseded {
		t.Fatalf("conflict reason = %q, want %q", got, ReasonPlanCommitSuperseded)
	}
	if !errors.Is(err, operations.ErrConflict) {
		t.Fatalf("classification lost the ErrConflict identity: %v", err)
	}

	refused := reasonEngine(&tickStore{applyErr: errors.New("disk I/O error")},
		DemandCoordinator{Store: &tickStore{}}, nil, now)
	_, err = refused.Tick(context.Background())
	if got := failureReason(err); got != ReasonPlanCommitFailed {
		t.Fatalf("genuine write failure reason = %q, want %q", got, ReasonPlanCommitFailed)
	}
}
