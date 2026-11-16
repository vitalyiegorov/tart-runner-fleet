package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

type OrphanRequeueStore interface {
	RecentOrphanRequeues(context.Context, string) ([]operations.OrphanRequeue, error)
	ClaimOrphanRequeue(context.Context, operations.OrphanRequeue, time.Duration, int) (bool, error)
	FinishOrphanRequeue(context.Context, operations.OrphanRequeue) error
}
type OrphanRequeueAPI interface {
	RequeueOrphan(context.Context, githubscaleset.WorkflowJob, func(string, []string) bool, bool) (string, error)
}
type OrphanRecovery struct {
	Config config.OrphanRequeue
	Store  OrphanRequeueStore
	API    OrphanRequeueAPI
}

func (r OrphanRecovery) Reconcile(ctx context.Context, c DemandCoordinator, bindings []Binding, snapshot GitHubQueueSnapshot, slo time.Duration) ([]string, error) {
	if r.Config.Mode != "act" && r.Config.Mode != "observe" {
		return nil, nil
	}
	findings := []string{"orphan requeue mode=" + r.Config.Mode}
	if len(bindings) == 0 {
		return findings, nil
	}
	scope := bindings[0].Scope
	history, err := r.Store.RecentOrphanRequeues(ctx, scope)
	if err != nil {
		return findings, err
	}
	for _, record := range history {
		findings = append(findings, fmt.Sprintf("orphan requeue %s run %d at %s: %s", record.Repository, record.RunID, record.At.UTC().Format(time.RFC3339), record.Outcome))
		if record.Outcome == "permission_denied" && c.now().Sub(record.At) < time.Hour {
			findings = append(findings, "grant Actions: write to the GitHub App; recovery paused for one hour")
			return findings, nil
		}
	}
	candidates, err := c.orphanCandidates(ctx, bindings, snapshot, r.Config.Threshold(slo))
	if err != nil {
		return findings, err
	}
	seen := map[int64]bool{}
	for _, job := range candidates {
		if seen[job.RunID] {
			continue
		}
		seen[job.RunID] = true
		repo := job.Repository.Owner + "/" + job.Repository.Name
		if !orphanOwner(c.AuditedAssignments, bindings, snapshot, job) {
			findings = append(findings, fmt.Sprintf("orphan requeue %s run %d: sibling owner", repo, job.RunID))
			continue
		}
		excluded := func(workflow string, jobs []string) bool { return r.Config.Excluded(repo, workflow, jobs) }
		if r.Config.Mode == "observe" {
			outcome, err := r.API.RequeueOrphan(ctx, job, excluded, false)
			if err != nil {
				return findings, err
			}
			findings = append(findings, fmt.Sprintf("orphan requeue %s run %d job %d age %.0fm: %s", repo, job.RunID, job.ID, c.now().Sub(job.CreatedAt).Minutes(), outcome))
			continue
		}
		record := operations.OrphanRequeue{Scope: scope, Repository: repo, RunID: job.RunID, At: c.now(), Outcome: "reserved"}
		claimed, err := r.Store.ClaimOrphanRequeue(ctx, record, r.Config.Cooldown(), r.Config.Limit())
		if err != nil {
			return findings, err
		}
		if !claimed {
			continue
		}
		outcome, actionErr := r.API.RequeueOrphan(ctx, job, excluded, true)
		if actionErr != nil {
			outcome = "failed"
		}
		record.Outcome = outcome
		if err := r.Store.FinishOrphanRequeue(ctx, record); err != nil {
			return findings, err
		}
		findings = append(findings, fmt.Sprintf("orphan requeue %s run %d job %d age %.0fm: %s", repo, job.RunID, job.ID, c.now().Sub(job.CreatedAt).Minutes(), outcome))
		if outcome == "permission_denied" {
			findings = append(findings, "grant Actions: write to the GitHub App; recovery paused for one hour")
			break
		}
		if actionErr != nil {
			return findings, actionErr
		}
		// Re-enter through a fresh inventory and audit before another mutation.
		break
	}
	return findings, nil
}

// Elect over every queued job in the run, not just the candidate this node
// serves: separate routing labels in one run must still elect a single actor.
func orphanOwner(rows []ScaleSetAssignment, bindings []Binding, snapshot GitHubQueueSnapshot, candidate githubscaleset.WorkflowJob) bool {
	var lowest int64
	found := false
	for _, job := range snapshot.QueuedJobs() {
		if job.RunID != candidate.RunID || job.Repository != candidate.Repository {
			continue
		}
		for _, row := range rows {
			if !strings.EqualFold(row.Scope, bindings[0].Scope) || (!containsFold(job.Labels, row.Name) && !advertisesEveryRoutingLabel(row.Labels, job.Labels) && !localAuditMatch(row, bindings, job)) {
				continue
			}
			if !row.Fresh || row.Assigned != 0 {
				return false
			}
			if !found || row.ID < lowest {
				lowest = row.ID
				found = true
			}
		}
	}
	for _, b := range bindings {
		if found && b.ScaleSetID == lowest {
			return true
		}
	}
	return false
}
func localAuditMatch(row ScaleSetAssignment, bindings []Binding, job githubscaleset.WorkflowJob) bool {
	matched, err := matchingBindings(bindings, job)
	if err != nil {
		return false
	}
	for _, b := range matched {
		if b.ScaleSetID == row.ID {
			return true
		}
	}
	return false
}
