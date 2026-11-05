package lifecycle

import (
	"context"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

type refusalReaderFake struct{ reads int }

func (r *refusalReaderFake) ReadRunnerRefusal(context.Context, operations.Instance) { r.reads++ }

// The read shares the traffic sample's rule: first stop only, and never against a
// guest already proved unresponsive, which could not answer.
func TestTheRefusalIsReadOnceAndNeverFromAnUnresponsiveGuest(t *testing.T) {
	for name, test := range map[string]struct {
		phase    int
		attempts int
		reads    int
	}{
		"first stop":         {reads: 1},
		"escalated stop":     {attempts: 1},
		"unresponsive guest": {phase: operations.DrainPhaseGuestUnresponsive},
	} {
		t.Run(name, func(t *testing.T) {
			calls := []string{}
			reader := &refusalReaderFake{}
			executor := DrainExecutor{VM: fakeVM{calls: &calls}, Refusal: reader}
			instance := lifecycleInstance(operations.StateDeregistering)
			instance.DrainPhase = test.phase
			operation := operations.Operation{Kind: OperationDrain, ResourceID: instance.ID}
			if test.attempts > 0 {
				operation.LastError = stageError{stage: StageStop, stopAttempts: test.attempts}.Error()
			}
			_ = executor.stopGuest(context.Background(), instance, operation)
			if reader.reads != test.reads {
				t.Fatalf("reads = %d, want %d", reader.reads, test.reads)
			}
		})
	}
}
