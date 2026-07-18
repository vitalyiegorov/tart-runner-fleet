package githubscaleset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

type ObserverConfig struct {
	BaseURL      string
	Repositories []Repository
	HTTP         Doer
	Tokens       TokenSource
	Clock        Clock
	Timeout      time.Duration
}

type Observer struct {
	base    *url.URL
	repos   []Repository
	http    Doer
	tokens  TokenSource
	clock   Clock
	timeout time.Duration
}

func NewObserver(c ObserverConfig) (*Observer, error) {
	base, err := url.Parse(c.BaseURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, errors.New("valid GitHub API base URL is required")
	}
	if c.HTTP == nil || c.Tokens == nil {
		return nil, errors.New("HTTP client and token source are required")
	}
	if c.Clock == nil {
		c.Clock = realClock{}
	}
	if c.Timeout <= 0 {
		c.Timeout = 15 * time.Second
	}
	return &Observer{base: base, repos: append([]Repository(nil), c.Repositories...), http: c.HTTP, tokens: c.Tokens, clock: c.Clock, timeout: c.Timeout}, nil
}

func (o *Observer) Refresh(ctx context.Context, previous *Snapshot) Observation {
	ctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	s, err := o.fetch(ctx)
	if err == nil {
		return Observation{Freshness: Fresh, Snapshot: s}
	}
	if previous != nil {
		return Observation{Freshness: Stale, Snapshot: previous, Err: err}
	}
	return Observation{Freshness: Unavailable, Err: err}
}

func (o *Observer) fetch(ctx context.Context) (*Snapshot, error) {
	s := &Snapshot{at: o.clock.Now(), runs: map[int64]WorkflowRun{}, jobs: map[int64]WorkflowJob{}, runners: map[int64]Runner{}}
	for _, repo := range o.repos {
		var repoRuns []WorkflowRun
		for _, status := range activeRunStatuses {
			var runPage struct {
				Runs []struct {
					ID        int64     `json:"id"`
					Attempt   int       `json:"run_attempt"`
					Status    string    `json:"status"`
					CreatedAt time.Time `json:"created_at"`
				} `json:"workflow_runs"`
			}
			path := fmt.Sprintf("/repos/%s/%s/actions/runs?status=%s&per_page=100", url.PathEscape(repo.Owner),
				url.PathEscape(repo.Name), url.QueryEscape(status))
			if err := o.pages(ctx, path, &runPage, func() {
				for _, r := range runPage.Runs {
					if !activeRun(r.Status) {
						continue
					}
					if _, duplicate := s.runs[r.ID]; duplicate {
						continue
					}
					run := WorkflowRun{ID: r.ID, Repository: repo, Status: r.Status, Attempt: r.Attempt, CreatedAt: r.CreatedAt.UTC()}
					s.runs[r.ID] = run
					repoRuns = append(repoRuns, run)
				}
				runPage.Runs = nil
			}); err != nil {
				return nil, fmt.Errorf("list %s workflow runs for %s/%s: %w", status, repo.Owner, repo.Name, err)
			}
		}
		for _, run := range repoRuns {
			var jobPage struct {
				Jobs []struct {
					ID           int64 `json:"id"`
					Name, Status string
					Labels       []string  `json:"labels"`
					CreatedAt    time.Time `json:"created_at"`
					StartedAt    time.Time `json:"started_at"`
					CompletedAt  time.Time `json:"completed_at"`
				} `json:"jobs"`
			}
			path := fmt.Sprintf("/repos/%s/%s/actions/runs/%d/jobs?filter=latest&per_page=100", url.PathEscape(repo.Owner), url.PathEscape(repo.Name), run.ID)
			if err := o.pages(ctx, path, &jobPage, func() {
				for _, j := range jobPage.Jobs {
					queuedAt := j.CreatedAt
					exactQueueTime := !queuedAt.IsZero()
					if queuedAt.IsZero() {
						queuedAt = j.StartedAt
						exactQueueTime = !queuedAt.IsZero()
					}
					if queuedAt.IsZero() {
						queuedAt = run.CreatedAt
					}
					job := WorkflowJob{ID: j.ID, RunID: run.ID, Repository: repo, Name: j.Name, Status: j.Status,
						Labels: append([]string(nil), j.Labels...), RunAttempt: run.Attempt, CreatedAt: queuedAt.UTC(),
						QueueTimeExact: exactQueueTime, StartedAt: j.StartedAt.UTC(), CompletedAt: j.CompletedAt.UTC()}
					s.jobs[j.ID] = job
					if j.Status == "queued" {
						s.queued = append(s.queued, j.ID)
					}
				}
				jobPage.Jobs = nil
			}); err != nil {
				return nil, fmt.Errorf("list jobs for run %d: %w", run.ID, err)
			}
		}
		var runnerPage struct {
			Runners []struct {
				ID           int64 `json:"id"`
				Name, Status string
				Busy         bool
				Labels       []struct {
					Name string `json:"name"`
				} `json:"labels"`
			} `json:"runners"`
		}
		if err := o.pages(ctx, fmt.Sprintf("/repos/%s/%s/actions/runners?per_page=100", url.PathEscape(repo.Owner), url.PathEscape(repo.Name)), &runnerPage, func() {
			for _, r := range runnerPage.Runners {
				labels := make([]string, len(r.Labels))
				for i, l := range r.Labels {
					labels[i] = l.Name
				}
				s.runners[r.ID] = Runner{ID: r.ID, Repository: repo, Name: r.Name, Status: r.Status, Busy: r.Busy, Labels: labels}
			}
			runnerPage.Runners = nil
		}); err != nil {
			return nil, fmt.Errorf("list runners for %s/%s: %w", repo.Owner, repo.Name, err)
		}
	}
	sort.Slice(s.queued, func(i, j int) bool { return s.queued[i] < s.queued[j] })
	return s, nil
}

