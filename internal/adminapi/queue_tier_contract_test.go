package adminapi

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestScopeQueuePublishesThePriorityTierBreakdown is the API half of issue #224.
// A priority tier that cannot be read back is unauditable in exactly the moment
// it matters: an operator watching a release wait needs to see which tier the
// fleet actually put it in, not infer it from a configuration file.
func TestScopeQueuePublishesThePriorityTierBreakdown(t *testing.T) {
	encoded, err := json.Marshal(ScopeQueue{Scope: "vitalyiegorov", Profile: "builder", ScaleSetID: 7,
		Jobs: 2, OldestEnqueuedAt: time.Unix(1000, 0).UTC(), OldestAgeSeconds: 3900,
		Tiers: []QueueTier{{Tier: "release", Jobs: 2, OldestEnqueuedAt: time.Unix(1000, 0).UTC(), OldestAgeSeconds: 3900}}})
	if err != nil {
		t.Fatalf("marshal scope queue: %v", err)
	}
	document := string(encoded)
	for _, field := range []string{`"scope"`, `"profile"`, `"scaleSetId"`, `"jobs"`, `"ageSeconds"`, `"tiers"`, `"tier"`} {
		if !strings.Contains(document, field) {
			t.Fatalf("fleet.v1 scope queue omits %s: %s", field, document)
		}
	}
}

// TestScopeQueueOmitsTheBreakdownWhenNoPolicyIsDeclared keeps the field additive.
// A daemon older than issue #224 and a fleet that declares no tier publish the
// same document they always did, and a consumer must read the absent key as "no
// policy" rather than as "no demand".
func TestScopeQueueOmitsTheBreakdownWhenNoPolicyIsDeclared(t *testing.T) {
	encoded, err := json.Marshal(ScopeQueue{Scope: "vitalyiegorov", Profile: "builder", ScaleSetID: 7, Jobs: 2})
	if err != nil {
		t.Fatalf("marshal scope queue: %v", err)
	}
	if strings.Contains(string(encoded), `"tiers"`) {
		t.Fatalf("an undeclared policy published a tier key: %s", encoded)
	}
}
