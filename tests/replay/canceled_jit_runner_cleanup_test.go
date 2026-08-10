package replay_test

import (
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

func TestCanceledJITRequestCleanupOutlivesReplacementJob(t *testing.T) {
	now := time.Date(2026, 7, 14, 8, 8, 36, 0, time.UTC)
	policy := operations.DurableCleanupRetryPolicy(30 * time.Second)

	for attempt := 1; attempt <= 100; attempt++ {
		next, retry := policy.Next(attempt, now, now)
		if !retry {
			t.Fatalf("cleanup dead-lettered at attempt %d while the replacement job can still be running", attempt)
		}
		if next.After(now.Add(30 * time.Second)) {
			t.Fatalf("cleanup retry exceeded its bounded backoff: %v", next)
		}
	}
}
