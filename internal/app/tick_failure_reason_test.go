package app

import (
	"context"
	"errors"
	"fmt"
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
		ReasonQueueSummaryUnreadable, ReasonPlanCommitFailed, ReasonPlanCommitContended,
		ReasonPlanCommitRejected} {
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

// TestEngineTickSeparatesContentionFromARefusedWrite is the regression case for
// the 2026-08-01 saturation incident. Between 18:15:48Z and 18:23:58Z the
// authority daemon logged `component=scheduler reason=plan_commit_failed` once a
// minute for eight consecutive minutes — the reporter's rate limit, so the real
// rate was per tick — while `scheduler_state.version` never left 2000. One token
// covered every way a commit can fail, so the log could not say whether the fleet
// was losing a harmless compare-and-set race or being refused by the durable
// layer, and the two need opposite operator responses.
//
// ApplyPlan already returns the distinguishing sentinel. Classify by it: a
// conflict is contention that the next tick repairs by re-observing, an invalid
// plan is a rejection that repeats until inputs change, and anything else stays
// the database failure the token has always meant.
func TestEngineTickSeparatesContentionFromARefusedWrite(t *testing.T) {
	now := time.Date(2026, 8, 1, 18, 20, 0, 0, time.UTC)
	for _, testCase := range []struct {
		name      string
		applyErr  error
		reason    string
		transient bool
	}{
		{name: "optimistic concurrency loss", applyErr: operations.ErrConflict,
			reason: ReasonPlanCommitContended, transient: true},
		{name: "durable layer rejected the plan", applyErr: operations.ErrInvalid,
			reason: ReasonPlanCommitRejected},
		{name: "wrapped conflict from the drain translation", reason: ReasonPlanCommitContended,
			applyErr: fmt.Errorf("drain trf-xl in state running: %w", operations.ErrConflict), transient: true},
		{name: "store failure keeps the original token", applyErr: errors.New("database is locked"),
			reason: ReasonPlanCommitFailed},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store := &tickStore{applyErr: testCase.applyErr}
			engine := Engine{Store: store, Demand: DemandCoordinator{Store: store},
				Inventory: fakeInventory{instances: domain.Fresh([]domain.Instance(nil), now),
					host: domain.Fresh(domain.Host{Available: tickConfig().LinuxCapacity}, now)},
				Config: tickConfig(), ControllerID: "c", Mode: reconcile.Authority,
				Now: func() time.Time { return now }}
			_, err := engine.Tick(context.Background())
			if err == nil {
				t.Fatal("a refused ApplyPlan must surface as an error")
			}
			if got := failureReason(err); got != testCase.reason {
				t.Fatalf("reason = %q, want %q", got, testCase.reason)
			}
			if got := transientFailure(err); got != testCase.transient {
				t.Fatalf("transient = %v, want %v", got, testCase.transient)
			}
			// Classification is additive: every errors.Is check callers already
			// perform against the durable sentinel keeps working.
			if testCase.applyErr != nil && !errors.Is(err, testCase.applyErr) {
				t.Fatalf("classification dropped the underlying error: %v", err)
			}
		})
	}
}

// TestTickFailureTransienceIsNarrow proves only a commit conflict may claim to be
// self-healing. Every other member of the vocabulary persists until something
// outside the scheduler loop changes, so pacing a fast retry on it would spin.
func TestTickFailureTransienceIsNarrow(t *testing.T) {
	for reason := range tickReasons {
		want := reason == ReasonPlanCommitContended
		if got := (tickFailure{reason: reason, err: errors.New("x")}).Transient(); got != want {
			t.Fatalf("%s Transient() = %v, want %v", reason, got, want)
		}
	}
	if transientFailure(errors.New("unclassified")) {
		t.Fatal("an unclassified error must not claim transience")
	}
}