var activeRunStatuses = [...]string{"queued", "in_progress", "pending", "waiting", "requested"}

func activeRun(status string) bool {
	for _, candidate := range activeRunStatuses {
		if status == candidate {
			return true
		}
	}
	return false
}

func (o *Observer) pages(ctx context.Context, path string, target any, consume func()) error {
	next, _ := o.resolve(path) // all call sites use fixed, already-valid URL templates
	for next != nil {
		token, err := o.tokens.Token(ctx)
		if err != nil {
			return fmt.Errorf("authenticate GitHub App: %w", err)
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, next.String(), nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		resp, err := o.http.Do(req)
		if err != nil {
			return err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			return ClassifyResponse(resp, o.clock.Now(), nil)
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(target)
		closeErr := resp.Body.Close()
		if err != nil {
			return fmt.Errorf("decode GitHub response: %w", err)
		}
		if closeErr != nil {
			return fmt.Errorf("close GitHub response: %w", closeErr)
		}
		consume()
		nextURL, err := o.next(resp.Header.Get("Link"))
		if err != nil {
			return err
		}
		next = nextURL
	}
	return nil
}

func (o *Observer) resolve(path string) (*url.URL, error) {
	u, err := url.Parse(path)
	if err != nil {
		return nil, err
	}
	if !u.IsAbs() && strings.HasPrefix(u.Path, "/") && o.base.Path != "" && o.base.Path != "/" &&
		!strings.HasPrefix(u.Path, strings.TrimRight(o.base.Path, "/")+"/") {
		u.Path = strings.TrimRight(o.base.Path, "/") + u.Path
	}
	return o.base.ResolveReference(u), nil
}
func (o *Observer) next(link string) (*url.URL, error) {
	for _, part := range strings.Split(link, ",") {
		bits := strings.Split(part, ";")
		if len(bits) < 2 || !strings.Contains(bits[1], `rel="next"`) {
			continue
		}
		raw := strings.Trim(strings.TrimSpace(bits[0]), "<>")
		u, err := o.resolve(raw)
		if err != nil {
			return nil, err
		}
		if u.Scheme != o.base.Scheme || u.Host != o.base.Host {
			return nil, errors.New("pagination URL escaped GitHub API origin")
		}
		return u, nil
	}
	return nil, nil
}
