package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/actions/scaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// 2026-07-25 incident, second half. The maestro instance
// trf-maestro-096ffcb3a52d8624 sat in draining for 206 minutes while its
// deregister operation burned 397 attempts (and an earlier operation for the
// same instance burned 469 before an abort recreated the work). Every one of
// those attempts persisted the same string — "runner lifecycle failed at
// deregister" — so an operator could not tell a busy-runner refusal from a
// permission regression, an unresolved registration scope, or a failed lookup,
// and the versioned admin API exposed only an unlabelled retrying count.
//
// ADR 0007 keeps owned runner cleanup retrying indefinitely on purpose, so the
// repair is not a retry budget: it is making the wait explainable. Every
// deregister-stage failure must persist exactly one bounded, credential-free
// reason drawn from the closed runner-administration vocabulary.
func TestDrainDeregisterFailurePersistsClosedVocabularyReason(t *testing.T) {
	for _, test := range []struct {
		name    string
		failure error
		want    string
	}{
		{name: "GitHub refuses to remove a runner still executing a job",
			failure: fmt.Errorf("remove scale-set runner: %w", scaleset.JobStillRunningError),
			want:    "runner lifecycle failed at deregister (runner_busy)"},
		{name: "runner administration denied",
			failure: fmt.Errorf("%w: %w", githubscaleset.ErrRunnerLookup, &githubscaleset.APIError{Kind: githubscaleset.Authorization, Status: http.StatusForbidden}),
			want:    "runner lifecycle failed at deregister (runner_forbidden)"},
		{name: "runner administration unavailable",
			failure: githubscaleset.ErrRunnerAdminUnavailable,
			want:    "runner lifecycle failed at deregister (runner_admin_unavailable)"},
		{name: "instance not bound to a registration scope",
			failure: fmt.Errorf("%w: %w", githubscaleset.ErrRunnerScopeUnresolved, operations.ErrUncertain),
			want:    "runner lifecycle failed at deregister (runner_scope_unresolved)"},
		{name: "pre-removal lookup failed",
			failure: fmt.Errorf("%w: %w", githubscaleset.ErrRunnerLookup, errors.New("gateway timeout")),
			want:    "runner lifecycle failed at deregister (runner_lookup_failed)"},
		{name: "removal failed",
			failure: fmt.Errorf("%w: %w", githubscaleset.ErrRunnerRemoval, errors.New("connection reset")),
			want:    "runner lifecycle failed at deregister (runner_removal_failed)"},
		{name: "unclassified failure still names a bounded reason",
			failure: errors.New("something else entirely"),
			want:    "runner lifecycle failed at deregister (deregister_failed)"},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := []string{}
			state := &memoryState{instance: recoveryInstance(operations.DrainPhaseStoppedRecovery)}
			control := &fakeDrainControl{calls: &calls, deregisterErr: test.failure}
			executor := drainExecutor(state, fakeVM{calls: &calls, running: false}, control)

			err := executor.Execute(context.Background(), operations.Operation{Kind: OperationDrain, ResourceID: state.instance.ID})

			if err == nil || err.Error() != test.want {
				t.Fatalf("persisted failure = %v, want %q", err, test.want)
			}
			var detail interface{ FailureReason() string }
			if !errors.As(err, &detail) {
				t.Fatal("a deregister failure must expose its bounded reason programmatically")
			}
			if !githubscaleset.ValidRunnerFailureReason(detail.FailureReason()) {
				t.Fatalf("reason %q is outside the closed vocabulary", detail.FailureReason())
			}
			if state.instance.State != operations.StateDraining {
				t.Fatalf("cleanup must remain the standing order, got %s", state.instance.State)
			}
			for _, call := range calls {
				if call == "stop:"+state.instance.ID || call == "delete:"+state.instance.ID {
					t.Fatalf("a failed deregister may never reach teardown; calls=%#v", calls)
				}
			}
		})
	}
}

