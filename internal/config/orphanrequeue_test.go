package config

import (
	"testing"
	"time"
)

func TestOrphanRequeueConfiguration(t *testing.T) {
	c := OrphanRequeue{}
	if c.Validate() != nil || c.Threshold(time.Minute) != 15*time.Minute || c.Cooldown() != 30*time.Minute || c.Limit() != 3 {
		t.Fatal(c)
	}
	c = OrphanRequeue{Mode: "act", ThresholdMinutes: 20, CooldownMinutes: 40, RunsPerHour: 2, Exclusions: []OrphanExclusion{{Repository: "budgie-at/*", Workflow: "*farm*", Job: "Plan*"}}}
	if c.Validate() != nil || c.Threshold(time.Hour) != time.Hour || c.Cooldown() != 40*time.Minute || c.Limit() != 2 {
		t.Fatal(c)
	}
	if !c.Excluded("budgie-at/budgie", "ios-simulator-farm", []string{"Plan (e2e)"}) || c.Excluded("o/r", "ios-simulator-farm", []string{"Plan"}) || c.Excluded("budgie-at/budgie", "build", []string{"Plan"}) || c.Excluded("budgie-at/budgie", "ios-simulator-farm", []string{"Build"}) {
		t.Fatal("patterns")
	}
	c.Exclusions = []OrphanExclusion{{Workflow: "farm"}}
	if !c.Excluded("o/r", "farm", nil) {
		t.Fatal("empty fields must match")
	}
	for _, invalid := range []OrphanRequeue{{Mode: "invalid"}, {ThresholdMinutes: -1}, {CooldownMinutes: -1}, {RunsPerHour: -1}, {Exclusions: []OrphanExclusion{{Job: "["}}}} {
		if invalid.Validate() == nil {
			t.Fatal(invalid)
		}
	}
}

func TestOrphanRequeueRequiresAuditedInventory(t *testing.T) {
	zero := 0
	for _, g := range []GitHub{{OrphanRequeue: OrphanRequeue{Mode: "act"}}, {OrphanRequeue: OrphanRequeue{Mode: "observe"}, CanonicalJobInventory: true}, {OrphanRequeue: OrphanRequeue{Mode: "act"}, CanonicalJobInventory: true, Scopes: []GitHubScope{{Name: "scope"}}, ParkedScaleSetAuditMinutes: &zero}, {OrphanRequeue: OrphanRequeue{Mode: "invalid"}}} {
		if g.validateOrphanRequeue() == nil {
			t.Fatal(g)
		}
	}
	if err := (GitHub{OrphanRequeue: OrphanRequeue{Mode: "observe"}, CanonicalJobInventory: true, Scopes: []GitHubScope{{Name: "scope"}}}).validateOrphanRequeue(); err != nil {
		t.Fatal(err)
	}
	if (Config{GitHub: GitHub{OrphanRequeue: OrphanRequeue{Mode: "invalid"}}}).Validate() == nil {
		t.Fatal("config accepted invalid mode")
	}
}

func TestOrphanExclusionCaseAndSlash(t *testing.T) {
	c := OrphanRequeue{Exclusions: []OrphanExclusion{{Repository: "BUDGIE-AT/*", Workflow: "PULL*"}}}
	if !c.Excluded("budgie-at/budgie", "Pull Request / CI", nil) {
		t.Fatal("exclusion failed open")
	}
	g := GitHub{OrphanRequeue: OrphanRequeue{ThresholdMinutes: 1}}
	if g.validateOrphanRequeue() == nil {
		t.Fatal("threshold below audit interval accepted")
	}
}
