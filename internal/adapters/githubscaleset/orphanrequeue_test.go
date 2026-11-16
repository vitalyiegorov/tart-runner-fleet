package githubscaleset

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

type actionToken struct{ write bool }

func (actionToken) Token(context.Context) (string, error)        { return "test", nil }
func (t actionToken) ActionsWrite(context.Context) (bool, error) { return t.write, nil }
func TestIssue520FreshRunGatesAndCancelBeforeRerun(t *testing.T) {
	for _, scenario := range []string{"act", "observe", "running", "waiting", "unknown", "excluded", "missing target", "new attempt", "completed", "forbidden", "no permission", "cancel conflict", "rerun conflict", "pagination running", "read failure", "missing name", "missing job name", "invalid candidate"} {
		t.Run(scenario, func(t *testing.T) {
			posts := []string{}
			cancelled := false
			observer, err := NewObserver(ObserverConfig{BaseURL: "https://api.test", Tokens: actionToken{write: scenario != "no permission"}, HTTP: doerFunc(func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodPost {
					posts = append(posts, req.URL.Path)
					if scenario == "forbidden" {
						return response(403, `{}`, ""), nil
					}
					if (scenario == "cancel conflict" && strings.HasSuffix(req.URL.Path, "cancel")) || (scenario == "rerun conflict" && strings.HasSuffix(req.URL.Path, "rerun-failed-jobs")) {
						return response(409, `{}`, ""), nil
					}
					cancelled = true
					return response(202, `{}`, ""), nil
				}
				if strings.HasSuffix(req.URL.Path, "/jobs") {
					status := "queued"
					switch scenario {
					case "running":
						status = "in_progress"
					case "waiting":
						status = "waiting"
					case "unknown":
						status = "pending"
					}
					name := "build"
					if scenario == "missing job name" {
						name = ""
					}
					id := 42
					if scenario == "missing target" {
						id = 43
					}
					link := ""
					if scenario == "pagination running" {
						if req.URL.Query().Get("page") == "2" {
							status = "in_progress"
						} else {
							link = `</repos/owner/repo/actions/runs/420/jobs?page=2>; rel="next"`
						}
					}
					return response(200, fmt.Sprintf(`{"jobs":[{"id":%d,"name":%q,"status":%q,"created_at":"2020-01-01T00:00:00Z"}]}`, id, name, status), link), nil
				}
				if scenario == "read failure" {
					return response(500, `{}`, ""), nil
				}
				status := "queued"
				if cancelled || scenario == "completed" {
					status = "completed"
				}
				attempt := 1
				if scenario == "new attempt" {
					attempt = 2
				}
				name := "farm"
				if scenario == "missing name" {
					name = ""
				}
				return response(200, fmt.Sprintf(`{"id":420,"name":%q,"conclusion":"cancelled","run_attempt":%d,"status":%q,"created_at":"2020-01-01T00:00:00Z"}`, name, attempt, status), ""), nil
			})})
			if err != nil {
				t.Fatal(err)
			}
			job := WorkflowJob{OrphanBefore: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), ID: 42, RunID: 420, RunAttempt: 1, Repository: Repository{Owner: "owner", Name: "repo"}}
			if scenario == "invalid candidate" {
				job.ID = 0
			}
			outcome, err := observer.RequeueOrphan(context.Background(), job, func(w string, _ []string) bool { return scenario == "excluded" && w == "farm" }, scenario != "observe")
			if (err != nil) != (scenario == "read failure") {
				t.Fatalf("outcome=%s err=%v", outcome, err)
			}
			want := 0
			switch scenario {
			case "act", "rerun conflict":
				want = 2
			case "forbidden", "cancel conflict":
				want = 1
			}
			if len(posts) != want {
				t.Fatalf("posts=%v outcome=%s", posts, outcome)
			}
			if want == 2 && (!strings.HasSuffix(posts[0], "/cancel") || !strings.HasSuffix(posts[1], "/rerun-failed-jobs")) {
				t.Fatal(posts)
			}
			if scenario == "excluded" && outcome != "excluded" {
				t.Fatal(outcome)
			}
			if scenario == "act" && outcome != "rerun" {
				t.Fatal(outcome)
			}
			if (scenario == "forbidden" || scenario == "no permission") && outcome != "permission_denied" {
				t.Fatal(outcome)
			}
		})
	}
}

type stagedActionToken struct {
	calls           int
	failAt          int
	permissionError bool
}

