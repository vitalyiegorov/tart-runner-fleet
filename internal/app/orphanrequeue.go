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
	RequestAudit func(context.Context) error
	AuditMaxAge  time.Duration
	Config       config.OrphanRequeue
	Store        OrphanRequeueStore
	API          OrphanRequeueAPI
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
		if record.Outcome == "cancelled_not_rerun" || record.Outcome == "reserved" && c.now().Sub(record.At) >= time.Minute {
			findings = append(findings, fmt.Sprintf("gh run rerun %d --failed -R %s", record.RunID, record.Repository))
		}
		findings = append(findings, fmt.Sprintf("orphan requeue %s run %d at %s: %s", record.Repository, record.RunID, record.At.UTC().Format(time.RFC3339), record.Outcome))
		if record.Outcome == "permission_denied" && c.now().Sub(record.At) < time.Hour {
			findings = append(findings, "grant Actions: write to the GitHub App; recovery paused for one hour")
			return findings, nil
		}
	}
	if r.Config.Mode == "act" {
		for _, record := range history {
			if record.Outcome != "cancelled_not_rerun" || c.now().Sub(record.At) < r.Config.Cooldown() {
				continue
			}
			parts := strings.SplitN(record.Repository, "/", 2)
			if len(parts) != 2 || record.RunAttempt <= 0 {
				continue
			}
			record.RetryAt = c.now()
			claimed, err := r.Store.ClaimOrphanRequeue(ctx, record, r.Config.Cooldown(), r.Config.Limit())
			if err != nil {
				return findings, err
			}
			if !claimed {
				continue
			}
			outcome, actionErr := r.API.RequeueOrphan(ctx, githubscaleset.WorkflowJob{ResumeOrphan: true, RunID: record.RunID, RunAttempt: record.RunAttempt, Repository: githubscaleset.Repository{Owner: parts[0], Name: parts[1]}}, func(string, []string) bool { return false }, true)
			if outcome != "rerun" && outcome != "noop" && outcome != "permission_denied" && outcome != "not_rerunnable" {
				outcome = "cancelled_not_rerun"
			}
			record.Outcome = outcome
			finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			finishErr := r.Store.FinishOrphanRequeue(finishCtx, record)
			cancel()
			if finishErr != nil {
				return findings, finishErr
			}
			return findings, actionErr
		}
	}
	candidates, err := c.orphanCandidates(ctx, bindings, snapshot, r.Config.Threshold(slo))
	if err != nil {
		return findings, err
	}
	auditRequested := false
	for _, job := range candidates {
		repo := job.Repository.Owner + "/" + job.Repository.Name
		rows := append([]ScaleSetAssignment(nil), c.AuditedAssignments...)
		for i := range rows {
			rows[i].Fresh = rows[i].Fresh && recoveryAuditFresh(rows[i].ObservedAt, c.now(), job.CreatedAt.Add(r.Config.Threshold(slo)), r.AuditMaxAge)
		}
		if reason := orphanOwner(rows, bindings, snapshot, job, c.now().Sub(job.CreatedAt), r.Config.Threshold(slo), r.Config.Cooldown()); reason != "" {
			findings = append(findings, fmt.Sprintf("orphan requeue %s run %d: %s", repo, job.RunID, reason))
			if reason == "awaiting fresh audit" && !auditRequested && r.RequestAudit != nil {
				auditRequested = true
				if err := r.RequestAudit(ctx); err != nil {
					return findings, err
				}
			}
			continue
		}
		excluded := func(workflow string, jobs []string) bool { return r.Config.Excluded(repo, workflow, jobs) }

		record := operations.OrphanRequeue{Scope: scope, Repository: repo, RunID: job.RunID, At: c.now(), Outcome: "reserved", RunAttempt: job.RunAttempt}
		claimed, err := r.Store.ClaimOrphanRequeue(ctx, record, r.Config.Cooldown(), r.Config.Limit())
		if err != nil {
			return findings, err
		}
		if !claimed {
			continue
		}
		job.OrphanBefore = c.now().Add(-r.Config.Threshold(slo))
		outcome, actionErr := r.API.RequeueOrphan(ctx, job, excluded, r.Config.Mode == "act")
		if actionErr != nil && outcome == "" {
			outcome = "failed"
		}
		record.Outcome = outcome
		finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		finishErr := r.Store.FinishOrphanRequeue(finishCtx, record)
		cancel()
		if err := finishErr; err != nil {
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

// Rank over every queued job in the run, including separate routing labels.
// Higher ranks become eligible after a stagger; no peer liveness protocol.
func orphanOwner(rows []ScaleSetAssignment, bindings []Binding, snapshot GitHubQueueSnapshot, candidate githubscaleset.WorkflowJob, age, threshold, cooldown time.Duration) string {
	ids := map[int64]bool{}
	for _, job := range snapshot.QueuedJobs() {
		if job.RunID != candidate.RunID || job.Repository != candidate.Repository {
			continue
		}
		for _, row := range rows {
			if !strings.EqualFold(row.Scope, bindings[0].Scope) || (!containsFold(job.Labels, row.Name) && !advertisesEveryRoutingLabel(row.Labels, job.Labels) && !localAuditMatch(row, bindings, job)) {
				continue
			}
			if !row.Fresh {
				return "awaiting fresh audit"
			}
			if row.Assigned != 0 {
				return fmt.Sprintf("assigned to set %d", row.ID)
			}
			ids[row.ID] = true
		}
	}
	bestRank := len(ids)
	for _, b := range bindings {
		if !ids[b.ScaleSetID] {
			continue
		}
		rank := 0
		for id := range ids {
			if id < b.ScaleSetID {
				rank++
			}
		}
		if age >= threshold+time.Duration(rank)*cooldown {
			return ""
		}
		bestRank = min(bestRank, rank)
	}
	if bestRank == len(ids) {
		return "awaiting fresh audit"
	}
	return fmt.Sprintf("sibling owner (rank %d)", bestRank)
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

func recoveryAuditFresh(at, now, threshold time.Time, maxAge time.Duration) bool {
	if maxAge == 0 {
		return true
	}
	return !at.IsZero() && !at.After(now) && (now.Sub(at) <= maxAge || !at.Before(threshold))
}
