package discharge

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// steps records the order effects were applied in. The ordering between the
// durable row and the VM is a safety property, so it is asserted, not assumed.
type steps struct{ applied []string }

type fakeStore struct {
	outcome  operations.DischargeOutcome
	err      error
	requests []operations.Discharge
	steps    *steps
}

func (f *fakeStore) DischargeDeadLetter(_ context.Context, request operations.Discharge) (operations.DischargeOutcome, error) {
	f.requests = append(f.requests, request)
	if f.steps != nil {
		f.steps.applied = append(f.steps.applied, "durable-row-retired")
	}
	return f.outcome, f.err
}

type fakeVM struct {
	// unreadable states the reading the backend could not take. It is a separate
	// field because InstancePowerUnknown is the empty string, and this fake's
	// default has to stay "proven stopped" for every case that is about something
	// other than the VM.
	unreadable bool
	power      domain.InstancePower
	runningErr error
	reapErr    error
	reaped     []string
	steps      *steps
}

// Power defaults to a proven stop, because "the VM is off" is the ordinary state
// of every case in this file that is about something other than the VM. The
// zero InstancePower is Unknown, which since issue #252 is itself a refusal, and
// a fake that answered it everywhere would refuse every test for the wrong
// reason.
func (f *fakeVM) Power(context.Context, string) (domain.InstancePower, error) {
	if f.unreadable {
		return domain.InstancePowerUnknown, f.runningErr
	}
	if f.power == "" && f.runningErr == nil {
		return domain.InstancePowerStopped, nil
	}
	return f.power, f.runningErr
}

func (f *fakeVM) Reap(_ context.Context, name string, _ operations.Ownership) error {
	f.reaped = append(f.reaped, name)
	if f.steps != nil {
		f.steps.applied = append(f.steps.applied, "vm-deleted")
	}
	return f.reapErr
}

// request is the well-formed operator mutation replaying the 2026-07-25 phantom.
func request(reap bool) adminapi.DischargeRequest {
	return adminapi.DischargeRequest{OperationID: "op-ea9b705d234ad29f14e79b6d",
		InstanceID: "trf-maestro-096ffcb3a52d8624", ReapInstance: reap,
		Confirm: adminapi.DischargeConfirmation, Reason: "GitHub 422 runner_busy leak, no owner run exists"}
}

func service(store *fakeStore, vm *fakeVM, authority bool) (Service, *strings.Builder) {
	audit := &strings.Builder{}
	return Service{Store: store, VM: vm, Authority: authority, Now: func() time.Time { return time.Unix(9000, 0).UTC() },
		Logger: slog.New(slog.NewTextHandler(audit, &slog.HandlerOptions{Level: slog.LevelWarn}))}, audit
}

func refusalCode(t *testing.T, err error) string {
	t.Helper()
	var refusal adminapi.Refusal
	if !errors.As(err, &refusal) || !errors.Is(err, adminapi.ErrRefused) {
		t.Fatalf("error=%v is not a bounded refusal", err)
	}
	if !adminapi.ValidRefusalCode(refusal.Code) {
		t.Fatalf("refusal code %q is outside the closed vocabulary", refusal.Code)
	}
	return refusal.Code
}

// The complete remedy, in the only safe order: the durable row is retired first
// and the VM removed second. Reversing them would leave a live row owning an
// absent VM, which turns the entire instance observation Unavailable and blocks
// planning host-wide with no VM left to prove anything.
func TestDischargeRetiresTheDurableRowBeforeRemovingTheVM(t *testing.T) {
	order := &steps{}
	store := &fakeStore{steps: order, outcome: operations.DischargeOutcome{OperationDischarged: true, InstanceReaped: true,
		Ownership: operations.Ownership{ControllerID: "tart-runner-fleet", ResourceID: "trf-maestro-096ffcb3a52d8624", OperationID: "op-provision"}}}
	vm := &fakeVM{steps: order}
	subject, audit := service(store, vm, true)
	result, err := subject.DischargeDeadLetter(context.Background(), request(true))
	if err != nil {
		t.Fatal(err)
	}
	if !result.OperationDischarged || !result.InstanceReaped || !result.VMDeleted {
		t.Fatalf("result=%#v", result)
	}
	if result.APIVersion != adminapi.APIVersion || result.Kind != adminapi.DischargeKind {
		t.Fatalf("result envelope=%#v", result)
	}
	if len(store.requests) != 1 || !store.requests[0].ReapInstance || store.requests[0].Reason == "" {
		t.Fatalf("durable requests=%#v", store.requests)
	}
	if len(vm.reaped) != 1 || vm.reaped[0] != "trf-maestro-096ffcb3a52d8624" {
		t.Fatalf("reaped=%#v", vm.reaped)
	}
	// Reversing these two steps leaves a live row owning an absent VM, which is the
	// unrepairable half of the wedge.
	if len(order.applied) != 2 || order.applied[0] != "durable-row-retired" || order.applied[1] != "vm-deleted" {
		t.Fatalf("effect order=%v; the durable row must be retired before the VM is removed", order.applied)
	}
	record := audit.String()
	for _, fragment := range []string{"operator discharge", "outcome=applied", "op-ea9b705d234ad29f14e79b6d",
		"trf-maestro-096ffcb3a52d8624", "reap=true", "runner_busy"} {
		if !strings.Contains(record, fragment) {
			t.Fatalf("audit record %q omits %q", record, fragment)
		}
	}
}

