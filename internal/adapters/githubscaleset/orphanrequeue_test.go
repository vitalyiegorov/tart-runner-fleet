package githubscaleset

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
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
					return response(200, fmt.Sprintf(`{"jobs":[{"id":%d,"name":%q,"status":%q}]}`, id, name, status), link), nil
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
				return response(200, fmt.Sprintf(`{"id":420,"name":%q,"run_attempt":%d,"status":%q}`, name, attempt, status), ""), nil
			})})
			if err != nil {
				t.Fatal(err)
			}
			job := WorkflowJob{ID: 42, RunID: 420, RunAttempt: 1, Repository: Repository{Owner: "owner", Name: "repo"}}
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
				tokens.failAt = 4
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
					return response(200, `{"jobs":[{"id":42,"name":"build","status":"queued"}]}`, ""), nil
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
					status = "queued"
				}
				return response(200, fmt.Sprintf(`{"id":420,"name":"build","run_attempt":%d,"status":%q}`, attempt, status), ""), nil
			})})
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := observer.RequeueOrphan(ctx, WorkflowJob{ID: 42, RunID: 420, RunAttempt: 1, Repository: Repository{Owner: "owner", Name: "repo"}}, func(string, []string) bool { return false }, true)
			noop := failure == "confirm attempt" || failure == "poll attempt" || failure == "unsupported token"
			if (err == nil) != noop {
				t.Fatalf("failure=%s outcome=%s err=%v", failure, outcome, err)
			}
			if failure != "rerun" && posts > 1 {
				t.Fatalf("proceeded to rerun after failure: %d", posts)
			}
		})
	}
}
