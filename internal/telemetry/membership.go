package telemetry

import (
	"fmt"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
)

// MembershipMetric is this node's published place in the fleet under ADR 0063:
// its declared mode, the worst stage of any set it serves, and the sets that
// are not serving. It is published only by a node that declared a membership;
// its absence means no watchdog runs here, never that every set is fine.
type MembershipMetric struct {
	Mode     string
	State    string
	Bindings []MembershipBindingMetric
}

// MembershipBindingMetric is one bound set that is not serving.
type MembershipBindingMetric struct {
	Scope                string
	ScaleSetID           int
	Stage                string
	Since                time.Time
	Attempts, Exclusions int
}

// membershipStages is the closed label space of the membership gauge, so a
// scrape always carries every stage and a zero is a zero, not a missing series.
var membershipStages = []string{"healing", "excluded", "probation"}

// SetMembership publishes the node's membership.
func (h *Health) SetMembership(metric MembershipMetric) {
	metric.Bindings = append([]MembershipBindingMetric(nil), metric.Bindings...)
	h.mu.Lock()
	h.revision++
	h.membership = &metric
	h.mu.Unlock()
}

// membershipResult is the judgement, shared by the status envelope and nothing
// else. Healing and probation are the watchdog doing its job, so they pass and
// say so; an excluded set fails, because this node has stopped serving it on
// purpose and that must never read like a quiet day (#292's lesson, again).
func membershipResult(metric *MembershipMetric) HealthResult {
	if metric == nil {
		return HealthResult{OK: true, Reasons: []string{}}
	}
	result := HealthResult{OK: true, Reasons: []string{}}
	for _, row := range metric.Bindings {
		result.Reasons = append(result.Reasons, membershipReason(row))
		if row.Stage == "excluded" {
			result.OK = false
		}
	}
	return result
}

func membershipReason(row MembershipBindingMetric) string {
	set := fmt.Sprintf("%s scale set %d", row.Scope, row.ScaleSetID)
	since := row.Since.UTC().Format(time.RFC3339)
	switch row.Stage {
	case "excluded":
		return fmt.Sprintf("%s is excluded since %s (exclusion %d): healing its session did not restore delivery, "+
			"so the session is released and GitHub binds new jobs to a sibling advertising the same labels; it rejoins "+
			"on probation by itself, and if it keeps failing recreate it with `fleet scale-sets recreate` (ADR 0056)",
			set, since, row.Exclusions)
	case "probation":
		return fmt.Sprintf("%s rejoined on probation since %s after %d exclusion(s); the next audit decides", set, since, row.Exclusions)
	default:
		return fmt.Sprintf("%s is wedged and healing since %s: its session recycled %d time(s)", set, since, row.Attempts)
	}
}

// membershipRow projects the published membership into the versioned surface.
func membershipRow(snapshot Snapshot) *adminapi.Membership {
	metric := snapshot.Membership
	if metric == nil {
		return nil
	}
	row := &adminapi.Membership{Mode: metric.Mode, State: metric.State, Bindings: []adminapi.MembershipBinding{}}
	for _, binding := range metric.Bindings {
		row.Bindings = append(row.Bindings, adminapi.MembershipBinding{Scope: binding.Scope, ScaleSetID: binding.ScaleSetID,
			Stage: binding.Stage, Since: binding.Since, Attempts: binding.Attempts, Exclusions: binding.Exclusions})
	}
	return row
}

// membershipGauges counts the sets in each non-serving stage.
func membershipGauges(metric *MembershipMetric) map[string]int {
	counts := make(map[string]int, len(membershipStages))
	for _, row := range metric.Bindings {
		counts[row.Stage]++
	}
	return counts
}
