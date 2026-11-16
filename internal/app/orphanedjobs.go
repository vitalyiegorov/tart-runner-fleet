package app

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
)

// ScaleSetAssignment is one audit reading, including whether its timestamp is fresh.
// A missing or stale reading is unknown, never evidence of zero assignment.
type ScaleSetAssignment struct {
	ObservedAt time.Time
	Scope      string
	ID         int64
	Name       string
	Labels     []string
	Assigned   int
	Fresh      bool
}

// orphanCandidates is the shared ADR 0098 predicate; REST cannot identify concurrency holds.
func (c DemandCoordinator) orphanCandidates(ctx context.Context, bindings []Binding, snapshot GitHubQueueSnapshot, slo time.Duration) ([]githubscaleset.WorkflowJob, error) {
	if snapshot == nil || slo <= 0 || len(c.AuditedAssignments) == 0 {
		return nil, nil
	}
	now := c.now()
	readings := map[int64]map[string]bool{}
	var candidates []githubscaleset.WorkflowJob
	for _, job := range snapshot.QueuedJobs() {
		if job.Status != "queued" || !job.QueueTimeExact || job.CreatedAt.IsZero() || now.Sub(job.CreatedAt) < slo {
			continue
		}
		matched, err := matchingBindings(bindings, job)
		if err != nil {
			return nil, err
		}
		orphaned := len(matched) > 0
		for _, binding := range matched {
			key := binding.durableKey()
			if !c.auditUnassigned(*binding, job.Labels) {
				orphaned = false
				continue
			}
			vouched, seen := readings[key]
			if !seen {
				vouched, err = c.vouchedJobs(ctx, *binding)
				if err != nil {
					return nil, err
				}
				readings[key] = vouched
			}
			if vouched[restJobKey(job.ID)] || vouched[restCorrelationKey(job.Repository.Owner, job.Repository.Name, job.RunID, job.Name)] {
				orphaned = false
			}
		}
		if orphaned {
			candidates = append(candidates, job)
		}
	}
	return candidates, nil
}

// UndeliveredJobs retains ADR 0098's read-only candidate diagnosis.
func (c DemandCoordinator) UndeliveredJobs(ctx context.Context, bindings []Binding, snapshot GitHubQueueSnapshot, slo time.Duration) ([]string, error) {
	candidates, err := c.orphanCandidates(ctx, bindings, snapshot, slo)
	if err != nil {
		return nil, err
	}
	var findings []string
	for _, job := range candidates {
		repo := job.Repository.Owner + "/" + job.Repository.Name
		findings = append(findings, fmt.Sprintf("%s run %d job %d: queued for %.0fm; unassigned job candidate: every matching audited set reports assigned=0 with no local runner demand. Rule out a concurrency group hold and a sibling backlog first. Operator remedy only after confirming the run is not held by a concurrency group: gh run cancel %d -R %s; then gh run rerun %d --failed -R %s", repo, job.RunID, job.ID, c.now().Sub(job.CreatedAt).Minutes(), job.RunID, repo, job.RunID, repo))
	}
	sort.Strings(findings)
	return findings, nil
}

// auditUnassigned requires local coverage and zero assignment across all matching
// sets in the scope, including siblings. A stale matching sibling vetoes it.
func (c DemandCoordinator) auditUnassigned(binding Binding, labels []string) bool {
	local := false
	for _, row := range c.AuditedAssignments {
		if !strings.EqualFold(row.Scope, binding.Scope) {
			continue
		}
		if row.ID != binding.ScaleSetID && !containsFold(labels, row.Name) && !advertisesEveryRoutingLabel(row.Labels, labels) {
			continue
		}
		if !row.Fresh || row.Assigned != 0 {
			return false
		}
		local = local || row.ID == binding.ScaleSetID
	}
	return local
}
