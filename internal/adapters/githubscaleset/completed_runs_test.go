package githubscaleset

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func TestCompletedRunsRequiresPositiveExactEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, body            string
		status, attempt, want int
	}{
		{"complete", `{"id":77,"status":"completed","run_attempt":2}`, 200, 2, 1},
		{"latest unknown", `{"id":77,"status":"completed","run_attempt":3}`, 200, 0, 1},
		{"active", `{"id":77,"status":"in_progress","run_attempt":2}`, 200, 2, 0},
		{"other attempt", `{"id":77,"status":"completed","run_attempt":3}`, 200, 2, 0},
		{"other run", `{"id":78,"status":"completed","run_attempt":2}`, 200, 2, 0},
		{"missing attempt", `{"id":77,"status":"completed"}`, 200, 2, 0},
		{"unknown", `{}`, 200, 2, 0},
		{"bad json", `{`, 200, 2, 0},
		{"gone", `{}`, 410, 2, 0},
		{"server", `{}`, 500, 2, 0},
		{"not found", `{}`, 404, 2, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, _ := NewObserver(ObserverConfig{BaseURL: "https://api.test", HTTP: doerFunc(func(r *http.Request) (*http.Response, error) {
				path := "/repos/o/r/actions/runs/77"
				if tc.attempt > 0 {
					path += "/attempts/2"
				}
				if r.URL.Path != path {
					t.Fatalf("path=%s", r.URL.Path)
				}
				if _, ok := r.Context().Deadline(); !ok {
					t.Fatal("missing deadline")
				}
				return response(tc.status, tc.body, ""), nil
			}), Tokens: TokenSourceFunc(func(context.Context) (string, error) { return "test", nil })})
			got, err := o.CompletedRuns(context.Background(), []WorkflowRun{{ID: 77, Attempt: tc.attempt, Repository: Repository{"o", "r"}}})
			if len(got) != tc.want {
				t.Fatalf("got=%v err=%v", got, err)
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCompletedRunsBoundsAndRotatesRequests(t *testing.T) {
	seen := map[string]int{}
	o, _ := NewObserver(ObserverConfig{BaseURL: "https://api.test", HTTP: doerFunc(func(r *http.Request) (*http.Response, error) { seen[r.URL.Path]++; return response(200, `{}`, ""), nil }), Tokens: TokenSourceFunc(func(context.Context) (string, error) { return "", nil })})
	candidates := []WorkflowRun{}
	for i := int64(1); i <= 15; i++ {
		candidates = append(candidates, WorkflowRun{ID: i, Repository: Repository{"o", "r"}})
	}
	for range 3 {
		before := 0
		for _, n := range seen {
			before += n
		}
		if _, err := o.CompletedRuns(context.Background(), candidates); err != nil {
			t.Fatal(err)
		}
		after := 0
		for _, n := range seen {
			after += n
		}
		if after-before != 10 {
			t.Fatalf("requests=%d", after-before)
		}
	}
	for i := 1; i <= 15; i++ {
		if seen[fmt.Sprintf("/repos/o/r/actions/runs/%d", i)] != 2 {
			t.Fatalf("rotation=%v", seen)
		}
	}
	if _, err := o.CompletedRuns(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := o.CompletedRuns(context.Background(), []WorkflowRun{{ID: -1}}); err != nil {
		t.Fatal(err)
	}
	o.tokens = TokenSourceFunc(func(context.Context) (string, error) { return "", errors.New("unavailable") })
	if _, err := o.CompletedRuns(context.Background(), candidates); err == nil {
		t.Fatal("authentication failure hidden")
	}
}
