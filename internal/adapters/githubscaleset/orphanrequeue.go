package githubscaleset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// RequeueOrphan deliberately bypasses inventory ETags. All pages and the run
// attempt are checked immediately before cancel. GitHub offers no atomic
// cancel-if-no-running-jobs API; this residual race is documented in ADR 0099.
func (o *Observer) RequeueOrphan(ctx context.Context, job WorkflowJob, excluded func(string, []string) bool, act bool) (outcome string, err error) {
	defer func() {
		var api *APIError
		if errors.As(err, &api) && api.Kind == Authorization && api.RetryAfter == 0 && outcome != "cancelled_not_rerun" {
			outcome, err = "permission_denied", nil
		}
	}()
	if (!job.ResumeOrphan && job.ID <= 0) || job.RunID <= 0 || job.RunAttempt <= 0 {
		return "noop", nil
	}
	if act {
		permissions, ok := o.tokens.(interface {
			ActionsWrite(context.Context) (bool, error)
		})
		if !ok {
			return "permission_denied", nil
		}
		write, err := permissions.ActionsWrite(ctx)
		if err != nil {
			return "", err
		}
		if !write {
			return "permission_denied", nil
		}
	}
	// An isolated observer shares transport/authority but has no cached pages.
	fresh := *o
	fresh.cache = map[string]cachedPage{}
	fresh.touched = nil
	base := fmt.Sprintf("/repos/%s/%s/actions/runs/%d", url.PathEscape(job.Repository.Owner), url.PathEscape(job.Repository.Name), job.RunID)
	var run orphanRunResponse
	readRun := func() error {
		fresh.cache = map[string]cachedPage{}
		endpoint, _ := fresh.resolve(base)
		page, e := fresh.page(ctx, endpoint.String())
		if e != nil {
			return e
		}
		run = orphanRunResponse{}
		return json.Unmarshal(page.body, &run)
	}
	if err := readRun(); err != nil {
		return "", err
	}
	if job.ResumeOrphan {
		if run.Status == "queued" || run.Status == "in_progress" {
			return "noop", nil
		}
		if !act {
			return "held", nil
		}
		return fresh.resumeOrphan(ctx, base, job)
	}
	if run.ID != job.RunID || run.Attempt != job.RunAttempt || run.Name == "" || (run.Status != "queued" && run.Status != "in_progress") {
		return "noop", nil
	}
	var page struct {
		Jobs []domain.OrphanRunJob `json:"jobs"`
	}
	names := []string{}
	var jobs []domain.OrphanRunJob
	if err := fresh.pages(ctx, base+"/jobs?filter=latest&per_page=100", &page, func() {
		for _, j := range page.Jobs {
			names = append(names, j.Name)
		}
		jobs = append(jobs, page.Jobs...)
		page.Jobs = nil
	}); err != nil {
		return "", err
	}
	for _, name := range names {
		if name == "" {
			return "held", nil
		}
	}
	if excluded(run.Name, names) {
		return "excluded", nil
	}
	if !domain.CanRequeueOrphan(run.Status, jobs, job.ID, job.OrphanBefore) {
		return "held", nil
	}
	initialRun := run
	// Reject any transition that raced the jobs listing.
	if err := readRun(); err != nil {
		return "", err
	}
	if run != initialRun {
		return "noop", nil
	}
	fresh.cache = map[string]cachedPage{}
	var final []domain.OrphanRunJob
	if err := fresh.pages(ctx, base+"/jobs?filter=latest&per_page=100", &page, func() { final = append(final, page.Jobs...); page.Jobs = nil }); err != nil {
		return "", err
	}
	if !slices.Equal(jobs, final) || !domain.CanRequeueOrphan(run.Status, final, job.ID, job.OrphanBefore) {
		return "held", nil
	}
	if !act {
		return "would_requeue", nil
	}
	status, err := fresh.orphanPost(ctx, base+"/cancel")
	if err != nil {
		var api *APIError
		if errors.As(err, &api) && api.Status >= 400 && api.Status < 500 {
			return "", err
		}
		return "cancelled_not_rerun", err
	}
	if status == http.StatusConflict || status == http.StatusUnprocessableEntity {
		return "noop", nil
	}
	return fresh.resumeOrphan(ctx, base, job)
}

// resumeOrphan never cancels. The durable attempt ties retries to the cancelled
// attempt; a newer attempt is already recovered and must remain untouched.
func (o *Observer) resumeOrphan(ctx context.Context, base string, job WorkflowJob) (outcome string, err error) {
	pollCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	return o.pollOrphan(pollCtx, base, job)
}

func (o *Observer) pollOrphan(pollCtx context.Context, base string, job WorkflowJob) (outcome string, err error) {
	outcome = "cancelled_not_rerun"
	for {
		o.cache = map[string]cachedPage{}
		endpoint, _ := o.resolve(base)
		answer, e := o.page(pollCtx, endpoint.String())
		if e != nil {
			return outcome, e
		}
		var run orphanRunResponse
		if e := json.Unmarshal(answer.body, &run); e != nil {
			return outcome, e
		}
		if run.ID != job.RunID {
			return outcome, errors.New("unexpected orphan run")
		}
		if run.Attempt > job.RunAttempt {
			return "noop", nil
		}
		if run.Attempt != job.RunAttempt {
			return outcome, errors.New("unexpected orphan attempt")
		}
		if run.Status == "completed" {
			if run.Conclusion != "cancelled" {
				return "noop", nil
			}
			status, e := o.orphanPost(pollCtx, base+"/rerun-failed-jobs")
			if e != nil {
				return outcome, e
			}
			if status == http.StatusConflict || status == http.StatusUnprocessableEntity {
				return outcome, nil
			}
			return "rerun", nil
		}
		if run.Status != "queued" && run.Status != "in_progress" {
			return outcome, nil
		}
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-pollCtx.Done():
			timer.Stop()
			return outcome, pollCtx.Err()
		case <-timer.C:
		}
	}
}
func (o *Observer) orphanPost(ctx context.Context, path string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	token, err := o.tokens.Token(ctx)
	if err != nil {
		return 0, err
	}
	endpoint, _ := o.resolve(path)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	response, err := o.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode == http.StatusConflict || response.StatusCode == http.StatusUnprocessableEntity || (response.StatusCode >= 200 && response.StatusCode < 300) {
		return response.StatusCode, nil
	}
	return response.StatusCode, ClassifyResponse(response, o.clock.Now(), nil)
}

type orphanRunResponse struct {
	ID         int64
	Conclusion string
	Status     string
	Name       string
	Attempt    int `json:"run_attempt"`
}
