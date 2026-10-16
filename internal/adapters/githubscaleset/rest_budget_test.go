package githubscaleset

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// Issue #377: both Macs share one installation's 5,000/h budget, and the REST
// inventory spent it ~45 minutes into every hour. GitHub answers the exhausted
// primary limit with 403 (not 429), no Retry-After, and X-RateLimit-Remaining
// 0 beside the epoch reset. That must classify as a rate limit whose retry
// horizon is the reset, never as an authorization failure to hammer.
func TestClassifyResponseTreatsExhaustedPrimaryLimitAsRateLimited(t *testing.T) {
	now := time.Date(2026, 10, 16, 13, 0, 0, 0, time.UTC)
	reset := now.Add(90 * time.Second)
	limited := &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{}}
	limited.Header.Set("X-RateLimit-Remaining", "0")
	limited.Header.Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
	var api *APIError
	if err := ClassifyResponse(limited, now, nil); !errors.As(err, &api) || api.Kind != RateLimited || api.RetryAfter != 90*time.Second {
		t.Fatalf("exhausted primary limit = %#v", api)
	}
	secondary := &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}}
	secondary.Header.Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
	if err := ClassifyResponse(secondary, now, nil); !errors.As(err, &api) || api.Kind != RateLimited || api.RetryAfter != 90*time.Second {
		t.Fatalf("429 without Retry-After = %#v", api)
	}
	explicit := &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}}
	explicit.Header.Set("Retry-After", "5")
	explicit.Header.Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
	if err := ClassifyResponse(explicit, now, nil); !errors.As(err, &api) || api.RetryAfter != 5*time.Second {
		t.Fatalf("Retry-After must win over the window reset: %#v", api)
	}
	forbidden := &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{}}
	forbidden.Header.Set("X-RateLimit-Remaining", "17")
	forbidden.Header.Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
	if err := ClassifyResponse(forbidden, now, nil); !errors.As(err, &api) || api.Kind != Authorization || api.RetryAfter != 0 {
		t.Fatalf("403 with budget left stays authorization: %#v", api)
	}
	past := &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{}}
	past.Header.Set("X-RateLimit-Remaining", "0")
	past.Header.Set("X-RateLimit-Reset", strconv.FormatInt(now.Add(-time.Second).Unix(), 10))
	if err := ClassifyResponse(past, now, nil); !errors.As(err, &api) || api.Kind != RateLimited || api.RetryAfter != 0 {
		t.Fatalf("elapsed reset carries no wait: %#v", api)
	}
}

