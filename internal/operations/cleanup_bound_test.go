package operations

import (
	"context"
	"errors"
	"testing"
	"time"
)

// 2026-07-25 incident: one deregister operation reached 397 attempts over 206
// minutes and an earlier one for the same instance reached 469, with no terminal
// state and no dead-letter signal. ADR 0007 chose unbounded cleanup retries so a
// budget could never expire while GitHub legitimately refuses to remove a runner
// executing a job — but "unbounded" also meant a permanently broken cleanup could
// never surface as dead, and its instance holds its resource vector while it
// retries.
//
// The bound is therefore an escalation ceiling, not a budget: it must exceed the
// longest legitimate refusal (GitHub's own six-hour maximum job duration, which
// at the thirty-second backoff ceiling is 720 attempts) and only then
// dead-letter, so the operation stops retrying and becomes visible instead of
// being retried invisibly forever.
func TestDurableCleanupRetriesPastAnyLegitimateRefusalThenDeadLetters(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	policy := DurableCleanupRetryPolicy(30 * time.Second)

	// A cleanup still inside the ceiling keeps retrying at the capped delay: the
	// exact behaviour ADR 0007 requires while a job could still be running.
	for _, attempt := range []int{1, 2, 100, 397, 469, DurableCleanupMaxAttempts - 1} {
		next, retry := policy.Next(attempt, now)
		if !retry {
			t.Fatalf("attempt %d abandoned cleanup inside the escalation ceiling", attempt)
		}
		if attempt > 10 && next.Sub(now) != 30*time.Second {
			t.Fatalf("attempt %d delay=%s, want the capped 30s", attempt, next.Sub(now))
		}
	}
	if DurableCleanupMaxAttempts*30*time.Second < 6*time.Hour {
		t.Fatalf("ceiling of %d attempts is shorter than GitHub's maximum job duration", DurableCleanupMaxAttempts)
	}
	if _, retry := policy.Next(DurableCleanupMaxAttempts, now); retry {
		t.Fatal("a cleanup failing past the escalation ceiling must dead-letter, not retry invisibly forever")
	}
}

// The worker is what turns the ceiling into a durable dead letter, which is what
// fleet_operations_dead and the bounded failure aggregate publish.
func TestWorkerDeadLettersCleanupPastTheEscalationCeiling(t *testing.T) {
	now := time.Unix(705, 0).UTC()
	failing := ExecutorFunc(func(context.Context, Operation) error {
		return errors.New("runner lifecycle failed at deregister (runner_busy)")
	})
	drain := workerOperation("drain", "deregister")
	drain.Attempts = DurableCleanupMaxAttempts - 1
	store := &workerStore{claimed: []Operation{drain}}
	worker := Worker{Store: store, Owner: "worker", Now: func() time.Time { return now },
		Executors:   map[string]Executor{"deregister": failing},
		RetryByKind: map[string]RetryPolicy{"deregister": DurableCleanupRetryPolicy(30 * time.Second)}}

	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.dead != 1 || store.retried != 1 {
		t.Fatalf("exhausted cleanup dead=%d retried=%d, want one dead letter", store.dead, store.retried)
	}
	if store.retryError != "runner lifecycle failed at deregister (runner_busy)" {
		t.Fatalf("dead letter lost its classified reason: %q", store.retryError)
	}
}
