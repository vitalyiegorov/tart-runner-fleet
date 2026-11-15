package operations

import (
	"strings"
	"testing"
)

func TestDemandConflictBoundsIdentifiersAndCheckVocabulary(t *testing.T) {
	detail := &DemandConflict{Check: "untrusted-check", JobID: strings.Repeat("j", 300)}
	line := detail.Error()
	if strings.Contains(line, "untrusted-check") || !strings.Contains(line, "unknown") || strings.Contains(line, strings.Repeat("j", 257)) {
		t.Fatal(line)
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
