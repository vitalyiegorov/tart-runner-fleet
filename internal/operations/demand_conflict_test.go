package operations

import (
	"strings"
	"testing"
)

func TestDemandConflictBoundsIdentifiers(t *testing.T) {
	detail := &DemandConflict{Check: ConflictReservationDuplicate, JobID: strings.Repeat("j", 300), Other: strings.Repeat("o", 300)}
	line := detail.Error()
	if !strings.Contains(line, "reservation_duplicate") || strings.Contains(line, strings.Repeat("j", 257)) || strings.Contains(line, strings.Repeat("o", 257)) {
		t.Fatal(line)
	}
}

func TestDemandConflictCodes(t *testing.T) {
	codes := []DemandConflictCode{ConflictDemandJobIdentity, ConflictReservationDuplicate, ConflictBindingShapeMismatch, ConflictBindingMetadataInvalid, ConflictBindingSourceCAS, ConflictBindingTargetCAS, ConflictBindingRebindCAS, ConflictInstanceStateTransition, ConflictTransitionInstanceCAS, ConflictTransitionIdempotentState}
	seen := map[DemandConflictCode]bool{}
	for _, code := range codes {
		if code == "" || seen[code] || !strings.Contains((&DemandConflict{Check: code}).Error(), string(code)) {
			t.Fatalf("invalid code %q", code)
		}
		seen[code] = true
	}
}

func TestDemandConflictErrorCarriesSafeIdentifiers(t *testing.T) {
	detail := &DemandConflict{Check: "instance_state_transition", MessageType: DemandJobStarted, RequestID: 101, JobID: "job-uuid", Runner: "runner-beta", Instance: "runner-alpha", From: StateAssigned, To: StateRunning}
	for _, want := range []string{"instance_state_transition", "JobStarted", "101", "job-uuid", "runner-beta", "runner-alpha", "assigned", "running"} {
		if !strings.Contains(detail.Error(), want) {
			t.Fatalf("missing %s: %v", want, detail)
		}
	}
}
