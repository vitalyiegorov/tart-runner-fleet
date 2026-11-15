package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// UndeliveredJobs diagnoses REST work before shared-label attribution removes
// jobs assigned nowhere from the local view. It grants no demand or mutation
// authority. Counters are aggregate: zero on ALL matching local sets is evidence
// of non-delivery here, never proof of the state of a sibling's broker.
func (c DemandCoordinator) UndeliveredJobs(ctx context.Context, bindings []Binding, snapshot GitHubQueueSnapshot, slo time.Duration) ([]string, error) {
	statisticsStore, ok := c.Store.(DemandStatisticsStore)
	if !ok || snapshot == nil || slo <= 0 {
		return nil, nil
	}
	now := c.now()
	type evidence struct {
		zero    bool
		vouched map[string]bool
	}
	readings := map[int64]evidence{}
	var findings []string
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
			reading, seen := readings[key]
			if !seen {
				statistics, err := statisticsStore.DemandStatistics(ctx, key)
				if err != nil && !errors.Is(err, operations.ErrNotFound) {
					return nil, err
				}
				reading.zero = err == nil && statistics.Valid() && !statistics.ObservedAt.IsZero() && !statistics.ObservedAt.After(now) && now.Sub(statistics.ObservedAt) <= c.statisticsMaxAge() && statistics.Assigned == 0 && statistics.Available == 0
				if reading.zero {
					reading.vouched, err = c.vouchedJobs(ctx, *binding)
					if err != nil {
						return nil, err
					}
				}
				readings[key] = reading
			}
			if !reading.zero || reading.vouched[restJobKey(job.ID)] || reading.vouched[restCorrelationKey(job.Repository.Owner, job.Repository.Name, job.RunID, job.Name)] {
				orphaned = false
			}
		}
		if orphaned {
			repo := job.Repository.Owner + "/" + job.Repository.Name
			findings = append(findings, fmt.Sprintf("%s run %d job %d (%s): queued for %.0fm; matching local sets report assigned=0 and available=0 with no local runner demand; possible unassigned job (a sibling may be serving it). Operator remedy after checking the run: gh run cancel %d -R %s; then gh run rerun %d --failed -R %s", repo, job.RunID, job.ID, job.Name, now.Sub(job.CreatedAt).Minutes(), job.RunID, repo, job.RunID, repo))
		}
	}
	sort.Strings(findings)
	return findings, nil
}
