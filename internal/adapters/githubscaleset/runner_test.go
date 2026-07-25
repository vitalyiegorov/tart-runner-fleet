package githubscaleset

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

// 2026-07-25 incident: drain operation op-ea9b705d234ad29f14e79b6d (kind
// deregister, resource trf-maestro-096ffcb3a52d8624) burned 397 attempts over
// 206 minutes, and an earlier operation for the same instance burned 469, with
// one indistinguishable persisted failure: "runner lifecycle failed at
// deregister". ADR 0007 deliberately retries owned runner cleanup forever, so
// the attempts are by design; what made the incident untriageable is that the
// durable record never said WHY. Runner administration therefore needs the same
// closed failure vocabulary the ingest path received in #95, so a permission
// regression, a busy-runner refusal, an unresolved registration scope, and a
// failed lookup are told apart from the operation row alone.

func TestRunnerFailureDetailClassifiesRunnerAdministration(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{name: "no failure", err: nil, want: ""},
		{name: "GitHub refuses to remove a runner executing a job",
			err:  fmt.Errorf("remove scale-set runner: %w", scaleset.JobStillRunningError),
			want: ReasonRunnerBusy},
		{name: "runner administration port missing", err: ErrRunnerAdminUnavailable, want: ReasonRunnerAdminUnavailable},
		{name: "instance not bound to a registration scope",
			err:  fmt.Errorf("%w: uncertain", ErrRunnerScopeUnresolved),
			want: ReasonRunnerScopeUnresolved},
		{name: "unauthenticated runner administration",
			err:  fmt.Errorf("%w: %w", ErrRunnerLookup, &APIError{Kind: Authentication, Status: http.StatusUnauthorized}),
			want: ReasonRunnerForbidden},
		{name: "unauthorized runner administration",
			err:  fmt.Errorf("%w: %w", ErrRunnerRemoval, &APIError{Kind: Authorization, Status: http.StatusForbidden}),
			want: ReasonRunnerForbidden},
		{name: "pre-removal lookup failed",
			err:  fmt.Errorf("%w: %w", ErrRunnerLookup, &APIError{Kind: Server, Status: http.StatusBadGateway}),
			want: ReasonRunnerLookupFailed},
		{name: "removal failed",
			err:  fmt.Errorf("%w: %w", ErrRunnerRemoval, errors.New("connection reset")),
			want: ReasonRunnerRemovalFailed},
		{name: "unclassified failure degrades to the generic reason",
			err:  errors.New("something else entirely"),
			want: ReasonDeregisterFailed},
		{name: "rate limited removal stays generic rather than guessing",
			err:  &APIError{Kind: RateLimited, Status: http.StatusTooManyRequests, RetryAfter: time.Second},
			want: ReasonDeregisterFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := RunnerFailureDetail(test.err); got != test.want {
				t.Fatalf("RunnerFailureDetail() = %q, want %q", got, test.want)
			}
			if test.want != "" && !ValidRunnerFailureReason(test.want) {
				t.Fatalf("%q is outside the closed vocabulary", test.want)
			}
		})
	}
}

// An authentication or authorization failure must never be reported as an
// absent runner: a permissions regression would then masquerade as successful
// cleanup and release the teardown of instances the fleet cannot actually
// deregister. Absence is proven only by a successful observation that returns no
// runner, never inferred from any error.
func TestRunnerFailureDetailNeverReportsAbsenceForDeniedAccess(t *testing.T) {
	for _, kind := range []ErrorKind{Authentication, Authorization} {
		err := fmt.Errorf("%w: %w", ErrRunnerLookup, &APIError{Kind: kind, Status: http.StatusForbidden})
		if got := RunnerFailureDetail(err); got != ReasonRunnerForbidden {
			t.Fatalf("RunnerFailureDetail(%s) = %q, want %q", kind, got, ReasonRunnerForbidden)
		}
	}
	if ValidRunnerFailureReason("runner_absent") {
		t.Fatal("absence must not exist as a failure reason: it is a success, and only a lookup may prove it")
	}
	if ValidRunnerFailureReason("") || ValidRunnerFailureReason("anything") {
		t.Fatal("the runner failure vocabulary must be closed")
	}
	for _, reason := range RunnerFailureReasons() {
		if !ValidRunnerFailureReason(reason) {
			t.Fatalf("advertised reason %q is not valid", reason)
		}
	}
}

// Every ScaleSet.Deregister failure must arrive classified, so the drain
// executor can persist a bounded reason instead of one opaque stage string.
func TestScaleSetDeregisterFailuresCarryTheirReason(t *testing.T) {
	fake := &fakeScaleSet{}
	withoutRunners, err := NewScaleSet(ScaleSetConfig{Messages: fake, JIT: fake, ScaleSetID: 9})
	if err != nil {
		t.Fatal(err)
	}
	if got := RunnerFailureDetail(withoutRunners.Deregister(context.Background(), "runner")); got != ReasonRunnerAdminUnavailable {
		t.Fatalf("missing runner administration reason = %q", got)
	}
	scale, err := NewScaleSet(ScaleSetConfig{Messages: fake, JIT: fake, Runners: fake, ScaleSetID: 9})
	if err != nil {
		t.Fatal(err)
	}
	if got := RunnerFailureDetail(scale.Deregister(context.Background(), "bad runner name")); got != ReasonRunnerAdminUnavailable {
		t.Fatalf("invalid runner name reason = %q", got)
	}
	fake.getErr = errors.New("github unavailable")
	if got := RunnerFailureDetail(scale.Deregister(context.Background(), "runner")); got != ReasonRunnerLookupFailed {
		t.Fatalf("lookup failure reason = %q", got)
	}
	if got := RunnerFailureDetail(func() error { _, err := scale.Registered(context.Background(), "runner"); return err }()); got != ReasonRunnerLookupFailed {
		t.Fatalf("registration observation reason = %q", got)
	}
	fake.getErr = nil
	fake.runner = &scaleset.RunnerReference{ID: 8, Name: "runner", RunnerScaleSetID: 9}
	fake.deleteErr = errors.New("connection reset")
	if got := RunnerFailureDetail(scale.Deregister(context.Background(), "runner")); got != ReasonRunnerRemovalFailed {
		t.Fatalf("removal failure reason = %q", got)
	}
	fake.deleteErr = fmt.Errorf("agent job still running: %w", scaleset.JobStillRunningError)
	if got := RunnerFailureDetail(scale.Deregister(context.Background(), "runner")); got != ReasonRunnerBusy {
		t.Fatalf("busy refusal reason = %q", got)
	}
}

// The idempotent post-condition stays symmetric: a runner GitHub does not have
// is a completed deregistration whether the pre-removal observation returns no
// runner or the actions service answers the observation itself with its own
// runner-not-found signal. Only a proof of absence may do this — never an
// unexplained failure.
func TestScaleSetDeregisterSucceedsWhenGitHubHasNoRunner(t *testing.T) {
	fake := &fakeScaleSet{}
	scale, err := NewScaleSet(ScaleSetConfig{Messages: fake, JIT: fake, Runners: fake, ScaleSetID: 9})
	if err != nil {
		t.Fatal(err)
	}
	if err := scale.Deregister(context.Background(), "runner"); err != nil {
		t.Fatalf("absent runner must be an idempotent success, got %v", err)
	}
	fake.getErr = fmt.Errorf("observe runner: %w", scaleset.RunnerNotFoundError)
	if err := scale.Deregister(context.Background(), "runner"); err != nil {
		t.Fatalf("runner-not-found observation must be an idempotent success, got %v", err)
	}
}
