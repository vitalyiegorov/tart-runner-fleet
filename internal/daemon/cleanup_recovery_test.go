package daemon

import (
	"context"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

func TestExistingTickOffersOnlyMatchingDeadCleanupToRecovery(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			calls := 0
			ticker := engineTicker{deadLetters: func(context.Context) ([]operations.DeadLetter, error) {
				return []operations.DeadLetter{phantomDeadLetter(false)}, nil
			},
				recoverCleanup: func(_ context.Context, l operations.DeadLetter, i domain.Instance) error {
					calls++
					if l.ResourceID != i.ID {
						t.Fatal("wrong resource")
					}
					if fail {
						return operations.ErrUncertain
					}
					return nil
				}}
			letters, _ := ticker.deadLetterMetrics(context.Background(), []domain.Instance{phantomInstance(domain.InstancePowerStopped)})
			err := ticker.recoverParkedCleanup(context.Background(), letters, []domain.Instance{{ID: "other"}, phantomInstance(domain.InstancePowerStopped)})
			if calls != 1 || (err != nil) != fail {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
		})
	}
}