func (t *stagedActionToken) Token(context.Context) (string, error) {
	t.calls++
	if t.calls == t.failAt {
		return "", context.Canceled
	}
	return "test", nil
}
func (t *stagedActionToken) ActionsWrite(context.Context) (bool, error) {
	if t.permissionError {
		return false, context.Canceled
	}
	return true, nil
}
func TestOrphanRequeuePartialFailuresNeverProceed(t *testing.T) {
	for _, failure := range []string{"jobs", "confirm", "confirm attempt", "cancel", "poll", "poll malformed", "poll attempt", "rerun", "cancel transport", "cancel token", "unsupported token", "permission check", "poll canceled"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reads := 0
			posts := 0
			tokens := &stagedActionToken{}
			if failure == "cancel token" {
				tokens.failAt = 5
			}
			tokens.permissionError = failure == "permission check"
			var source TokenSource = tokens
			if failure == "unsupported token" {
				source = TokenSourceFunc(func(context.Context) (string, error) { return "test", nil })
			}
			observer, err := NewObserver(ObserverConfig{BaseURL: "https://api.test", Tokens: source, HTTP: doerFunc(func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodPost {
					posts++
					if (failure == "cancel" && posts == 1) || (failure == "rerun" && posts == 2) {
						return response(500, `{}`, ""), nil
					}
					if failure == "cancel transport" {
						return nil, context.Canceled
					}
					return response(202, `{}`, ""), nil
				}
				if strings.HasSuffix(req.URL.Path, "/jobs") {
					if failure == "jobs" {
						return response(500, `{}`, ""), nil
					}
					return response(200, `{"jobs":[{"id":42,"name":"build","status":"queued","created_at":"2020-01-01T00:00:00Z"}]}`, ""), nil
				}
				reads++
				if (failure == "confirm" && reads == 2) || (failure == "poll" && reads == 3) {
					return response(500, `{}`, ""), nil
				}
				if failure == "poll malformed" && reads == 3 {
					return response(200, `{`, ""), nil
				}
				attempt := 1
				if (failure == "confirm attempt" && reads == 2) || (failure == "poll attempt" && reads == 3) {
					attempt = 2
				}
				status := "queued"
				if posts > 0 {
					status = "completed"
				}
				if failure == "poll canceled" && reads == 3 {
					cancel()
					status = "completed"
				}
				return response(200, fmt.Sprintf(`{"id":420,"name":"build","conclusion":"cancelled","run_attempt":%d,"status":%q,"created_at":"2020-01-01T00:00:00Z"}`, attempt, status), ""), nil
			})})
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := observer.RequeueOrphan(ctx, WorkflowJob{OrphanBefore: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), ID: 42, RunID: 420, RunAttempt: 1, Repository: Repository{Owner: "owner", Name: "repo"}}, func(string, []string) bool { return false }, true)
			noop := failure == "confirm attempt" || failure == "poll attempt" || failure == "unsupported token" || failure == "poll canceled"
			if (err == nil) != noop {
				t.Fatalf("failure=%s outcome=%s err=%v", failure, outcome, err)
			}
			if (failure == "cancel" || failure == "cancel transport") && outcome != "cancelled_not_rerun" {
				t.Fatalf("uncertain cancel lost: %s", outcome)
			}
			if failure != "rerun" && failure != "poll canceled" && posts > 1 {
				t.Fatalf("proceeded to rerun after failure: %d", posts)
			}
		})
	}
}

