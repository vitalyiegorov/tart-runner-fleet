package domain

import "testing"

func releasePolicy() PriorityPolicy {
	return PriorityPolicy{Tiers: []PriorityTier{
		{Name: "release", Match: []PriorityMatch{
			{WorkflowRef: "*/.github/workflows/release*.yml@*"},
			{JobName: "*Publish to Stores*"},
		}},
		{Name: "main", Match: []PriorityMatch{{Scope: "budgie-at/*", WorkflowRef: "*@refs/heads/main"}}},
	}}
}

func TestUndeclaredPolicyClassifiesEveryDemandIntoTheDefaultTier(t *testing.T) {
	var policy PriorityPolicy
	got := policy.Classify(DemandFacts{Repo: "a/b", WorkflowRef: "a/b/.github/workflows/release.yml@refs/heads/main", JobName: "Publish"})
	if got != (Priority{}) {
		t.Fatalf("undeclared policy classified %#v, want the zero priority", got)
	}
}

func TestDeclarationOrderRanksTiersAboveTheDefault(t *testing.T) {
	policy := releasePolicy()
	release := policy.Classify(DemandFacts{Repo: "vitalyiegorov/suuudokuuu",
		WorkflowRef: "vitalyiegorov/suuudokuuu/.github/workflows/release-stores.yml@refs/heads/main", JobName: "App Store"})
	main := policy.Classify(DemandFacts{Repo: "budgie-at/budgie",
		WorkflowRef: "budgie-at/budgie/.github/workflows/ci.yml@refs/heads/main", JobName: "Build iOS E2E app"})
	unmatched := policy.Classify(DemandFacts{Repo: "budgie-at/budgie",
		WorkflowRef: "budgie-at/budgie/.github/workflows/e2e.yml@refs/pull/597/merge", JobName: "Build iOS E2E app"})

	if release.Tier != "release" || main.Tier != "main" || unmatched.Tier != "" {
		t.Fatalf("tiers = %q/%q/%q, want release/main/default", release.Tier, main.Tier, unmatched.Tier)
	}
	if release.Rank <= main.Rank || main.Rank <= unmatched.Rank {
		t.Fatalf("ranks = %d/%d/%d, want strictly descending by declaration order", release.Rank, main.Rank, unmatched.Rank)
	}
	if unmatched.Rank != 0 {
		t.Fatalf("default tier rank = %d, want 0 so an undeclared policy is the zero value", unmatched.Rank)
	}
}

func TestTheFirstDeclaredMatchingTierWins(t *testing.T) {
	policy := PriorityPolicy{Tiers: []PriorityTier{
		{Name: "release", Match: []PriorityMatch{{JobName: "publish*"}}},
		{Name: "main", Match: []PriorityMatch{{Scope: "a/b"}}},
	}}
	got := policy.Classify(DemandFacts{Repo: "a/b", JobName: "publish android"})
	if got.Tier != "release" {
		t.Fatalf("tier = %q, want the first declared match", got.Tier)
	}
}

func TestEveryDeclaredFacetOfOneMatchMustHold(t *testing.T) {
	policy := releasePolicy()
	// The scope matches the "main" rule but the ref does not.
	got := policy.Classify(DemandFacts{Repo: "budgie-at/budgie",
		WorkflowRef: "budgie-at/budgie/.github/workflows/ci.yml@refs/pull/12/merge", JobName: "unit"})
	if got.Tier != "" {
		t.Fatalf("tier = %q, want the default tier: one facet of the rule did not hold", got.Tier)
	}
}

func TestClassificationIsCaseInsensitive(t *testing.T) {
	policy := releasePolicy()
	got := policy.Classify(DemandFacts{Repo: "vitalyiegorov/suuudokuuu", JobName: "Build and PUBLISH TO STORES (ios)"})
	if got.Tier != "release" {
		t.Fatalf("tier = %q, want release: runner-facing names are matched case-insensitively", got.Tier)
	}
}

func TestAMatchWithNoDeclaredFacetNeverClassifies(t *testing.T) {
	policy := PriorityPolicy{Tiers: []PriorityTier{{Name: "release", Match: []PriorityMatch{{}}}}}
	if got := policy.Classify(DemandFacts{Repo: "a/b"}); got.Tier != "" {
		t.Fatalf("tier = %q, want the default tier: an empty rule must not match everything", got.Tier)
	}
}

func TestATierWithNoRuleNeverClassifies(t *testing.T) {
	policy := PriorityPolicy{Tiers: []PriorityTier{{Name: "release"}}}
	if got := policy.Classify(DemandFacts{Repo: "a/b"}); got.Tier != "" {
		t.Fatalf("tier = %q, want the default tier", got.Tier)
	}
}

func TestPatternWildcardsSpanSeparators(t *testing.T) {
	cases := []struct {
		pattern string
		value   string
		want    bool
	}{
		{"*", "anything/at/all", true},
		{"a/b", "a/b", true},
		{"a/b", "a/bc", false},
		{"*release*", "owner/repo/.github/workflows/release.yml@refs/heads/main", true},
		{"owner/*@refs/heads/main", "owner/repo/.github/workflows/ci.yml@refs/heads/main", true},
		{"owner/*@refs/heads/main", "owner/repo/.github/workflows/ci.yml@refs/pull/1/merge", false},
		{"a**b", "ab", true},
		{"owner/*", "other/repo", false},
		{"a*b*c", "a-b", false},
		{"", "anything", false},
	}
	policy := PriorityPolicy{}
	for _, tc := range cases {
		tiered := PriorityPolicy{Tiers: []PriorityTier{{Name: "t", Match: []PriorityMatch{{Scope: tc.pattern}}}}}
		got := tiered.Classify(DemandFacts{Repo: tc.value}).Tier == "t"
		if got != tc.want {
			t.Fatalf("match(%q, %q) = %t, want %t", tc.pattern, tc.value, got, tc.want)
		}
	}
	if policy.Declared() {
		t.Fatal("an empty policy must not report itself as declared")
	}
	if !releasePolicy().Declared() {
		t.Fatal("a policy with tiers must report itself as declared")
	}
}

func TestTheDefaultTierHasAName(t *testing.T) {
	if got := PriorityTierName(Priority{}); got != DefaultPriorityTier {
		t.Fatalf("default tier name = %q, want %q", got, DefaultPriorityTier)
	}
	if got := PriorityTierName(Priority{Tier: "release", Rank: 1}); got != "release" {
		t.Fatalf("declared tier name = %q, want release", got)
	}
}
