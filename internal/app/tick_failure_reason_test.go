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
