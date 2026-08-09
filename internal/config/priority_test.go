package config

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

func priorityConfig(priority Priority) Config {
	cfg := Default()
	cfg.Priority = priority
	return cfg
}

func releaseTier() Priority {
	return Priority{EscalateAfter: 30 * time.Minute, Tiers: []domain.PriorityTier{
		{Name: "release", Match: []domain.PriorityMatch{{WorkflowRef: "*/.github/workflows/release*.yml@*"}}},
	}}
}

func TestPriorityRoundTripsThroughTheWireFormat(t *testing.T) {
	cfg := priorityConfig(releaseTier())
	var buffer bytes.Buffer
	if err := Encode(&buffer, cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buffer.String(), `"escalateAfterSeconds": 1800`) {
		t.Fatalf("encoded config does not carry the escalation threshold:\n%s", buffer.String())
	}
	decoded, err := Decode(&buffer)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Priority.EscalateAfter != 30*time.Minute || len(decoded.Priority.Tiers) != 1 ||
		decoded.Priority.Tiers[0].Name != "release" || len(decoded.Priority.Tiers[0].Match) != 1 ||
		decoded.Priority.Tiers[0].Match[0].WorkflowRef != "*/.github/workflows/release*.yml@*" {
		t.Fatalf("decoded priority = %#v", decoded.Priority)
	}
}

func TestAConfigWithoutPriorityEncodesNoPriorityKey(t *testing.T) {
	var buffer bytes.Buffer
	if err := Encode(&buffer, Default()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buffer.String(), "priority") {
		t.Fatalf("an undeclared policy encoded a priority key:\n%s", buffer.String())
	}
	decoded, err := Decode(&buffer)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Priority.Policy().Declared() || decoded.Priority.EscalateAfter != 0 {
		t.Fatalf("decoded priority = %#v, want the zero policy", decoded.Priority)
	}
}

func TestPriorityValidationRefusesUnsafeDeclarations(t *testing.T) {
	long := strings.Repeat("a", maxPriorityPatternLength+1)
	many := make([]domain.PriorityTier, maxPriorityTiers+1)
	for i := range many {
		many[i] = domain.PriorityTier{Name: "t" + string(rune('a'+i)), Match: []domain.PriorityMatch{{Scope: "a/b"}}}
	}
	cases := []struct {
		name     string
		priority Priority
		want     string
	}{
		{"escalation without tiers", Priority{EscalateAfter: time.Minute}, "requires at least one tier"},
		{"tiers without escalation", Priority{Tiers: releaseTier().Tiers}, "escalation threshold"},
		{"escalation too short", Priority{EscalateAfter: time.Second, Tiers: releaseTier().Tiers}, "escalation threshold"},
		{"escalation too long", Priority{EscalateAfter: 48 * time.Hour, Tiers: releaseTier().Tiers}, "escalation threshold"},
		{"unnamed tier", Priority{EscalateAfter: time.Hour, Tiers: []domain.PriorityTier{{Match: []domain.PriorityMatch{{Scope: "a/b"}}}}}, "tier name"},
		{"invalid tier name", Priority{EscalateAfter: time.Hour, Tiers: []domain.PriorityTier{{Name: "Release Now", Match: []domain.PriorityMatch{{Scope: "a/b"}}}}}, "tier name"},
		{"duplicate tier name", Priority{EscalateAfter: time.Hour, Tiers: []domain.PriorityTier{
			{Name: "release", Match: []domain.PriorityMatch{{Scope: "a/b"}}},
			{Name: "release", Match: []domain.PriorityMatch{{Scope: "c/d"}}},
		}}, "duplicate"},
		{"tier with no rule", Priority{EscalateAfter: time.Hour, Tiers: []domain.PriorityTier{{Name: "release"}}}, "at least one match rule"},
		{"rule with no facet", Priority{EscalateAfter: time.Hour, Tiers: []domain.PriorityTier{{Name: "release", Match: []domain.PriorityMatch{{}}}}}, "declares none of scope"},
		{"pattern too long", Priority{EscalateAfter: time.Hour, Tiers: []domain.PriorityTier{{Name: "release", Match: []domain.PriorityMatch{{Scope: long}}}}}, "too long"},
		{"too many tiers", Priority{EscalateAfter: time.Hour, Tiers: many}, "at most"},
		{"too many rules", Priority{EscalateAfter: time.Hour, Tiers: []domain.PriorityTier{{Name: "release", Match: tooManyRules()}}}, "at most"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := priorityConfig(test.priority).Validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() = %v, want an error containing %q", err, test.want)
			}
		})
	}
}

func tooManyRules() []domain.PriorityMatch {
	rules := make([]domain.PriorityMatch, maxPriorityRulesPerTier+1)
	for i := range rules {
		rules[i] = domain.PriorityMatch{Scope: "a/b"}
	}
	return rules
}

func TestEncodingRefusesASubSecondEscalationThreshold(t *testing.T) {
	priority := releaseTier()
	priority.EscalateAfter = 90*time.Second + 500*time.Millisecond
	err := Encode(&bytes.Buffer{}, priorityConfig(priority))
	if err == nil || !strings.Contains(err.Error(), "whole seconds") {
		t.Fatalf("Encode() = %v, want a whole-seconds refusal", err)
	}
}

func TestAValidPriorityDeclarationIsAccepted(t *testing.T) {
	if err := priorityConfig(releaseTier()).Validate(); err != nil {
		t.Fatalf("valid priority refused: %v", err)
	}
}

func TestClonedPriorityDoesNotShareItsTiers(t *testing.T) {
	cfg := priorityConfig(releaseTier())
	clone := cfg.Clone()
	clone.Priority.Tiers[0].Name = "mutated"
	clone.Priority.Tiers[0].Match[0].WorkflowRef = "mutated"
	if cfg.Priority.Tiers[0].Name != "release" || cfg.Priority.Tiers[0].Match[0].WorkflowRef == "mutated" {
		t.Fatalf("clone shares tier storage with its source: %#v", cfg.Priority)
	}
}

func TestPolicyProjectsTheDeclaredTiers(t *testing.T) {
	policy := releaseTier().Policy()
	got := policy.Classify(domain.DemandFacts{WorkflowRef: "o/r/.github/workflows/release-stores.yml@refs/heads/main"})
	if got.Tier != "release" || got.Rank != 1 {
		t.Fatalf("classification = %#v, want the release tier at rank 1", got)
	}
}
