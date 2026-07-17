package githubscaleset

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type fixedClock time.Time

func (c fixedClock) Now() time.Time { return time.Time(c) }

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }
func response(status int, body, link string) *http.Response {
	h := http.Header{}
	if link != "" {
		h.Set("Link", link)
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body))}
}

type closeErrorBody struct{ io.Reader }

func (closeErrorBody) Close() error { return errors.New("close response") }

func TestObserverPaginatesIndexesAndPreservesQueuedSibling(t *testing.T) {
	seen := map[string]int{}
	deadline := false
	auth := false
	doer := doerFunc(func(r *http.Request) (*http.Response, error) {
		seen[r.URL.Path+"?"+r.URL.RawQuery]++
		_, deadline = r.Context().Deadline()
		auth = r.Header.Get("Authorization") == "Bearer app-token" && r.Header.Get("X-GitHub-Api-Version") != ""
		switch r.URL.Path + "?" + r.URL.RawQuery {
		case "/repos/o/r/actions/runs?per_page=100":
			return response(200, `{"workflow_runs":[{"id":10,"run_attempt":3,"status":"in_progress","created_at":"2026-07-17T09:47:50Z"}]}`, `<https://api.test/repos/o/r/actions/runs?page=2&per_page=100>; rel="next"`), nil
		case "/repos/o/r/actions/runs?page=2&per_page=100":
			return response(200, `{"workflow_runs":[{"id":11,"status":"completed"}]}`, ""), nil
		case "/repos/o/r/actions/runs/10/jobs?filter=all&per_page=100":
			return response(200, `{"jobs":[{"id":101,"name":"running","status":"in_progress","labels":["self-hosted"],"started_at":"2026-07-17T09:49:00Z"},{"id":102,"name":"sibling","status":"queued","labels":["macos"],"started_at":"2026-07-17T09:47:52Z"}]}`, `</repos/o/r/actions/runs/10/jobs?filter=all&page=2&per_page=100>; rel="next"`), nil
		case "/repos/o/r/actions/runs/10/jobs?filter=all&page=2&per_page=100":
			return response(200, `{"jobs":[{"id":103,"name":"waiting","status":"waiting"}]}`, ""), nil
		case "/repos/o/r/actions/runners?per_page=100":
			return response(200, `{"runners":[{"id":201,"name":"tart","status":"online","busy":true,"labels":[{"name":"arm64"}]}]}`, `</repos/o/r/actions/runners?page=2&per_page=100>; rel="next"`), nil
		case "/repos/o/r/actions/runners?page=2&per_page=100":
			return response(200, `{"runners":[{"id":202,"name":"idle","status":"offline","busy":false}]}`, ""), nil
		default:
			t.Fatalf("unexpected request %s", r.URL.String())
			return nil, nil
		}
	})
	o, err := NewObserver(ObserverConfig{BaseURL: "https://api.test", Repositories: []Repository{{Owner: "o", Name: "r"}}, HTTP: doer, Tokens: TokenSourceFunc(func(context.Context) (string, error) { return "app-token", nil }), Clock: fixedClock(time.Unix(123, 0)), Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	obs := o.Refresh(context.Background(), nil)
	if obs.Freshness != Fresh || obs.Err != nil || !deadline || !auth {
		t.Fatalf("observation: %+v deadline=%v auth=%v", obs, deadline, auth)
	}
	if len(seen) != 6 {
		t.Fatalf("pagination missed: %v", seen)
	}
	queued := obs.Snapshot.QueuedJobs()
	if len(queued) != 2 || queued[0].ID != 102 || queued[1].ID != 103 {
		t.Fatalf("queued siblings lost: %+v", queued)
	}
	run, _ := obs.Snapshot.Run(10)
	if run.Status != "in_progress" || run.Attempt != 3 || !run.CreatedAt.Equal(time.Date(2026, 7, 17, 9, 47, 50, 0, time.UTC)) {
		t.Fatal("run index")
	}
	job, _ := obs.Snapshot.Job(102)
	if job.RunID != 10 || job.RunAttempt != 3 || !job.CreatedAt.Equal(time.Date(2026, 7, 17, 9, 47, 52, 0, time.UTC)) {
		t.Fatal("job index")
	}
	waiting, _ := obs.Snapshot.Job(103)
	if !waiting.CreatedAt.Equal(run.CreatedAt) {
		t.Fatalf("missing job timestamp did not fall back to run creation: %#v", waiting)
	}
	runner, _ := obs.Snapshot.Runner(201)
	if !runner.Busy || runner.Labels[0] != "arm64" {
		t.Fatal("runner index")
	}
}

func TestObserverFreshnessAndFailureModes(t *testing.T) {
	now := fixedClock(time.Unix(2, 0))
	repo := []Repository{{Owner: "o", Name: "r"}}
	newWith := func(d Doer, tokens TokenSource) (*Observer, error) {
		return NewObserver(ObserverConfig{BaseURL: "https://api.test", Repositories: repo, HTTP: d, Tokens: tokens, Clock: now, Timeout: time.Millisecond})
	}
	t.Run("unavailable and stale", func(t *testing.T) {
		o, _ := newWith(doerFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("down") }), TokenSourceFunc(func(context.Context) (string, error) { return "t", nil }))
		first := o.Refresh(context.Background(), nil)
		if first.Freshness != Unavailable || first.Snapshot != nil || first.Err == nil {
			t.Fatal("not unavailable")
		}
		prior := &Snapshot{}
		second := o.Refresh(context.Background(), prior)
		if second.Freshness != Stale || second.Snapshot != prior {
			t.Fatal("did not preserve prior snapshot")
		}
	})
	t.Run("auth", func(t *testing.T) {
		o, _ := newWith(doerFunc(func(*http.Request) (*http.Response, error) { t.Fatal("HTTP called"); return nil, nil }), TokenSourceFunc(func(context.Context) (string, error) { return "", errors.New("app") }))
		if o.Refresh(context.Background(), nil).Freshness != Unavailable {
			t.Fatal()
		}
	})
	t.Run("status", func(t *testing.T) {
		o, _ := newWith(doerFunc(func(*http.Request) (*http.Response, error) {
			r := response(429, "rate", "")
			r.Header.Set("Retry-After", "2")
			return r, nil
		}), TokenSourceFunc(func(context.Context) (string, error) { return "t", nil }))
		obs := o.Refresh(context.Background(), nil)
		var api *APIError
		if !errors.As(obs.Err, &api) || api.Kind != RateLimited || api.RetryAfter != 2*time.Second {
			t.Fatalf("%v", obs.Err)
		}
	})
	t.Run("decode", func(t *testing.T) {
		o, _ := newWith(doerFunc(func(*http.Request) (*http.Response, error) { return response(200, "{", ""), nil }), TokenSourceFunc(func(context.Context) (string, error) { return "t", nil }))
		if o.Refresh(context.Background(), nil).Err == nil {
			t.Fatal()
		}
	})
}

