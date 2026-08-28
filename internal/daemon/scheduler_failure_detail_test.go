package daemon

import (
	"errors"
	"fmt"
	"testing"
)

type classifiedFailure struct{ reason string }

func (c classifiedFailure) Error() string         { return "scheduler tick failed" }
func (c classifiedFailure) FailureReason() string { return c.reason }

// A failing tick has no plan, so the admin API's only account of why is this
// classification. It must survive wrapping — the error reaches here through the
// engine — and it must stay empty rather than inventing a reason for an error
// that carries none, because an unclassified failure that reads as a named one
// is worse than silence.
func TestSchedulerFailureDetailNamesOnlyWhatTheErrorClassified(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{"classified", classifiedFailure{reason: "plan_commit_rejected"}, "plan_commit_rejected"},
		{"wrapped", fmt.Errorf("tick: %w", classifiedFailure{reason: "observation_stale"}), "observation_stale"},
		{"unclassified", errors.New("connection reset"), ""},
		{"nil", nil, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := schedulerFailureDetail(test.err); got != test.want {
				t.Fatalf("schedulerFailureDetail(%v) = %q, want %q", test.err, got, test.want)
			}
		})
	}
}
