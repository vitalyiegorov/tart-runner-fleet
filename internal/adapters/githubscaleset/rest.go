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
	BaseURL                string
	Repositories           []Repository
	HTTP                   Doer
	Tokens                 TokenSource
	Clock                  Clock
	Timeout                time.Duration
	IncludeRunnerInventory bool
}

type Observer struct {
	base           *url.URL
	repos          []Repository
	http           Doer
	tokens         TokenSource
	clock          Clock
	timeout        time.Duration
	includeRunners bool

	// cache holds every page of the last inventory keyed by request URL so the
	// next refresh can revalidate it. GitHub does not bill a 304 against the
	// primary limit, and most pages are unchanged between two polls: the three
	// rarely-populated status lists and every run whose jobs have settled. Pages
	// absent from a refresh are dropped so the cache tracks the live inventory.
	// A refresh is a single sequential pass, and one observer serves one ingest
	// loop, so the cache needs no lock.
	cache   map[string]cachedPage
	touched map[string]struct{}
	budget  Budget
}

type cachedPage struct {
	etag string
	body []byte
	link string
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
	return &Observer{base: base, repos: append([]Repository(nil), c.Repositories...), http: c.HTTP, tokens: c.Tokens,
		clock: c.Clock, timeout: c.Timeout, includeRunners: c.IncludeRunnerInventory, cache: map[string]cachedPage{}}, nil
}

// Refresh takes one inventory. The timeout bounds each request, never the
// pass: a refresh is one listing per run status plus one per active run, issued
// in sequence, so its length grows with the queue. Bounding the whole pass made
// every node's inventory of a busy repository fail at the same moment during a
// burst although GitHub answered each request promptly (issue #449).
func (o *Observer) Refresh(ctx context.Context, previous *Snapshot) Observation {
	s, err := o.fetch(ctx)
	if err == nil {
		return Observation{Freshness: Fresh, Snapshot: s, Budget: o.budget}
	}
	if previous != nil {
		return Observation{Freshness: Stale, Snapshot: previous, Err: err, Budget: o.budget}
	}
	return Observation{Freshness: Unavailable, Err: err, Budget: o.budget}
}

func (o *Observer) fetch(ctx context.Context) (s *Snapshot, err error) {
	// The budget belongs to this refresh's answers alone: a refresh GitHub never
	// answered must not replay a window an earlier one reported.
	o.budget = Budget{}
	o.touched = make(map[string]struct{}, len(o.cache))
	defer func() {
		// Only a complete refresh proves a page left the inventory. One that
		// failed part-way keeps the validators of every page it never reached,
		// or the refresh that hit the exhausted limit would discard exactly the
		// ETags that make the first refresh after the reset cheap.
		if err == nil {
			for key := range o.cache {
				if _, kept := o.touched[key]; !kept {
					delete(o.cache, key)
				}
			}
		}
		o.touched = nil
	}()
	s = &Snapshot{at: o.clock.Now(), runs: map[int64]WorkflowRun{}, jobs: map[int64]WorkflowJob{}, runners: map[int64]Runner{}}
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
		if o.includeRunners {
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
		key := next.String()
		page, err := o.page(ctx, key)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(page.body, target); err != nil {
			return fmt.Errorf("decode GitHub response: %w", err)
		}
		if page.etag != "" {
			o.cache[key] = page
			if o.touched != nil {
				o.touched[key] = struct{}{}
			}
		}
		consume()
		nextURL, err := o.next(page.link)
		if err != nil {
			return err
		}
		next = nextURL
	}
	return nil
}

// page fetches one page within its own timeout, revalidating a cached copy.
func (o *Observer) page(ctx context.Context, key string) (cachedPage, error) {
	ctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	token, err := o.tokens.Token(ctx)
	if err != nil {
		return cachedPage{}, fmt.Errorf("authenticate GitHub App: %w", err)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, key, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	cached, revalidate := o.cache[key]
	if revalidate {
		req.Header.Set("If-None-Match", cached.etag)
	}
	resp, err := o.http.Do(req)
	if err != nil {
		return cachedPage{}, err
	}
	// Each answer replaces the budget, so a headerless one leaves it
	// unobserved instead of carrying an older window forward.
	o.budget = ParseBudget(resp.Header)
	switch {
	case revalidate && resp.StatusCode == http.StatusNotModified:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		if err := resp.Body.Close(); err != nil {
			return cachedPage{}, fmt.Errorf("close GitHub response: %w", err)
		}
		return cached, nil
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		return cachedPage{}, ClassifyResponse(resp, o.clock.Now(), nil)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	closeErr := resp.Body.Close()
	if err != nil {
		return cachedPage{}, fmt.Errorf("read GitHub response: %w", err)
	}
	if closeErr != nil {
		return cachedPage{}, fmt.Errorf("close GitHub response: %w", closeErr)
	}
	return cachedPage{etag: resp.Header.Get("ETag"), body: body, link: resp.Header.Get("Link")}, nil
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
