package githubscaleset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// RequeueOrphan deliberately bypasses inventory ETags. All pages and the run
// attempt are checked immediately before cancel. GitHub offers no atomic
// cancel-if-no-running-jobs API; this residual race is documented in ADR 0099.
func (o *Observer) RequeueOrphan(ctx context.Context, job WorkflowJob, excluded func(string, []string) bool, act bool) (outcome string, err error) {
	defer func() {
		var api *APIError
		if errors.As(err, &api) && api.Kind == Authorization {
			outcome, err = "permission_denied", nil
		}
	}()
	if job.ID <= 0 || job.RunID <= 0 || job.RunAttempt <= 0 {
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
	if !domain.CanRequeueOrphan(run.Status, jobs, job.ID) || excluded(run.Name, names) {
		return "held", nil
	}
	// Reject a rerun that raced the jobs listing.
	if err := readRun(); err != nil {
		return "", err
	}
	if run.Attempt != job.RunAttempt || (run.Status != "queued" && run.Status != "in_progress") {
		return "noop", nil
	}
	if !act {
		return "would_requeue", nil
	}
	status, err := fresh.orphanPost(ctx, base+"/cancel")
	if err != nil {
		return "", err
	}
	if status == http.StatusConflict || status == http.StatusUnprocessableEntity {
		return "noop", nil
	}
	// Bounded by both request deadlines and a one-minute overall deadline.
	pollCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	for i := 0; i < 12; i++ {
		endpoint, _ := fresh.resolve(base)
		answer, err := fresh.page(pollCtx, endpoint.String())
		if err != nil {
			return "", err
		}
		run = orphanRunResponse{}
		if err := json.Unmarshal(answer.body, &run); err != nil {
			return "", err
		}
		if run.ID != job.RunID || run.Attempt != job.RunAttempt {
			return "noop", nil
		}
		if run.Status == "completed" {
			status, err := fresh.orphanPost(pollCtx, base+"/rerun-failed-jobs")
			if err != nil {
				return "", err
			}
			if status == http.StatusConflict || status == http.StatusUnprocessableEntity {
				return "noop", nil
			}
			return "rerun", nil
		}
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-pollCtx.Done():
			timer.Stop()
			return "", pollCtx.Err()
		case <-timer.C:
		}
	}
	return "", errors.New("orphan cancellation did not complete within one minute")
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
	ID      int64
	Status  string
	Name    string
	Attempt int `json:"run_attempt"`
}
