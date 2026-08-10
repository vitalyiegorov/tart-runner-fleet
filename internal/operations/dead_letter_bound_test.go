package operations

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// This file is defect 2 of the 2026-08-10 incident (issue #233).
//
// `fleet operations discharge` refused with `operation_not_dead` after 67
// attempts and 90 minutes, and `fleet operations` reported `retrying 1, dead 0`
// throughout. "Retrying forever" is not a state an operator can act on, and the
// documented recovery path in docs/OPERATIONS.md was therefore unavailable in
// precisely the situation it exists for.

// TestAnAttemptCeilingIsNotATimeBound is the arithmetic that made the ceiling
// untrue. ADR 0007 reasoned about 720 attempts as "six hours at the
// thirty-second backoff ceiling", which is only true if an attempt costs
// nothing. On the studio each attempt cost the backend's full 45-second command
// deadline, so the real ceiling was fifteen hours.
func TestAnAttemptCeilingIsNotATimeBound(t *testing.T) {
	const measuredAttemptCost = 45 * time.Second
	realCeiling := DurableCleanupMaxAttempts * (measuredAttemptCost + 30*time.Second)
	if realCeiling <= DurableCleanupMaxElapsed {
		t.Fatalf("an attempt that costs %s makes the ceiling %s, which no longer needs a separate elapsed bound",
			measuredAttemptCost, realCeiling)
	}
	policy := DurableCleanupRetryPolicy(30 * time.Second)
	started := time.Date(2026, 8, 10, 12, 55, 1, 0, time.UTC)
	if _, retry := policy.Next(68, started.Add(DurableCleanupMaxElapsed), started); retry {
		t.Fatal("cleanup kept retrying past its own elapsed ceiling, so nothing could ever be discharged")
	}
	if _, retry := policy.Next(68, started.Add(DurableCleanupMaxElapsed-time.Second), started); !retry {
		t.Fatal("cleanup was abandoned inside the elapsed ceiling ADR 0007 requires")
	}
}

// TestElapsedBoundIsNotInferredFromAnUnknownStart keeps the bound fail-open in
// the one direction that matters: an operation whose creation instant is
// unreadable must not be expired on a guess.
func TestElapsedBoundIsNotInferredFromAnUnknownStart(t *testing.T) {
	policy := DurableCleanupRetryPolicy(30 * time.Second)
	if _, retry := policy.Next(2, time.Unix(0, 0).UTC().Add(100*time.Hour), time.Time{}); !retry {
		t.Fatal("an unknown start was read as a long one")
	}
}

// TestWorkerParksAnExhaustedOperationAtOnce is the other half. An executor that
// has proved a failure permanent must not have to wait out a ceiling sized for a
// fault that might still resolve itself: the operation becomes a dead letter
// immediately, which is what makes the escape hatch reachable.
func TestWorkerParksAnExhaustedOperationAtOnce(t *testing.T) {
	store := &workerStore{claimed: []Operation{workerOperation("wedged", "deregister")}}
	worker := Worker{Store: store, Owner: "worker", OperationDeadline: time.Second,
		RetryByKind: map[string]RetryPolicy{"deregister": DurableCleanupRetryPolicy(30 * time.Second)},
		Executors: map[string]Executor{"deregister": ExecutorFunc(func(context.Context, Operation) error {
			return fmt.Errorf("runner lifecycle failed at stop: %w", ErrExhausted)
		})}}
	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.dead != 1 {
		t.Fatalf("dead = %d after an exhausted executor, want the operation parked at once", store.dead)
	}
	if !store.retryAvailable.IsZero() {
		t.Fatalf("a parked operation was scheduled for %s", store.retryAvailable)
	}
}

// TestWorkerKeepsRetryingAnOrdinaryFailure pins the other side: only a proven
// exhaustion parks early. A cleanup GitHub may still legitimately refuse keeps
// retrying, exactly as ADR 0007 requires.
func TestWorkerKeepsRetryingAnOrdinaryFailure(t *testing.T) {
	store := &workerStore{claimed: []Operation{workerOperation("busy", "deregister")}}
	worker := Worker{Store: store, Owner: "worker", OperationDeadline: time.Second,
		RetryByKind: map[string]RetryPolicy{"deregister": DurableCleanupRetryPolicy(30 * time.Second)},
		Executors: map[string]Executor{"deregister": ExecutorFunc(func(context.Context, Operation) error {
			return errors.New("runner lifecycle failed at deregister (runner_busy)")
		})}}
	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.dead != 0 || store.retried != 1 {
		t.Fatalf("dead=%d retried=%d, want an ordinary refusal to keep retrying", store.dead, store.retried)
	}
}

// TestWorkerDeadLettersOnElapsedTimeAlone drives the elapsed bound through the
// worker, which is the only place the operation's creation instant meets its
// retry policy.
func TestWorkerDeadLettersOnElapsedTimeAlone(t *testing.T) {
	created := time.Date(2026, 8, 10, 12, 55, 1, 0, time.UTC)
	operation := workerOperation("wedged", "deregister")
	operation.CreatedAt = created
	operation.Attempts = 67
	store := &workerStore{claimed: []Operation{operation}}
	worker := Worker{Store: store, Owner: "worker", OperationDeadline: time.Second,
		Now:         func() time.Time { return created.Add(DurableCleanupMaxElapsed + time.Minute) },
		RetryByKind: map[string]RetryPolicy{"deregister": DurableCleanupRetryPolicy(30 * time.Second)},
		Executors: map[string]Executor{"deregister": ExecutorFunc(func(context.Context, Operation) error {
			return errors.New("runner lifecycle failed at stop")
		})}}
	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.dead != 1 {
		t.Fatalf("dead = %d at attempt 67 after six hours, want a dischargeable dead letter", store.dead)
	}
}
