package githubscaleset

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Issue #449: during a budgie burst every node's REST inventory for that one
// repository went stale at the same moment. A refresh is five run-status
// listings plus one jobs listing per active run, issued in sequence, and the
// GitHub timeout bounded the whole pass. Twenty answers of ~0.75s each crossed
// the 15s limit, so every refresh died on its own deadline although GitHub
// answered every request promptly. The timeout bounds one request: a burst
// may make a refresh longer, never make it fail.
func TestObserverBoundsEachRequestNotTheWholeRefresh(t *testing.T) {
	const (
		timeout = time.Second
		answer  = 250 * time.Millisecond
		runs    = 3
	)
	requests := 0
	doer := doerFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		select {
		case <-time.After(answer):
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		if strings.HasSuffix(r.URL.Path, "/jobs") {
			run := strings.Split(r.URL.Path, "/")[6]
			return response(http.StatusOK, `{"jobs":[{"id":`+run+`,"status":"queued","labels":["macos"]}]}`, ""), nil
		}
		if r.URL.Query().Get("status") != "queued" {
			return response(http.StatusOK, `{"workflow_runs":[]}`, ""), nil
		}
		var listed []string
		for id := 1; id <= runs; id++ {
			listed = append(listed, fmt.Sprintf(`{"id":%d,"status":"queued"}`, id))
		}
		return response(http.StatusOK, `{"workflow_runs":[`+strings.Join(listed, ",")+`]}`, ""), nil
	})
	o, err := NewObserver(ObserverConfig{BaseURL: "https://api.test", Repositories: []Repository{{Owner: "o", Name: "r"}}, HTTP: doer,
		Tokens: TokenSourceFunc(func(context.Context) (string, error) { return "t", nil }), Timeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	want := len(activeRunStatuses) + runs
	if total := time.Duration(want) * answer; total <= timeout {
		t.Fatalf("fixture must outlast one timeout: %v of answers within %v", total, timeout)
	}
	obs := o.Refresh(context.Background(), nil)
	if obs.Err != nil || obs.Freshness != Fresh {
		t.Fatalf("a slow-but-answering refresh failed after %d requests: %v (%v)", requests, obs.Err, obs.Freshness)
	}
	if requests != want || len(obs.Snapshot.QueuedJobs()) != runs {
		t.Fatalf("requests = %d (want %d), queued = %d", requests, want, len(obs.Snapshot.QueuedJobs()))
	}
}

// A request GitHub never answers still ends at the timeout, so bounding each
// request keeps a hung connection from holding the inventory loop forever.
func TestObserverStillTimesOutAnUnansweredRequest(t *testing.T) {
	doer := doerFunc(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	o, err := NewObserver(ObserverConfig{BaseURL: "https://api.test", Repositories: []Repository{{Owner: "o", Name: "r"}}, HTTP: doer,
		Tokens: TokenSourceFunc(func(context.Context) (string, error) { return "t", nil }), Timeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if obs := o.Refresh(context.Background(), nil); obs.Err == nil || obs.Freshness != Unavailable {
		t.Fatalf("unanswered refresh = %+v", obs)
	}
}
