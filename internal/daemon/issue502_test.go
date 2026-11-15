package daemon

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

func TestDemandConflictWarningNamesChecksWithoutPayload(t *testing.T) {
	for _, check := range []string{"demand_job_identity", "reservation_duplicate", "binding_shape_mismatch", "binding_metadata_invalid", "binding_source_cas", "binding_target_cas", "binding_rebind_cas", "instance_state_transition", "transition_instance_cas", "transition_idempotent_state"} {
		t.Run(check, func(t *testing.T) {
			var output bytes.Buffer
			reporter := newFailureReporter(&output, nil)
			detail := &operations.DemandConflict{Check: check, MessageType: operations.DemandJobStarted, RequestID: 101, JobID: "job-uuid", Runner: "runner-beta", Instance: "runner-alpha", From: operations.StateAssigned, To: operations.StateRunning}
			err := errors.Join(errors.New(`payload={"jitConfig":"secret-jit","token":"secret-token","labels":["private-label"]}`), detail)
			failure := githubscaleset.DescribeIngestFailure(err)
			reporter.reportBinding(app.Binding{}, failure)
			line := output.String()
			for _, want := range []string{"reason=demand_commit_conflict", "phase=commit", "check=" + check, "messageType=JobStarted", "runnerRequestId=101", "jobId=job-uuid", "runner=runner-beta", "instance=runner-alpha", "from=assigned", "to=running"} {
				if !strings.Contains(line, want) {
					t.Fatalf("missing %q: %s", want, line)
				}
			}
			for _, secret := range []string{"payload", "jitConfig", "secret-jit", "token", "secret-token", "labels", "private-label"} {
				if strings.Contains(line, secret) {
					t.Fatalf("leaked %q: %s", secret, line)
				}
			}
		})
	}
	// Joined independent projections must retain every target, including beneath session wrappers.
	var output bytes.Buffer
	reporter := newFailureReporter(&output, nil)
	err := &githubscaleset.SessionFailure{Reason: githubscaleset.ReasonDemandCommitConflict, Cause: errors.Join(
		&operations.DemandConflict{Check: "binding_source_cas", Instance: "first"},
		&operations.DemandConflict{Check: "binding_target_cas", Instance: "second"})}
	reporter.reportBinding(app.Binding{}, githubscaleset.DescribeIngestFailure(err))
	if !strings.Contains(output.String(), "instance=first") || !strings.Contains(output.String(), "instance=second") {
		t.Fatal(output.String())
	}
}

func TestIssue502LocalConflictKeepsSessionAndRedelivers(t *testing.T) {
	ctx := context.Background()
	broker := &durableBroker{pending: 7}
	initial := &brokerGeneration{broker: broker}
	harness := newRecoveryHarness(t, initial, githubscaleset.SessionRecoveryPolicy{MaxConsecutiveFailures: 1})
	durable := false
	attempts := 0
	commit := func(context.Context, githubscaleset.Demand) error {
		attempts++
		if attempts <= 2 {
			// Both a pre-inbox refusal and a post-commit projection refusal must nack.
			durable = attempts == 2
			return &operations.DemandConflict{Check: "instance_state_transition", Instance: "runner"}
		}
		if !durable {
			t.Fatal("ack before durable inbox commit")
		}
		return nil
	}
	for range 2 {
		if err := harness.source.Handle(ctx, commit); !errors.Is(err, operations.ErrConflict) || app.FailureReasonOf(err) != githubscaleset.ReasonDemandCommitConflict {
			t.Fatalf("conflict lost: %v", err)
		}
		if harness.opens.Load() != 0 || initial.closed.Load() != 0 || len(broker.acks) != 0 || harness.source.failures.Consecutive != 0 {
			t.Fatalf("local conflict replaced session or acknowledged: opens=%d closes=%d acks=%v failures=%+v", harness.opens.Load(), initial.closed.Load(), broker.acks, harness.source.failures)
		}
	}
	if err := harness.source.Handle(ctx, commit); err != nil {
		t.Fatal(err)
	}
	if attempts != 3 || len(broker.acks) != 1 || broker.acks[0] != 7 || harness.opens.Load() != 0 {
		t.Fatalf("attempts=%d acks=%v opens=%d", attempts, broker.acks, harness.opens.Load())
	}
}

func TestIssue502TransportFailureStillReplacesSession(t *testing.T) {
	for _, err := range []error{errors.New("transport connection reset"), &githubscaleset.APIError{Kind: githubscaleset.Server}, &githubscaleset.APIError{Kind: githubscaleset.Conflict}, operations.ErrConflict} {
		initial := &wedgedSessionSource{err: err}
		harness := newRecoveryHarness(t, initial, githubscaleset.SessionRecoveryPolicy{})
		if got := harness.handle(context.Background()); got == nil || harness.opens.Load() != 1 || initial.closed.Load() != 1 {
			t.Fatalf("err=%v opens=%d closes=%d", got, harness.opens.Load(), initial.closed.Load())
		}
		if err := harness.handle(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}
