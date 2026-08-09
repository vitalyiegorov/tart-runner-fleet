package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
)

func tieredQueueStatus() adminapi.StatusEnvelope {
	var status adminapi.StatusEnvelope
	status.Data.Queues = []adminapi.Queue{{Profile: "builder", Jobs: 5, OldestAgeSeconds: 600}}
	status.Data.ScopeQueues = []adminapi.ScopeQueue{
		{Scope: "vitalyiegorov", Profile: "builder", ScaleSetID: 7, Jobs: 2, OldestAgeSeconds: 3900,
			Tiers: []adminapi.QueueTier{
				{Tier: "release", Jobs: 2, OldestAgeSeconds: 3900},
			}},
		{Scope: "budgie-at", Profile: "builder", ScaleSetID: 8, Jobs: 3, OldestAgeSeconds: 4860,
			Tiers: []adminapi.QueueTier{
				{Tier: "default", Jobs: 3, OldestAgeSeconds: 4860},
			}},
	}
	return status
}

// TestQueuesTableNamesTheTierAWaitingDemandLandedIn is the auditability
// requirement of issue #224. On 2026-08-09 the only way to see why a release was
// behind a pull request's E2E build was to read the scheduler's mind.
func TestQueuesTableNamesTheTierAWaitingDemandLandedIn(t *testing.T) {
	var buffer bytes.Buffer
	renderCommand(&buffer, "queues", tieredQueueStatus())
	out := buffer.String()

	for _, want := range []string{"TIER", "release", "default", "vitalyiegorov", "budgie-at"} {
		if !strings.Contains(out, want) {
			t.Fatalf("queues table missing %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "release") > strings.Index(out, "default") {
		t.Fatalf("tier rows are not in scope order:\n%s", out)
	}
}

// TestQueuesTableOmitsTheTierSectionWhenNoPolicyIsDeclared keeps the operator
// surface of a fleet with no tiers exactly as it was.
func TestQueuesTableOmitsTheTierSectionWhenNoPolicyIsDeclared(t *testing.T) {
	var buffer bytes.Buffer
	renderCommand(&buffer, "queues", scopeQueueStatus())
	if strings.Contains(buffer.String(), "TIER") {
		t.Fatalf("tier header rendered with no tier rows:\n%s", buffer.String())
	}
}

// TestQueuesJSONCarriesTheTierBreakdown proves an agent can read the
// classification without scraping a table.
func TestQueuesJSONCarriesTheTierBreakdown(t *testing.T) {
	encoded, err := json.Marshal(viewFor("queues", tieredQueueStatus()))
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Scopes []adminapi.ScopeQueue `json:"scopes"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Scopes) != 2 || len(decoded.Scopes[0].Tiers) != 1 || decoded.Scopes[0].Tiers[0].Tier != "release" {
		t.Fatalf("decoded scopes = %+v", decoded.Scopes)
	}
}

// TestScopeQueueJSONOmitsTiersWhenAbsent proves the field is additive: a daemon
// that publishes no breakdown produces the byte-identical document it always did.
func TestScopeQueueJSONOmitsTiersWhenAbsent(t *testing.T) {
	encoded, err := json.Marshal(adminapi.ScopeQueue{Scope: "s", Profile: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "tiers") {
		t.Fatalf("absent breakdown encoded a key: %s", encoded)
	}
}