// Discharging the operation alone never touches the host.
func TestDischargeWithoutReapNeverTouchesTart(t *testing.T) {
	store := &fakeStore{outcome: operations.DischargeOutcome{OperationDischarged: true}}
	vm := &fakeVM{power: domain.InstancePowerRunning}
	subject, _ := service(store, vm, false)
	subject.Authority = true
	result, err := subject.DischargeDeadLetter(context.Background(), request(false))
	if err != nil || result.VMDeleted || len(vm.reaped) != 0 {
		t.Fatalf("result=%#v reaped=%#v err=%v", result, vm.reaped, err)
	}
}

// A failed VM removal reports the durable half that DID apply, so the operator
// knows the row is already retired and the correct action is to retry — not to
// reach for `tart delete` or the database.
func TestDischargeReportsThePartiallyAppliedRemedyWhenTheVMSurvives(t *testing.T) {
	store := &fakeStore{outcome: operations.DischargeOutcome{OperationDischarged: true, InstanceReaped: true}}
	vm := &fakeVM{reapErr: errors.New("tart delete failed")}
	subject, audit := service(store, vm, true)
	result, err := subject.DischargeDeadLetter(context.Background(), request(true))
	if code := refusalCode(t, err); code != adminapi.RefusalVMDeleteFailed {
		t.Fatalf("refusal=%s", code)
	}
	if !result.OperationDischarged || !result.InstanceReaped || result.VMDeleted {
		t.Fatalf("partial result=%#v must still report the applied durable half", result)
	}
	if !strings.Contains(audit.String(), adminapi.RefusalVMDeleteFailed) {
		t.Fatalf("audit record %q omits the refusal", audit.String())
	}
}

