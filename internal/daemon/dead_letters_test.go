package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/telemetry"
)

// phantomInstance is the 2026-07-25 durable row: draining, stopped, and owned by
// a deregister GitHub will never release.
func phantomInstance(power domain.InstancePower) domain.Instance {
	return domain.Instance{ID: "trf-maestro-096ffcb3a52d8624", Profile: "macos-maestro",
		State: operations.StateDraining, Power: power}
}

func phantomDeadLetter(progressing bool) operations.DeadLetter {
	return operations.DeadLetter{OperationID: "op-ea9b705d234ad29f14e79b6d", Kind: "deregister",
		Code: "deregister:runner_busy", ResourceID: "trf-maestro-096ffcb3a52d8624", Attempts: 835,
		ResourceProgressing: progressing}
}

// Parked needs both halves of the truth, and this seam is the only place that has
// them: the durable side proves nothing is pending or claimed, and the inventory
// side proves the VM is stopped. A release may discount only a resource that
// satisfies both, so a real running VM keeps deferring activation.
func TestParkedDeadLettersRequireAStoppedVMAndNoProgressingWork(t *testing.T) {
	for name, testCase := range map[string]struct {
		instances   []domain.Instance
		progressing bool
		parked      bool
	}{
		"stopped VM with nothing progressing": {instances: []domain.Instance{phantomInstance(domain.InstancePowerStopped)}, parked: true},
		// A VM a successful Tart enumeration proves absent cannot be interrupted by a
		// generation swap either, and it is strictly stronger evidence than stopped.
		// Refusing to park it would let exactly the wedge ADR 0021 removed survive in a
		// new shape: a dead letter whose VM is already gone disabling updates forever.
		"absent VM with nothing progressing": {instances: []domain.Instance{phantomInstance(domain.InstancePowerAbsent)}, parked: true},
		"running VM":                         {instances: []domain.Instance{phantomInstance(domain.InstancePowerRunning)}},
		"unknown power":                      {instances: []domain.Instance{phantomInstance(domain.InstancePowerUnknown)}},
		"another operation can still advance it": {instances: []domain.Instance{phantomInstance(domain.InstancePowerStopped)},
			progressing: true},
		"no live instance row at all": {instances: nil},
		"row belongs to another instance": {instances: []domain.Instance{{ID: "trf-linux-1",
			State: operations.StateRunning, Power: domain.InstancePowerStopped}}},
	} {
		t.Run(name, func(t *testing.T) {
			ticker := engineTicker{deadLetters: func(context.Context) ([]operations.DeadLetter, error) {
				return []operations.DeadLetter{phantomDeadLetter(testCase.progressing)}, nil
			}}
			letters, err := ticker.deadLetterMetrics(context.Background(), testCase.instances)
			if err != nil {
				t.Fatal(err)
			}
			if len(letters) != 1 {
				t.Fatalf("letters=%#v", letters)
			}
			if letters[0].Parked != testCase.parked {
				t.Fatalf("parked=%t want %t", letters[0].Parked, testCase.parked)
			}
			if letters[0].OperationID != "op-ea9b705d234ad29f14e79b6d" || letters[0].Code != "deregister:runner_busy" ||
				letters[0].Attempts != 835 {
				t.Fatalf("letter=%#v", letters[0])
			}
		})
	}
}

// A daemon wired without the durable port publishes none rather than guessing.
func TestDeadLetterMetricsAreAbsentWithoutTheDurablePort(t *testing.T) {
	letters, err := engineTicker{}.deadLetterMetrics(context.Background(), nil)
	if err != nil || letters != nil {
		t.Fatalf("letters=%#v err=%v", letters, err)
	}
}

// The parked list reaches the published document, which is what lets an operator
// name the operation and what lets `fleet update` discount it.
func TestEngineTickerPublishesParkedDeadLetters(t *testing.T) {
	engine, health := tickerFixture(t)
	ticker := engineTicker{engine: engine, health: health,
		operationCounts: func(context.Context) (int, int, error) { return 0, 1, nil },
		deadLetters: func(context.Context) ([]operations.DeadLetter, error) {
			return []operations.DeadLetter{phantomDeadLetter(false)}, nil
		}}
	if err := ticker.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	letters := health.Snapshot().DeadLetters
	if len(letters) != 1 || letters[0].OperationID != "op-ea9b705d234ad29f14e79b6d" {
		t.Fatalf("snapshot dead letters=%#v", letters)
	}
	if got := health.Snapshot().Observations["operations"].Freshness; got != telemetry.ObservationFresh {
		t.Fatalf("operations freshness=%s", got)
	}
}

// Rule 4, applied to the new aggregate: an unreadable dead-letter listing degrades
// the observation. Publishing an empty list would read as "nothing is parked",
// which is precisely the reading that would release the quiescence gate on a fleet
// whose capacity is still committed.
func TestUnreadableDeadLettersDegradeTheOperationsObservation(t *testing.T) {
	engine, health := tickerFixture(t)
	ticker := engineTicker{engine: engine, health: health,
		operationCounts: func(context.Context) (int, int, error) { return 0, 1, nil },
		deadLetters: func(context.Context) ([]operations.DeadLetter, error) {
			return nil, errors.New("database is locked")
		}}
	if err := ticker.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := health.Snapshot().Observations["operations"].Freshness; got != telemetry.ObservationUnavailable {
		t.Fatalf("operations freshness=%s", got)
	}
	if letters := health.Snapshot().DeadLetters; len(letters) != 0 {
		t.Fatalf("unreadable listing published letters: %#v", letters)
	}
}
