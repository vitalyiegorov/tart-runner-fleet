package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
)

func scopeQueueStatus() adminapi.StatusEnvelope {
	var status adminapi.StatusEnvelope
	status.Data.Queues = []adminapi.Queue{{Profile: "builder", Jobs: 5, OldestAgeSeconds: 600}}
	status.Data.ScopeQueues = []adminapi.ScopeQueue{
		{Scope: "budgie-org", Profile: "builder", ScaleSetID: 1, Jobs: 4,
			OldestEnqueuedAt: time.Unix(1000, 0).UTC(), OldestAgeSeconds: 36000},
		{Scope: "sudoku-repo", Profile: "builder", ScaleSetID: 1, Jobs: 1, OldestAgeSeconds: 30},
	}
	return status
}

// TestQueuesTableShowsWhoseDemandItIs proves the table names the scope. The
// aggregate row "builder 5" cannot tell an operator that four of those jobs
// belong to a scope with no runners, which is what a stalled queue looks like.
func TestQueuesTableShowsWhoseDemandItIs(t *testing.T) {
	var buffer bytes.Buffer
	renderCommand(&buffer, "queues", scopeQueueStatus())
	out := buffer.String()

	for _, want := range []string{"SCOPE", "budgie-org", "sudoku-repo", "SCALE SET"} {
		if !strings.Contains(out, want) {
			t.Fatalf("queues table missing %q:\n%s", want, out)
		}
	}
	// The aggregate must still be rendered alongside it.
	if !strings.Contains(out, "PROFILE") || !strings.Contains(out, "builder") {
		t.Fatalf("aggregate rows lost:\n%s", out)
	}
}

// TestQueuesTableOmitsScopeSectionWhenAbsent proves a daemon that publishes no
// breakdown (an older generation) renders exactly as before, with no empty
// header implying missing data.
func TestQueuesTableOmitsScopeSectionWhenAbsent(t *testing.T) {
	var status adminapi.StatusEnvelope
	status.Data.Queues = []adminapi.Queue{{Profile: "small", Jobs: 1}}
	var buffer bytes.Buffer
	renderCommand(&buffer, "queues", status)
	if strings.Contains(buffer.String(), "SCOPE") {
		t.Fatalf("scope header rendered with no scope rows:\n%s", buffer.String())
	}
}

// TestQueuesJSONCarriesBothViews proves the JSON view exposes the breakdown for
// scripts and agents, keeping the per-profile aggregate available.
func TestQueuesJSONCarriesBothViews(t *testing.T) {
	encoded, err := json.Marshal(viewFor("queues", scopeQueueStatus()))
	if err != nil {
		t.Fatalf("Marshal() = %v", err)
	}
	var decoded struct {
		Profiles []adminapi.Queue      `json:"profiles"`
		Scopes   []adminapi.ScopeQueue `json:"scopes"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("Unmarshal() = %v", err)
	}
	if len(decoded.Profiles) != 1 || len(decoded.Scopes) != 2 {
		t.Fatalf("decoded = %+v", decoded)
	}
	if decoded.Scopes[0].Scope != "budgie-org" || decoded.Scopes[0].Jobs != 4 {
		t.Fatalf("scope row = %+v", decoded.Scopes[0])
	}
}

// TestQueuesJSONStaysBackwardCompatible proves that without a breakdown the JSON
// view remains the bare array older consumers parse.
func TestQueuesJSONStaysBackwardCompatible(t *testing.T) {
	var status adminapi.StatusEnvelope
	status.Data.Queues = []adminapi.Queue{{Profile: "small", Jobs: 2}}
	encoded, err := json.Marshal(viewFor("queues", status))
	if err != nil {
		t.Fatalf("Marshal() = %v", err)
	}
	var rows []adminapi.Queue
	if err := json.Unmarshal(encoded, &rows); err != nil {
		t.Fatalf("legacy consumers can no longer parse queues: %v (%s)", err, encoded)
	}
	if len(rows) != 1 || rows[0].Profile != "small" {
		t.Fatalf("rows = %+v", rows)
	}
}
