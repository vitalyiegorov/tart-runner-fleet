package operations

import (
	"errors"
	"testing"
	"time"
)

// The durable layer validates the discharge itself. The operator API checks the
// same facts earlier, but the store must never depend on that: a future caller
// that skips the API cannot be allowed to write an unattributed mutation.
func TestDischargeValidationRequiresBothIdentitiesAReasonAndATime(t *testing.T) {
	at := time.Unix(9000, 0).UTC()
	for name, testCase := range map[string]struct {
		discharge Discharge
		valid     bool
	}{
		"complete":       {discharge: Discharge{OperationID: "op-1", InstanceID: "trf-1", Reason: "leak", At: at}, valid: true},
		"with reap":      {discharge: Discharge{OperationID: "op-1", InstanceID: "trf-1", ReapInstance: true, Reason: "leak", At: at}, valid: true},
		"no operation":   {discharge: Discharge{InstanceID: "trf-1", Reason: "leak", At: at}},
		"no instance":    {discharge: Discharge{OperationID: "op-1", Reason: "leak", At: at}},
		"no reason":      {discharge: Discharge{OperationID: "op-1", InstanceID: "trf-1", At: at}},
		"blank reason":   {discharge: Discharge{OperationID: "op-1", InstanceID: "trf-1", Reason: " \n\t ", At: at}},
		"no timestamp":   {discharge: Discharge{OperationID: "op-1", InstanceID: "trf-1", Reason: "leak"}},
		"nothing at all": {},
	} {
		t.Run(name, func(t *testing.T) {
			if testCase.discharge.Valid() != testCase.valid {
				t.Fatalf("Valid()=%t want %t", testCase.discharge.Valid(), testCase.valid)
			}
		})
	}
}

// Discharging is terminal but is neither success nor a dead letter. Confusing it
// with either is what would let a dependent believe a prerequisite succeeded, or
// keep a closed wedge blocking release updates forever.
func TestDischargedIsItsOwnTerminalStatus(t *testing.T) {
	for _, other := range []OperationStatus{OperationPending, OperationClaimed, OperationCompleted, OperationDead} {
		if OperationDischarged == other {
			t.Fatalf("discharged collides with %q", other)
		}
	}
	if OperationDischarged != "discharged" {
		t.Fatalf("discharged status value=%q; the durable vocabulary is a compatibility contract", OperationDischarged)
	}
}

// Every discharge refusal must remain a conflict so callers that predate the split
// keep classifying it identically.
func TestDischargeRefusalsWrapConflict(t *testing.T) {
	for _, err := range []error{ErrResourceMismatch, ErrNotDeadLettered, ErrResourceProgressing, ErrInstanceNotReapable} {
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("%v does not wrap ErrConflict", err)
		}
	}
}