// The persisted failure must never echo upstream response text, a token, or a
// runner credential — rule 7. Only the stage and the closed reason may appear.
func TestDrainDeregisterFailureNeverEchoesUpstreamText(t *testing.T) {
	calls := []string{}
	state := &memoryState{instance: recoveryInstance(operations.DrainPhaseStoppedRecovery)}
	secret := "ghs_deadbeefdeadbeefdeadbeef"
	control := &fakeDrainControl{calls: &calls, deregisterErr: fmt.Errorf("%w: token %s rejected by https://api.github.com/orgs/x/actions/runners",
		githubscaleset.ErrRunnerRemoval, secret)}
	executor := drainExecutor(state, fakeVM{calls: &calls}, control)

	err := executor.Execute(context.Background(), operations.Operation{Kind: OperationDrain, ResourceID: state.instance.ID})

	if err == nil {
		t.Fatal("expected a classified deregister failure")
	}
	if got := err.Error(); got != "runner lifecycle failed at deregister (runner_removal_failed)" {
		t.Fatalf("persisted failure = %q", got)
	}
}

// FailureCode is the inverse mapping telemetry needs: a durable failure string
// becomes exactly one closed code, and anything unrecognized is withheld rather
// than surfaced as free-form text.
func TestFailureCodeMapsDurableFailuresToClosedCodes(t *testing.T) {
	for _, test := range []struct {
		persisted string
		want      string
	}{
		{persisted: "runner lifecycle failed at deregister (runner_busy)", want: "deregister:runner_busy"},
		{persisted: "runner lifecycle failed at deregister (runner_forbidden)", want: "deregister:runner_forbidden"},
		{persisted: "runner lifecycle failed at deregister", want: "deregister"},
		{persisted: "runner lifecycle failed at drain_guard", want: "drain_guard"},
		{persisted: "runner lifecycle failed at persist", want: "persist"},
		{persisted: "runner lifecycle failed at deregister (made up)", want: CodeUnclassified},
		{persisted: "executor panic: runtime error", want: CodeUnclassified},
		{persisted: "", want: CodeUnclassified},
	} {
		t.Run(test.persisted, func(t *testing.T) {
			if got := FailureCode(test.persisted); got != test.want {
				t.Fatalf("FailureCode(%q) = %q, want %q", test.persisted, got, test.want)
			}
		})
	}
}

// The registration scope is resolved per instance, so an instance whose
// repo/profile pair is not configured cannot be deregistered at all. That
// failure was previously indistinguishable from a GitHub outage; it must arrive
// classified so the operator looks at configuration instead of GitHub.
func TestControlRouterDeregisterNamesAnUnresolvedScope(t *testing.T) {
	router := ControlRouter{Sources: map[SourceKey]SourceBinding{}}

	err := router.Deregister(context.Background(), lifecycleInstance(operations.StateDraining))

	if !errors.Is(err, githubscaleset.ErrRunnerScopeUnresolved) || !errors.Is(err, operations.ErrUncertain) {
		t.Fatalf("unresolved scope error = %v", err)
	}
	if got := githubscaleset.RunnerFailureDetail(err); got != githubscaleset.ReasonRunnerScopeUnresolved {
		t.Fatalf("unresolved scope reason = %q", got)
	}
}

// FailureReason is the boundary guard, so it must withhold anything outside the
// closed vocabulary even when a stage error is constructed by hand.
func TestStageErrorWithholdsUnboundedReasons(t *testing.T) {
	if got := (stageError{stage: StageDeregister, reason: "ghs_do-not-leak"}).FailureReason(); got != "" {
		t.Fatalf("FailureReason() = %q, want it withheld", got)
	}
	if got := (stageError{stage: StageDeregister}).FailureReason(); got != "" {
		t.Fatalf("a stage without a reason must report none, got %q", got)
	}
	if got := (stageError{stage: StageDeregister, reason: githubscaleset.ReasonRunnerBusy}).FailureReason(); got != githubscaleset.ReasonRunnerBusy {
		t.Fatalf("FailureReason() = %q", got)
	}
}