func TestOrphanFinalEvidenceAndResume(t *testing.T) {
	for _, scenario := range []string{"run transition", "job transition", "young sibling", "cancelled context", "poll error", "rerun conflict", "secondary limit", "resume", "resume live", "final read failure"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reads, lists, posts := 0, 0, 0
			o, _ := NewObserver(ObserverConfig{BaseURL: "https://api.test", Tokens: actionToken{true}, HTTP: doerFunc(func(req *http.Request) (*http.Response, error) {
				if req.Header.Get("If-None-Match") != "" {
					t.Fatal("recovery reused ETag")
				}
				if err := req.Context().Err(); err != nil {
					return nil, err
				}
				if req.Method == http.MethodPost {
					posts++
					if scenario == "secondary limit" {
						r := orphanFreshResponse(403, "{}", "")
						r.Header.Set("Retry-After", "90")
						return r, nil
					}
					if posts == 1 && scenario == "cancelled context" {
						cancel()
					}
					if posts == 2 && scenario == "rerun conflict" {
						return orphanFreshResponse(409, "{}", ""), nil
					}
					return orphanFreshResponse(202, "{}", ""), nil
				}
				if strings.HasSuffix(req.URL.Path, "/jobs") {
					lists++
					if scenario == "final read failure" && lists == 2 {
						return response(500, "{}", ""), nil
					}
					status := "queued"
					if scenario == "job transition" && lists == 2 {
						status = "in_progress"
					}
					body := fmt.Sprintf(`{"jobs":[{"id":42,"name":"build","status":%q,"created_at":"2020-01-01T00:00:00Z"}`, status)
					if scenario == "young sibling" {
						body += fmt.Sprintf(`,{"id":43,"name":"hosted","status":"queued","created_at":%q}`, time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC).Add(-time.Minute).Format(time.RFC3339))
					}
					return orphanFreshResponse(200, body+"]}", ""), nil
				}
				reads++
				status := "queued"
				if scenario == "run transition" && reads == 2 {
					status = "in_progress"
				}
				if posts > 0 || scenario == "resume" {
					status = "completed"
				}
				if posts > 0 && scenario == "poll error" {
					return orphanFreshResponse(500, "{}", ""), nil
				}
				return orphanFreshResponse(200, fmt.Sprintf(`{"id":420,"name":"build","status":%q,"conclusion":"cancelled","run_attempt":1}`, status), ""), nil
			})})
			job := WorkflowJob{OrphanBefore: time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC).Add(-15 * time.Minute), ID: 42, RunID: 420, RunAttempt: 1, Repository: Repository{Owner: "o", Name: "r"}}
			if scenario == "resume" || scenario == "resume live" {
				job.ResumeOrphan = true
			}
			outcome, err := o.RequeueOrphan(ctx, job, func(string, []string) bool { return false }, true)
			switch scenario {
			case "final read failure":
				if err == nil || posts != 0 {
					t.Fatalf("missing final observation allowed cancel: %s %v", outcome, err)
				}
			case "run transition", "job transition", "young sibling", "resume live":
				if posts != 0 {
					t.Fatalf("unsafe posts=%d", posts)
				}
			case "cancelled context", "resume":
				if outcome != "rerun" || err != nil {
					t.Fatalf("%s %v", outcome, err)
				}
			case "poll error", "rerun conflict":
				if outcome != "cancelled_not_rerun" {
					t.Fatalf("%s %v", outcome, err)
				}
			case "secondary limit":
				if outcome == "permission_denied" || err == nil {
					t.Fatalf("%s %v", outcome, err)
				}
			}
		})
	}
}

func orphanFreshResponse(status int, body, link string) *http.Response {
	r := response(status, body, link)
	r.Header.Set("ETag", "test-etag")
	return r
}

func TestOrphanResumeRejectsUncertainOrActiveAttempt(t *testing.T) {
	for _, scenario := range []string{"wrong run", "older attempt", "new attempt", "success conclusion", "unknown state", "observe"} {
		t.Run(scenario, func(t *testing.T) {
			posts := 0
			o, _ := NewObserver(ObserverConfig{BaseURL: "https://api.test", Tokens: actionToken{true}, HTTP: doerFunc(func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodPost {
					posts++
					return response(202, "{}", ""), nil
				}
				id, attempt, status, conclusion := 420, 2, "completed", "cancelled"
				switch scenario {
				case "wrong run":
					id = 421
				case "older attempt":
					attempt = 1
				case "new attempt":
					attempt = 3
				case "success conclusion":
					conclusion = "success"
				case "unknown state":
					status = "unknown"
				}
				return response(200, fmt.Sprintf(`{"id":%d,"run_attempt":%d,"status":%q,"conclusion":%q}`, id, attempt, status, conclusion), ""), nil
			})})
			outcome, err := o.RequeueOrphan(context.Background(), WorkflowJob{ResumeOrphan: true, RunID: 420, RunAttempt: 2, Repository: Repository{Owner: "o", Name: "r"}}, func(string, []string) bool { return false }, scenario != "observe")
			if posts != 0 {
				t.Fatal("unsafe rerun")
			}
			want := "noop"
			if scenario == "wrong run" || scenario == "older attempt" || scenario == "unknown state" {
				want = "cancelled_not_rerun"
			}
			if scenario == "observe" {
				want = "held"
			}
			if outcome != want || (err != nil) != (scenario == "wrong run" || scenario == "older attempt") {
				t.Fatalf("%s %v", outcome, err)
			}
		})
	}
}

// A continuation deadline expiring after a successful poll must retain the
// reservation and must not turn a still-queued run into a rerun.
func TestOrphanContinuationDeadlineRetainsRecovery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	posts := 0
	o, _ := NewObserver(ObserverConfig{BaseURL: "https://api.test", Tokens: actionToken{true}, HTTP: doerFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPost {
			posts++
			return response(202, "{}", ""), nil
		}
		cancel()
		return response(200, `{"id":420,"run_attempt":1,"status":"queued"}`, ""), nil
	})})
	outcome, err := o.pollOrphan(ctx, "/repos/o/r/actions/runs/420", WorkflowJob{RunID: 420, RunAttempt: 1})
	if outcome != "cancelled_not_rerun" || !errors.Is(err, context.Canceled) || posts != 0 {
		t.Fatalf("%s %v posts=%d", outcome, err, posts)
	}
}