// A refresh that revalidates every page with the ETag GitHub returned costs
// nothing against the primary limit when the page is unchanged (304), so the
// three usually-empty status lists and every settled run stop being billed.
// The observer must send If-None-Match, rebuild the page from its cache on 304,
// keep paginating from the cached Link, and report the budget headers so the
// caller can pace itself. A page that leaves the inventory is forgotten.
func TestObserverRevalidatesWithETagAndReportsBudget(t *testing.T) {
	now := time.Date(2026, 10, 16, 13, 0, 0, 0, time.UTC)
	reset := now.Add(17 * time.Minute)
	pages := map[string]struct{ body, link string }{
		"/repos/o/r/actions/runs?status=in_progress&per_page=100":        {`{"workflow_runs":[{"id":10,"run_attempt":1,"status":"in_progress","created_at":"2026-10-16T12:00:00Z"}]}`, `<https://api.test/repos/o/r/actions/runs?status=in_progress&page=2&per_page=100>; rel="next"`},
		"/repos/o/r/actions/runs?status=in_progress&page=2&per_page=100": {`{"workflow_runs":[{"id":11,"run_attempt":1,"status":"in_progress","created_at":"2026-10-16T12:01:00Z"}]}`, ""},
		"/repos/o/r/actions/runs?status=queued&per_page=100":             {`{"workflow_runs":[]}`, ""},
		"/repos/o/r/actions/runs?status=pending&per_page=100":            {`{"workflow_runs":[]}`, ""},
		"/repos/o/r/actions/runs?status=waiting&per_page=100":            {`{"workflow_runs":[]}`, ""},
		"/repos/o/r/actions/runs?status=requested&per_page=100":          {`{"workflow_runs":[]}`, ""},
		"/repos/o/r/actions/runs/10/jobs?filter=latest&per_page=100":     {`{"jobs":[{"id":101,"name":"build","status":"queued","labels":["self-hosted"],"created_at":"2026-10-16T12:00:01Z"}]}`, ""},
		"/repos/o/r/actions/runs/11/jobs?filter=latest&per_page=100":     {`{"jobs":[{"id":111,"name":"test","status":"in_progress","labels":["self-hosted"],"created_at":"2026-10-16T12:01:01Z"}]}`, ""},
	}
	gone := false
	billed, revalidated := 0, 0
	conditional := map[string]string{}
	doer := doerFunc(func(r *http.Request) (*http.Response, error) {
		key := r.URL.Path + "?" + r.URL.RawQuery
		page, ok := pages[key]
		if !ok {
			t.Fatalf("unexpected request %s", key)
		}
		changed := gone && key == "/repos/o/r/actions/runs?status=in_progress&page=2&per_page=100"
		if changed {
			page = struct{ body, link string }{`{"workflow_runs":[]}`, ""}
		}
		etag := `W/"` + key + `"`
		var resp *http.Response
		if r.Header.Get("If-None-Match") == etag && !changed {
			conditional[key] = r.Header.Get("If-None-Match")
			revalidated++
			resp = response(http.StatusNotModified, "", "")
		} else {
			billed++
			resp = response(http.StatusOK, page.body, page.link)
		}
		resp.Header.Set("ETag", etag)
		resp.Header.Set("X-RateLimit-Limit", "5000")
		resp.Header.Set("X-RateLimit-Remaining", strconv.Itoa(4000-billed))
		resp.Header.Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		return resp, nil
	})
	o, err := NewObserver(ObserverConfig{BaseURL: "https://api.test", Repositories: []Repository{{Owner: "o", Name: "r"}}, HTTP: doer,
		Tokens: TokenSourceFunc(func(context.Context) (string, error) { return "t", nil }), Clock: fixedClock(now), Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	first := o.Refresh(context.Background(), nil)
	if first.Err != nil || first.Freshness != Fresh || billed != len(pages) || revalidated != 0 {
		t.Fatalf("first refresh = %+v billed=%d revalidated=%d", first, billed, revalidated)
	}
	if first.Budget != (Budget{Limit: 5000, Remaining: 4000 - len(pages), Reset: reset, Observed: true}) {
		t.Fatalf("budget = %+v", first.Budget)
	}
	second := o.Refresh(context.Background(), first.Snapshot)
	if second.Err != nil || second.Freshness != Fresh || billed != len(pages) || revalidated != len(pages) {
		t.Fatalf("second refresh = %+v billed=%d revalidated=%d conditional=%v", second, billed, revalidated, conditional)
	}
	if len(second.Snapshot.QueuedJobs()) != 1 || second.Snapshot.QueuedJobs()[0].ID != 101 {
		t.Fatalf("cached pages must rebuild the same inventory: %+v", second.Snapshot.QueuedJobs())
	}
	if job, ok := second.Snapshot.Job(111); !ok || job.RunID != 11 {
		t.Fatal("cached pagination link was not followed")
	}
	if second.Snapshot == first.Snapshot {
		t.Fatal("a revalidated refresh is a new observation, not the prior pointer")
	}
	gone = true
	third := o.Refresh(context.Background(), second.Snapshot)
	if third.Err != nil || billed != len(pages)+1 {
		t.Fatalf("changed page must be billed once: %+v billed=%d", third, billed)
	}
	if _, ok := third.Snapshot.Run(11); ok {
		t.Fatal("vanished run survived")
	}
	if _, cached := o.cache["/repos/o/r/actions/runs/11/jobs?filter=latest&per_page=100"]; cached {
		t.Fatal("a page that left the inventory must be forgotten")
	}
	if !third.Budget.Observed || third.Budget.Remaining != 4000-billed {
		t.Fatalf("budget must follow the latest response: %+v", third.Budget)
	}
}

// An error response still carries the budget, and a transport failure
// carries none: the caller must never read a zero budget as an empty one.
func TestObserverBudgetSurvivesErrorsAndStaysUnobservedWithoutHeaders(t *testing.T) {
	now := time.Date(2026, 10, 16, 13, 0, 0, 0, time.UTC)
	reset := now.Add(12 * time.Minute)
	limited := doerFunc(func(*http.Request) (*http.Response, error) {
		resp := response(http.StatusForbidden, "limit", "")
		resp.Header.Set("X-RateLimit-Limit", "5000")
		resp.Header.Set("X-RateLimit-Remaining", "0")
		resp.Header.Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		return resp, nil
	})
	tokens := TokenSourceFunc(func(context.Context) (string, error) { return "t", nil })
	o, _ := NewObserver(ObserverConfig{BaseURL: "https://api.test", Repositories: []Repository{{Owner: "o", Name: "r"}}, HTTP: limited, Tokens: tokens, Clock: fixedClock(now)})
	obs := o.Refresh(context.Background(), nil)
	var api *APIError
	if !errors.As(obs.Err, &api) || api.Kind != RateLimited || obs.Budget != (Budget{Limit: 5000, Remaining: 0, Reset: reset, Observed: true}) {
		t.Fatalf("limited observation = %+v err=%v", obs.Budget, obs.Err)
	}
	if !obs.Budget.Scarce() {
		t.Fatal("an empty budget is scarce")
	}
	bare := doerFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, `{"workflow_runs":[]}`, ""), nil
	})
	o, _ = NewObserver(ObserverConfig{BaseURL: "https://api.test", Repositories: []Repository{{Owner: "o", Name: "r"}}, HTTP: bare, Tokens: tokens, Clock: fixedClock(now)})
	if obs := o.Refresh(context.Background(), nil); obs.Err != nil || obs.Budget.Observed || obs.Budget.Scarce() {
		t.Fatalf("headerless response must leave the budget unobserved: %+v", obs.Budget)
	}
	partial := doerFunc(func(*http.Request) (*http.Response, error) {
		resp := response(http.StatusOK, `{"workflow_runs":[]}`, "")
		resp.Header.Set("X-RateLimit-Remaining", "12")
		return resp, nil
	})
	o, _ = NewObserver(ObserverConfig{BaseURL: "https://api.test", Repositories: []Repository{{Owner: "o", Name: "r"}}, HTTP: partial, Tokens: tokens, Clock: fixedClock(now)})
	if obs := o.Refresh(context.Background(), nil); obs.Budget.Observed {
		t.Fatalf("a budget without a reset is not observed: %+v", obs.Budget)
	}
	for _, c := range []struct {
		budget Budget
		scarce bool
	}{
		{Budget{Limit: 5000, Remaining: 500, Reset: reset, Observed: true}, true},
		{Budget{Limit: 5000, Remaining: 501, Reset: reset, Observed: true}, false},
		{Budget{Limit: 0, Remaining: 0, Reset: reset, Observed: true}, true},
		{Budget{Limit: 0, Remaining: 3, Reset: reset, Observed: true}, false},
	} {
		if c.budget.Scarce() != c.scarce {
			t.Fatalf("Scarce(%+v) = %v", c.budget, c.budget.Scarce())
		}
	}
}

