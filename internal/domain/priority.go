package domain

import "strings"

// A store release cannot outrank a pull request's E2E build under aged FIFO
// alone. On 2026-08-09 the owner's App Store and Play Store submit jobs
// (vitalyiegorov/suuudokuuu run 31327479374) had waited over an hour, and when
// the mac mini's builder slot freed, the fleet was about to hand it to
// budgie-at/budgie's "Build iOS E2E app" because that job was sixteen minutes
// older. Aged FIFO was working exactly as designed; it simply has no way to say
// that a release outranks ordinary CI. The only lever left was an operator
// cancelling the other repository's queued runs by hand (issue #224).
//
// A PriorityPolicy is that missing sentence, written once in configuration.
// It classifies a demand from what the scale-set message already carries --
// repository, workflow ref, and job display name -- into one of the declared
// tiers, or into the default tier when nothing matches.
//
// The vocabulary is deliberately names, not numbers. ADR 0012 refused an
// unbounded numeric priority in runner labels, and the same reasoning holds
// here: an operator declares an ordered list of named tiers and the rank is
// derived from that order, so no caller can invent a rank of its own.
type PriorityPolicy struct {
	// Tiers are ordered highest priority first. The tier that is not written
	// down -- the default tier every unmatched demand lands in -- ranks below
	// all of them.
	Tiers []PriorityTier
}

// PriorityTier is one declared class of demand. Rank is never written by an
// operator: it is the tier's position in PriorityPolicy.Tiers.
type PriorityTier struct {
	Name  string
	Match []PriorityMatch
}

// PriorityMatch is one rule. Every facet an operator declares must hold (AND);
// a tier claims a demand when ANY of its rules holds (OR). A rule that declares
// no facet at all matches nothing, so an incomplete rule fails closed instead of
// silently promoting the whole fleet.
type PriorityMatch struct {
	// Scope is matched against the repository slug (`owner/repo`).
	Scope string
	// WorkflowRef is matched against the job's workflow ref, which GitHub sends
	// as `owner/repo/.github/workflows/file.yml@refs/heads/main`.
	WorkflowRef string
	// JobName is matched against the job's display name.
	JobName string
}

// DemandFacts are the classification inputs, and they are exactly the facts a
// scale-set JobAvailable message already carries. Nothing here is fetched, so a
// classification can never fail, block, or go stale.
type DemandFacts struct {
	Repo        string
	WorkflowRef string
	JobName     string
}

// Priority is a demand's resolved tier. The zero value is the default tier,
// which is what every demand of an undeclared policy gets -- that is what keeps
// a fleet with no tiers byte-for-byte identical to aged FIFO.
type Priority struct {
	// Tier is the declared tier name, or empty for the default tier.
	Tier string
	// Rank orders tiers: HIGHER outranks. The default tier is zero so an
	// unclassified demand and a demand of a fleet that declares no tiers are the
	// same value.
	Rank int
}

// DefaultPriorityTier is what the tier nobody declares is called on an operator
// surface. The value carried in domain.Priority is empty, because the absence of
// a classification IS the default tier; a renderer needs a word for it, and one
// reserved word is better than each renderer inventing its own. Configuration
// refuses a declared tier of this name so the vocabulary stays closed.
const DefaultPriorityTier = "default"

// PriorityTierName is the tier's name on an operator surface.
func PriorityTierName(priority Priority) string {
	if priority.Tier == "" {
		return DefaultPriorityTier
	}
	return priority.Tier
}

// Declared reports whether an operator wrote any tier down at all.
func (p PriorityPolicy) Declared() bool { return len(p.Tiers) > 0 }

// Classify resolves one demand's tier. The first declared tier that claims the
// demand wins, so an operator reads precedence straight down the list.
func (p PriorityPolicy) Classify(facts DemandFacts) Priority {
	for index, tier := range p.Tiers {
		for _, rule := range tier.Match {
			if rule.matches(facts) {
				return Priority{Tier: tier.Name, Rank: len(p.Tiers) - index}
			}
		}
	}
	return Priority{}
}

func (m PriorityMatch) matches(facts DemandFacts) bool {
	if m.Scope == "" && m.WorkflowRef == "" && m.JobName == "" {
		return false
	}
	for _, facet := range [][2]string{{m.Scope, facts.Repo}, {m.WorkflowRef, facts.WorkflowRef}, {m.JobName, facts.JobName}} {
		if facet[0] == "" {
			continue
		}
		if !matchesPriorityPattern(facet[0], facet[1]) {
			return false
		}
	}
	return true
}

// matchesPriorityPattern is the whole pattern language: `*` stands for any run
// of characters including `/` and `@`, everything else is literal, and the match
// is anchored and case-insensitive. It is deliberately not a regular expression
// -- an operator's configuration must not be able to make the planner do
// unbounded work, and a workflow ref is a path, not a language.
func matchesPriorityPattern(pattern, value string) bool {
	return matchSegments(strings.Split(strings.ToLower(pattern), "*"), strings.ToLower(value))
}

// matchSegments walks the literal segments a pattern splits into. Each segment
// after the first is found at the earliest position that still leaves the rest
// of the value available, which is the standard greedy-free glob walk and is
// linear in the value for the patterns an operator writes.
func matchSegments(segments []string, value string) bool {
	if len(segments) == 1 {
		return segments[0] == value
	}
	if !strings.HasPrefix(value, segments[0]) {
		return false
	}
	value = value[len(segments[0]):]
	last := segments[len(segments)-1]
	for _, segment := range segments[1 : len(segments)-1] {
		index := strings.Index(value, segment)
		if index < 0 {
			return false
		}
		value = value[index+len(segment):]
	}
	return strings.HasSuffix(value, last)
}
