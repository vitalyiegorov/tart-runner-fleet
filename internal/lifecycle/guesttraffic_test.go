package lifecycle

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

type recordingTraffic struct{ calls *[]string }

func (r recordingTraffic) RecordGuestTraffic(_ context.Context, instance operations.Instance) {
	*r.calls = append(*r.calls, "traffic:"+instance.ID)
}

// TestDrainSamplesGuestTrafficAfterDeregisterBeforeStop pins ADR 0080's one
// sample: after the runner is deregistered and its deletion confirmed — the job
// is over — and before the guest is stopped, which is the last moment its
// counters exist.
func TestDrainSamplesGuestTrafficAfterDeregisterBeforeStop(t *testing.T) {
	now := time.Unix(1000, 0).UTC()
	calls := []string{}
	safe := operations.DeletionConfirmation{Fresh: true, RunnerInactive: true, JobsInactive: true, ObservedAt: now}
	control := &fakeDrainControl{calls: &calls, safe: true, confirmations: []operations.DeletionConfirmation{safe, safe}}
	state := &memoryState{instance: lifecycleInstance(operations.StateDraining)}
	executor := DrainExecutor{State: state, VM: fakeVM{calls: &calls}, Control: control, Traffic: recordingTraffic{&calls},
		Now: func() time.Time { return now }, ConfirmationMaxAge: time.Minute}
	if err := executor.Execute(context.Background(), operations.Operation{Kind: OperationDrain, ResourceID: state.instance.ID}); err != nil {
		t.Fatal(err)
	}
	want := []string{"guard:trf-small-1", "deregister:trf-small-1", "confirm:trf-small-1", "traffic:trf-small-1",
		"stop:trf-small-1", "confirm:trf-small-1", "delete:trf-small-1"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls=%#v", calls)
	}
}

// TestDrainSamplesOnlyOnTheFirstStopAndNeverADeadGuest keeps sampling off the
// escalation ladder — a guest that would not stop is not asked again — and off
// a guest the fleet has proved unresponsive, whose exec could only time out.
func TestDrainSamplesOnlyOnTheFirstStopAndNeverADeadGuest(t *testing.T) {
	for name, testCase := range map[string]struct {
		phase    int
		attempts int
		want     bool
	}{
		"first graceful stop":        {want: true},
		"occupancy budget":           {phase: operations.DrainPhaseOccupancyBudget, want: true},
		"escalated stop":             {attempts: 1},
		"unresponsive guest reclaim": {phase: operations.DrainPhaseGuestUnresponsive},
	} {
		t.Run(name, func(t *testing.T) {
			calls := []string{}
			instance := lifecycleInstance(operations.StateDeregistering)
			instance.DrainPhase = testCase.phase
			executor := DrainExecutor{VM: fakeVM{calls: &calls}, Traffic: recordingTraffic{&calls}}
			operation := operations.Operation{Kind: OperationDrain, ResourceID: instance.ID}
			if testCase.attempts > 0 {
				operation.LastError = stageError{stage: StageStop, stopAttempts: testCase.attempts}.Error()
			}
			_ = executor.stopGuest(context.Background(), instance, operation)
			if sampled := len(calls) > 0 && calls[0] == "traffic:trf-small-1"; sampled != testCase.want {
				t.Fatalf("sampled=%t calls=%#v", sampled, calls)
			}
		})
	}
}