// A refresh that fails part-way has not proved any page left the inventory, so
// the pages it never reached keep their validators. Otherwise the refresh that
// hits the exhausted limit throws away exactly the ETags that would make the
// first refresh after the reset cheap.
func TestObserverKeepsValidatorsWhenARefreshFailsPartWay(t *testing.T) {
	now := time.Date(2026, 10, 16, 13, 0, 0, 0, time.UTC)
	failing := ""
	doer := doerFunc(func(r *http.Request) (*http.Response, error) {
		key := r.URL.Path + "?" + r.URL.RawQuery
		if key == failing {
			return response(http.StatusBadGateway, "down", ""), nil
		}
		resp := response(http.StatusOK, `{"workflow_runs":[]}`, "")
		resp.Header.Set("ETag", `"`+key+`"`)
		return resp, nil
	})
	o, _ := NewObserver(ObserverConfig{BaseURL: "https://api.test", Repositories: []Repository{{Owner: "o", Name: "r"}}, HTTP: doer,
		Tokens: TokenSourceFunc(func(context.Context) (string, error) { return "t", nil }), Clock: fixedClock(now)})
	if obs := o.Refresh(context.Background(), nil); obs.Err != nil || len(o.cache) != len(activeRunStatuses) {
		t.Fatalf("first refresh = %v cached=%d", obs.Err, len(o.cache))
	}
	failing = "/repos/o/r/actions/runs?status=" + activeRunStatuses[0] + "&per_page=100"
	if obs := o.Refresh(context.Background(), nil); obs.Err == nil {
		t.Fatal("expected the failing page to fail the refresh")
	}
	if len(o.cache) != len(activeRunStatuses) {
		t.Fatalf("a failed refresh pruned validators: %d left of %d", len(o.cache), len(activeRunStatuses))
	}
}

// The budget belongs to the latest answer, so an answer without the headers
// and a refresh that got no answer both leave it unobserved rather than
// replaying a window GitHub reported earlier.
func TestObserverBudgetDoesNotOutliveTheResponseThatReportedIt(t *testing.T) {
	now := time.Date(2026, 10, 16, 13, 0, 0, 0, time.UTC)
	headers, down := true, false
	doer := doerFunc(func(*http.Request) (*http.Response, error) {
		if down {
			return nil, errors.New("down")
		}
		resp := response(http.StatusOK, `{"workflow_runs":[]}`, "")
		if headers {
			resp.Header.Set("X-RateLimit-Limit", "5000")
			resp.Header.Set("X-RateLimit-Remaining", "3")
			resp.Header.Set("X-RateLimit-Reset", strconv.FormatInt(now.Add(time.Hour).Unix(), 10))
		}
		return resp, nil
	})
	o, _ := NewObserver(ObserverConfig{BaseURL: "https://api.test", Repositories: []Repository{{Owner: "o", Name: "r"}}, HTTP: doer,
		Tokens: TokenSourceFunc(func(context.Context) (string, error) { return "t", nil }), Clock: fixedClock(now)})
	if obs := o.Refresh(context.Background(), nil); !obs.Budget.Scarce() {
		t.Fatalf("first refresh budget = %+v", obs.Budget)
	}
	headers = false
	if obs := o.Refresh(context.Background(), nil); obs.Budget.Observed {
		t.Fatalf("headerless answers replayed an old budget: %+v", obs.Budget)
	}
	headers, down = true, false
	o.Refresh(context.Background(), nil)
	down = true
	if obs := o.Refresh(context.Background(), nil); obs.Err == nil || obs.Budget.Observed {
		t.Fatalf("an unanswered refresh replayed an old budget: %+v err=%v", obs.Budget, obs.Err)
	}
}
