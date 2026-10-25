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
				recoverCleanup: func(_ context.Context, l operations.DeadLetter, i domain.Instance, _ bool) error {
					calls++
					if l.ResourceID != i.ID {
						t.Fatal("wrong resource")
					}
					if fail {
						return operations.ErrUncertain
					}
					return nil
				}}
			_, letters, _ := ticker.deadLetterMetrics(context.Background(), []domain.Instance{phantomInstance(domain.InstancePowerStopped)})
			err := ticker.recoverDeadCleanup(context.Background(), letters, []domain.Instance{{ID: "other"}, phantomInstance(domain.InstancePowerStopped)}, true)
			if calls != 1 || (err != nil) != fail {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
		})
	}
}

// A drain that died at stop with its VM still running is not parked, and the
// tick must still offer it, carrying the host verdict recovery decides on
// (issue #424, ADR 0081).
func TestExistingTickOffersUnparkedDeadCleanupWithTheHostVerdict(t *testing.T) {
	for _, healthy := range []bool{false, true} {
		var offered []bool
		ticker := engineTicker{deadLetters: func(context.Context) ([]operations.DeadLetter, error) {
			return []operations.DeadLetter{phantomDeadLetter(false)}, nil
		}, recoverCleanup: func(_ context.Context, _ operations.DeadLetter, _ domain.Instance, host bool) error {
			offered = append(offered, host)
			return nil
		}}
		running := []domain.Instance{phantomInstance(domain.InstancePowerRunning)}
		letters, durable, err := ticker.deadLetterMetrics(context.Background(), running)
		if err != nil || len(letters) != 1 || letters[0].Parked {
			t.Fatalf("letters=%#v err=%v", letters, err)
		}
		if err := ticker.recoverDeadCleanup(context.Background(), durable, running, healthy); err != nil {
			t.Fatal(err)
		}
		if len(offered) != 1 || offered[0] != healthy {
			t.Fatalf("offered=%v healthy=%v", offered, healthy)
		}
	}
}

func TestShadowTickRecoversNothing(t *testing.T) {
	if err := (engineTicker{}).recoverDeadCleanup(context.Background(), []operations.DeadLetter{phantomDeadLetter(false)},
		[]domain.Instance{phantomInstance(domain.InstancePowerRunning)}, true); err != nil {
		t.Fatal(err)
	}
}
