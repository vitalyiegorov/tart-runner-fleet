package lifecycle

import (
	"context"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// knee-doctor#1352: fourteen E2E jobs on Tart guests ended with "The runner has
// received a shutdown signal" and the controller log named none of them, so a
// fleet stop could not be told from one the fleet never made. Every stop rung
// that succeeds is now reported with the phase that decided the drain and the
// rung that ended the guest; a rung that fails reports nothing, because it
// stopped nothing.
func TestDrainReportsEveryGuestItStops(t *testing.T) {
	for _, test := range []struct {
		name     string
		phase    int
		attempts int
		wantRung string
	}{
		{"job completed", operations.DrainPhaseEvent, 0, "graceful"},
		{"idle runner reclaimed", operations.DrainPhaseLingeringRunner, 0, "graceful"},
		{"escalated stop", operations.DrainPhaseEvent, GracefulStopAttempts, "forced"},
	} {
		t.Run(test.name, func(t *testing.T) {
			executor, state, _, _ := drainFixture(operations.StateDeregistering)
			state.instance.DrainPhase = test.phase
			var reported []GuestStop
			executor.OnStop = func(stop GuestStop) { reported = append(reported, stop) }
			operation := stopFailure()
			operation.LastError = (stageError{stage: StageStop, stopAttempts: test.attempts}).Error()
			if err := executor.Execute(context.Background(), operation); err != nil {
				t.Fatal(err)
			}
			if len(reported) != 1 {
				t.Fatalf("reported %d stops, want 1: %#v", len(reported), reported)
			}
			stop := reported[0]
			if stop.Instance.ID != "trf-small-1" || stop.Phase != DrainPhaseName(test.phase) || stop.Rung != test.wantRung {
				t.Fatalf("stop = %#v, want instance trf-small-1, phase %s, rung %s", stop, DrainPhaseName(test.phase), test.wantRung)
			}
		})
	}
}

func TestDrainReportsNoStopWhenTheStopFails(t *testing.T) {
	executor, _, vm, _ := drainFixture(operations.StateDeregistering)
	vm.stopErr, vm.running = errWedgedGuest, true
	executor.OnStop = func(stop GuestStop) { t.Fatalf("a failed stop was reported: %#v", stop) }
	if err := executor.Execute(context.Background(), stopFailure()); err == nil {
		t.Fatal("a wedged guest's stop succeeded")
	}
}