func TestObserverValidationPaginationSafetyAndStatuses(t *testing.T) {
	goodHTTP := doerFunc(func(*http.Request) (*http.Response, error) { return response(200, `{"workflow_runs":[]}`, ""), nil })
	goodToken := TokenSourceFunc(func(context.Context) (string, error) { return "t", nil })
	for _, c := range []ObserverConfig{{BaseURL: "://", HTTP: goodHTTP, Tokens: goodToken}, {BaseURL: "https://api.test", Tokens: goodToken}, {BaseURL: "https://api.test", HTTP: goodHTTP}} {
		if _, err := NewObserver(c); err == nil {
			t.Fatal("validation accepted")
		}
	}
	o, _ := NewObserver(ObserverConfig{BaseURL: "https://api.test", HTTP: goodHTTP, Tokens: goodToken})
	if activeRun("completed") || !activeRun("queued") || !activeRun("requested") || !activeRun("pending") {
		t.Fatal("active status")
	}
	if _, err := o.next(`<https://evil.test/x>; rel="next"`); err == nil {
		t.Fatal("cross-origin pagination")
	}
	if _, err := o.next(`<http://[::1>; rel="next"`); err == nil {
		t.Fatal("malformed pagination")
	}
	if n, err := o.next(`<https://api.test/x>; rel="last"`); err != nil || n != nil {
		t.Fatal("non-next link")
	}
	if _, err := o.resolve("://"); err == nil {
		t.Fatal("malformed path")
	}
	if (realClock{}).Now().IsZero() {
		t.Fatal("real clock")
	}
	enterprise, _ := NewObserver(ObserverConfig{BaseURL: "https://github.example/api/v3", HTTP: goodHTTP, Tokens: goodToken})
	if resolved, err := enterprise.resolve("/repos/o/r/actions/runs"); err != nil || resolved.Path != "/api/v3/repos/o/r/actions/runs" {
		t.Fatalf("enterprise API path = %v, %v", resolved, err)
	}
	if resolved, err := enterprise.next(`<https://github.example/api/v3/repos/o/r/actions/runs?page=2>; rel="next"`); err != nil ||
		resolved.Path != "/api/v3/repos/o/r/actions/runs" {
		t.Fatalf("enterprise pagination = %v, %v", resolved, err)
	}
}

func TestObserverWrapsJobsRunnersAndPaginationErrors(t *testing.T) {
	token := TokenSourceFunc(func(context.Context) (string, error) { return "t", nil })
	tests := []struct {
		name string
		do   Doer
	}{
		{"jobs", doerFunc(func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Path, "/runs/1/jobs") {
				return response(500, "boom", ""), nil
			}
			return response(200, `{"workflow_runs":[{"id":1,"status":"queued"}]}`, ""), nil
		})},
		{"runners", doerFunc(func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Path, "/runners") {
				return response(500, "boom", ""), nil
			}
			return response(200, `{"workflow_runs":[]}`, ""), nil
		})},
		{"pagination", doerFunc(func(*http.Request) (*http.Response, error) {
			return response(200, `{"workflow_runs":[]}`, `<https://evil.test/x>; rel="next"`), nil
		})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o, err := NewObserver(ObserverConfig{BaseURL: "https://api.test", Repositories: []Repository{{Owner: "o", Name: "r"}}, HTTP: tt.do, Tokens: token})
			if err != nil {
				t.Fatal(err)
			}
			if o.Refresh(context.Background(), nil).Err == nil {
				t.Fatal("expected wrapped endpoint error")
			}
		})
	}
}

func TestObserverRejectsResponseThatCannotBeClosed(t *testing.T) {
	doer := doerFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: closeErrorBody{Reader: strings.NewReader(`{"workflow_runs":[]}`)}}, nil
	})
	observer, err := NewObserver(ObserverConfig{BaseURL: "https://api.test", Repositories: []Repository{{Owner: "o", Name: "r"}}, HTTP: doer,
		Tokens: TokenSourceFunc(func(context.Context) (string, error) { return "token", nil })})
	if err != nil {
		t.Fatal(err)
	}
	observation := observer.Refresh(context.Background(), nil)
	if observation.Err == nil || !strings.Contains(observation.Err.Error(), "close GitHub response") {
		t.Fatalf("Refresh() error = %v", observation.Err)
	}
}
