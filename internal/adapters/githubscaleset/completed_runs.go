package githubscaleset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
)

// CompletedRuns shares the observer's authenticated, deadline-bound transport.
// At most ten run attempts are read per poll. Rotation prevents an active oldest
// run from starving later candidates. No response body is logged or persisted.
func (o *Observer) CompletedRuns(ctx context.Context, candidates []WorkflowRun) ([]WorkflowRun, error) {
	sort.Slice(candidates, func(i, j int) bool { return runOrder(candidates[i]) < runOrder(candidates[j]) })
	start := sort.Search(len(candidates), func(i int) bool { return runOrder(candidates[i]) > o.completionCursor })
	if start == len(candidates) {
		start = 0
	}
	var completed []WorkflowRun
	for i := 0; i < min(10, len(candidates)); i++ {
		run := candidates[(start+i)%len(candidates)]
		o.completionCursor = runOrder(run)
		if run.ID <= 0 || run.Attempt < 0 || run.Repository.Owner == "" || run.Repository.Name == "" {
			continue
		}
		path := fmt.Sprintf("/repos/%s/%s/actions/runs/%d", url.PathEscape(run.Repository.Owner), url.PathEscape(run.Repository.Name), run.ID)
		if run.Attempt > 0 {
			path += fmt.Sprintf("/attempts/%d", run.Attempt)
		}
		endpoint, _ := o.resolve(path)
		page, err := o.page(ctx, endpoint.String())
		if err != nil {
			var api *APIError
			if errors.As(err, &api) && api.Kind != Authentication && api.Kind != Authorization && api.Kind != RateLimited {
				continue
			}
			return completed, err
		}
		var answer struct {
			ID      int64
			Status  string
			Attempt int `json:"run_attempt"`
		}
		if err := json.Unmarshal(page.body, &answer); err != nil {
			continue
		}
		// An unknown broker attempt uses the latest run: completed there proves all
		// older attempts terminal too. A known attempt always requires an exact match.
		if answer.ID == run.ID && answer.Status == "completed" && answer.Attempt > 0 && (run.Attempt == 0 || answer.Attempt == run.Attempt) {
			completed = append(completed, run)
		}
	}
	return completed, nil
}

func runOrder(run WorkflowRun) string {
	return fmt.Sprintf("%s/%s/%020d/%010d", run.Repository.Owner, run.Repository.Name, run.ID, run.Attempt)
}