func TestDischargeRefusesEveryUnsafeMutation(t *testing.T) {
	for name, testCase := range map[string]struct {
		authority bool
		request   adminapi.DischargeRequest
		store     *fakeStore
		vm        *fakeVM
		want      string
	}{
		"observe mode": {authority: false, request: request(true), store: &fakeStore{}, vm: &fakeVM{},
			want: adminapi.RefusalNotAuthority},
		"missing confirmation": {authority: true, store: &fakeStore{}, vm: &fakeVM{},
			request: adminapi.DischargeRequest{OperationID: "op-1", InstanceID: "trf-1", Reason: "leak"},
			want:    adminapi.RefusalUnconfirmed},
		"blank reason": {authority: true, store: &fakeStore{}, vm: &fakeVM{},
			request: adminapi.DischargeRequest{OperationID: "op-1", InstanceID: "trf-1",
				Confirm: adminapi.DischargeConfirmation, Reason: "   "},
			want: adminapi.RefusalReasonRequired},
		"unbounded reason": {authority: true, store: &fakeStore{}, vm: &fakeVM{},
			request: adminapi.DischargeRequest{OperationID: "op-1", InstanceID: "trf-1",
				Confirm: adminapi.DischargeConfirmation, Reason: strings.Repeat("x", adminapi.MaxReasonBytes+1)},
			want: adminapi.RefusalReasonRequired},
		"missing identities": {authority: true, store: &fakeStore{}, vm: &fakeVM{},
			request: adminapi.DischargeRequest{Confirm: adminapi.DischargeConfirmation, Reason: "leak"},
			want:    adminapi.RefusalInvalidRequest},
		// A power the backend could not read is refused as an unobserved VM, not
		// waved through as a stopped one (issue #252). The operator's authority
		// covers a leaked registration; it does not cover a machine nobody read.
		"vm power unreadable": {authority: true, request: request(true), store: &fakeStore{},
			vm: &fakeVM{unreadable: true}, want: adminapi.RefusalVMUnobserved},
		"vm is running": {authority: true, request: request(true), store: &fakeStore{}, vm: &fakeVM{power: domain.InstancePowerRunning},
			want: adminapi.RefusalVMRunning},
		"tart unreadable": {authority: true, request: request(true), store: &fakeStore{},
			vm: &fakeVM{runningErr: errors.New("tart unavailable")}, want: adminapi.RefusalVMUnobserved},
		"unknown operation": {authority: true, request: request(true), vm: &fakeVM{},
			store: &fakeStore{err: operations.ErrNotFound}, want: adminapi.RefusalUnknownOperation},
		"operation belongs elsewhere": {authority: true, request: request(true), vm: &fakeVM{},
			store: &fakeStore{err: operations.ErrResourceMismatch}, want: adminapi.RefusalResourceMismatch},
		"operation still retrying": {authority: true, request: request(true), vm: &fakeVM{},
			store: &fakeStore{err: operations.ErrNotDeadLettered}, want: adminapi.RefusalOperationLive},
		"resource still progressing": {authority: true, request: request(true), vm: &fakeVM{},
			store: &fakeStore{err: operations.ErrResourceProgressing}, want: adminapi.RefusalResourceBusy},
		"instance is live": {authority: true, request: request(true), vm: &fakeVM{},
			store: &fakeStore{err: operations.ErrInstanceNotReapable}, want: adminapi.RefusalInstanceState},
		"durable request rejected": {authority: true, request: request(true), vm: &fakeVM{},
			store: &fakeStore{err: operations.ErrInvalid}, want: adminapi.RefusalInvalidRequest},
		"unclassified conflict": {authority: true, request: request(true), vm: &fakeVM{},
			store: &fakeStore{err: operations.ErrConflict}, want: adminapi.RefusalResourceBusy},
		"store unavailable": {authority: true, request: request(true), vm: &fakeVM{},
			store: &fakeStore{err: errors.New("database is locked")}, want: adminapi.RefusalStoreUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			subject, audit := service(testCase.store, testCase.vm, testCase.authority)
			result, err := subject.DischargeDeadLetter(context.Background(), testCase.request)
			if code := refusalCode(t, err); code != testCase.want {
				t.Fatalf("refusal=%s want %s", code, testCase.want)
			}
			if result != (adminapi.DischargeResult{}) {
				t.Fatalf("refused mutation returned a result: %#v", result)
			}
			if len(testCase.vm.reaped) != 0 {
				t.Fatalf("refused mutation removed a VM: %#v", testCase.vm.reaped)
			}
			if !strings.Contains(audit.String(), "outcome=refused") {
				t.Fatalf("refusal was not audited: %q", audit.String())
			}
		})
	}
}

// The store must never see a request that failed a guard, so no durable write can
// happen behind a refusal.
func TestRefusedMutationsNeverReachTheStore(t *testing.T) {
	store := &fakeStore{}
	subject, _ := service(store, &fakeVM{power: domain.InstancePowerRunning}, true)
	if _, err := subject.DischargeDeadLetter(context.Background(), request(true)); err == nil {
		t.Fatal("running VM mutation accepted")
	}
	if len(store.requests) != 0 {
		t.Fatalf("refused mutation reached the store: %#v", store.requests)
	}
}

// A service without a logger must still work: the audit sink is optional wiring,
// never a precondition for a safety decision.
func TestDischargeWorksWithoutAnAuditSink(t *testing.T) {
	store := &fakeStore{outcome: operations.DischargeOutcome{OperationDischarged: true}}
	subject := Service{Store: store, VM: &fakeVM{}, Authority: true}
	result, err := subject.DischargeDeadLetter(context.Background(), request(false))
	if err != nil || !result.OperationDischarged {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if store.requests[0].At.IsZero() {
		t.Fatal("a discharge without an injected clock must still stamp a time")
	}
}
