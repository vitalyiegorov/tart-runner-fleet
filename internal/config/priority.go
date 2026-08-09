package config

import (
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// Priority is the operator's declaration of which demand outranks which
// (issue #224). Tiers are ordered highest first and the default tier -- the one
// nobody writes down -- ranks below all of them.
//
// EscalateAfter is not optional decoration beside the list. A tier is a licence
// for one class of work to overtake another, and without escalation a steady
// stream of high-tier arrivals starves everything below it. Validate refuses
// tiers without a threshold and a threshold without tiers, so the two facts can
// never drift apart in a file an operator edits by hand.
type Priority struct {
	EscalateAfter time.Duration
	Tiers         []domain.PriorityTier
}

// Policy is the classification the planner and the queue view both read.
func (p Priority) Policy() domain.PriorityPolicy {
	return domain.PriorityPolicy{Tiers: p.Tiers}
}

// The bounds below exist because this is configuration a human writes and the
// planner then evaluates on every tick for every queued demand. None of them is
// a limit anybody should reach: three tiers and a handful of rules already say
// everything an operator has ever asked for at the console.
const (
	maxPriorityTiers         = 8
	maxPriorityRulesPerTier  = 16
	maxPriorityPatternLength = 256
	minPriorityEscalation    = time.Minute
	maxPriorityEscalation    = 24 * time.Hour
)

// priorityTierGrammar keeps a tier name renderable in a table column, a JSON
// field, and a log line without quoting or truncation rules of its own. It is
// the same shape a capability declaration uses.
var priorityTierGrammar = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

func (p Priority) validate() error {
	if len(p.Tiers) == 0 {
		if p.EscalateAfter != 0 {
			// Escalation with no tier is not a harmless setting: every demand would
			// still be default tier, but they would be grouped by age band and the
			// bounded young lanes would stop deciding anything. Refuse it rather
			// than silently change the order of a fleet that declared no policy.
			return errors.New("priority escalation requires at least one tier")
		}
		return nil
	}
	if len(p.Tiers) > maxPriorityTiers {
		return fmt.Errorf("priority declares %d tiers, at most %d are allowed", len(p.Tiers), maxPriorityTiers)
	}
	if p.EscalateAfter < minPriorityEscalation || p.EscalateAfter > maxPriorityEscalation {
		return fmt.Errorf("priority escalation threshold must be between %s and %s", minPriorityEscalation, maxPriorityEscalation)
	}
	seen := make(map[string]bool, len(p.Tiers))
	for _, tier := range p.Tiers {
		if !priorityTierGrammar.MatchString(tier.Name) {
			return fmt.Errorf("invalid priority tier name %q; a tier name is lower-case letters, digits, and dashes", tier.Name)
		}
		if tier.Name == domain.DefaultPriorityTier {
			return fmt.Errorf("priority tier %q is reserved for the tier every unmatched demand lands in", domain.DefaultPriorityTier)
		}
		if seen[tier.Name] {
			return fmt.Errorf("duplicate priority tier %q", tier.Name)
		}
		seen[tier.Name] = true
		if err := validateTierRules(tier); err != nil {
			return err
		}
	}
	return nil
}

func validateTierRules(tier domain.PriorityTier) error {
	if len(tier.Match) == 0 {
		return fmt.Errorf("priority tier %q needs at least one match rule", tier.Name)
	}
	if len(tier.Match) > maxPriorityRulesPerTier {
		return fmt.Errorf("priority tier %q declares %d match rules, at most %d are allowed", tier.Name, len(tier.Match), maxPriorityRulesPerTier)
	}
	for _, rule := range tier.Match {
		if rule.Scope == "" && rule.WorkflowRef == "" && rule.JobName == "" {
			return fmt.Errorf("priority tier %q has a match rule that declares none of scope, workflowRef, or jobName", tier.Name)
		}
		for _, pattern := range []string{rule.Scope, rule.WorkflowRef, rule.JobName} {
			if len(pattern) > maxPriorityPatternLength {
				return fmt.Errorf("priority tier %q has a match pattern that is too long (%d characters, at most %d)", tier.Name, len(pattern), maxPriorityPatternLength)
			}
		}
	}
	return nil
}

func (p Priority) clone() Priority {
	out := p
	out.Tiers = append([]domain.PriorityTier(nil), p.Tiers...)
	for i := range out.Tiers {
		out.Tiers[i].Match = append([]domain.PriorityMatch(nil), p.Tiers[i].Match...)
	}
	return out
}

// wirePriority is the on-disk shape. It is a pointer in wireConfig so a fleet
// that declares no policy encodes no key at all, which is what lets a release
// older than this setting keep decoding the file with DisallowUnknownFields.
type wirePriority struct {
	EscalateAfterSeconds int                `json:"escalateAfterSeconds"`
	Tiers                []wirePriorityTier `json:"tiers"`
}

type wirePriorityTier struct {
	Name  string              `json:"name"`
	Match []wirePriorityMatch `json:"match"`
}

type wirePriorityMatch struct {
	Scope       string `json:"scope,omitempty"`
	WorkflowRef string `json:"workflowRef,omitempty"`
	JobName     string `json:"jobName,omitempty"`
}

func decodePriority(w *wirePriority) Priority {
	if w == nil {
		return Priority{}
	}
	priority := Priority{EscalateAfter: time.Duration(w.EscalateAfterSeconds) * time.Second,
		Tiers: make([]domain.PriorityTier, 0, len(w.Tiers))}
	for _, tier := range w.Tiers {
		rules := make([]domain.PriorityMatch, 0, len(tier.Match))
		for _, rule := range tier.Match {
			rules = append(rules, domain.PriorityMatch{Scope: rule.Scope, WorkflowRef: rule.WorkflowRef, JobName: rule.JobName})
		}
		priority.Tiers = append(priority.Tiers, domain.PriorityTier{Name: tier.Name, Match: rules})
	}
	return priority
}

func encodePriority(priority Priority) (*wirePriority, error) {
	if len(priority.Tiers) == 0 {
		return nil, nil
	}
	seconds, err := wholeSeconds("priority escalation", priority.EscalateAfter)
	if err != nil {
		return nil, err
	}
	wire := &wirePriority{EscalateAfterSeconds: seconds, Tiers: make([]wirePriorityTier, 0, len(priority.Tiers))}
	for _, tier := range priority.Tiers {
		rules := make([]wirePriorityMatch, 0, len(tier.Match))
		for _, rule := range tier.Match {
			rules = append(rules, wirePriorityMatch{Scope: rule.Scope, WorkflowRef: rule.WorkflowRef, JobName: rule.JobName})
		}
		wire.Tiers = append(wire.Tiers, wirePriorityTier{Name: tier.Name, Match: rules})
	}
	return wire, nil
}
