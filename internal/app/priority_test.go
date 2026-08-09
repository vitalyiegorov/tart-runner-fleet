package app

import (
	"context"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

func releasePriority() config.Priority {
	return config.Priority{EscalateAfter: 30 * time.Minute, Tiers: []domain.PriorityTier{
		{Name: "release", Match: []domain.PriorityMatch{{JobName: "*publish to stores*"}}},
	}}
}

func TestSchedulerConfigCarriesEscalationOnlyWhenATierIsDeclared(t *testing.T) {
	if got := BuildSchedulerConfig(config.Default()).PriorityEscalation; got != 0 {
		t.Fatalf("undeclared policy set escalation to %s, want zero", got)
	}
	cfg := config.Default()
	cfg.Priority = releasePriority()
	if got := BuildSchedulerConfig(cfg).PriorityEscalation; got != 30*time.Minute {
		t.Fatalf("declared policy escalation = %s, want 30m", got)
	}
}

func TestQueuedDemandsCarryTheTierTheyWereClassifiedInto(t *testing.T) {
	queue := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	store := &fakeDemandStore{statistics: operations.DemandStatistics{MessageID: 9, Available: 4, ObservedAt: time.Now().UTC()}, records: []operations.DemandRecord{
		{Status: operations.DemandJobAvailable, RunnerRequestID: 1, Owner: "vitalyiegorov", Repository: "suuudokuuu", WorkflowRunID: 31327479374,
			DisplayName: "Build and Publish to Stores", WorkflowRef: "vitalyiegorov/suuudokuuu/.github/workflows/stores.yml@refs/heads/main", QueueTime: queue},
		{Status: operations.DemandJobAvailable, RunnerRequestID: 2, Owner: "vitalyiegorov", Repository: "suuudokuuu", WorkflowRunID: 31327479374,
			DisplayName: "Build iOS E2E app", WorkflowRef: "vitalyiegorov/suuudokuuu/.github/workflows/e2e.yml@refs/pull/597/merge", QueueTime: queue},
	}}
	binding := Binding{ScaleSetID: 3, Profile: domain.Profile{ID: "builder", Route: "macos-builder", Platform: domain.PlatformMacOS}}
	coordinator := DemandCoordinator{Store: store, Priority: releasePriority().Policy()}

	got, err := coordinator.QueuedDemands(context.Background(), binding)
	if err != nil || len(got) != 2 {
		t.Fatalf("QueuedDemands() = %#v, %v", got, err)
	}
	if got[0].Priority.Tier != "release" || got[0].Priority.Rank != 1 {
		t.Fatalf("release demand priority = %#v, want the release tier at rank 1", got[0].Priority)
	}
	if got[1].Priority != (domain.Priority{}) {
		t.Fatalf("E2E demand priority = %#v, want the default tier", got[1].Priority)
	}
}

func TestAnUndeclaredPolicyLeavesEveryDemandInTheDefaultTier(t *testing.T) {
	queue := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	store := &fakeDemandStore{statistics: operations.DemandStatistics{MessageID: 9, Available: 4, ObservedAt: time.Now().UTC()}, records: []operations.DemandRecord{
		{Status: operations.DemandJobAvailable, RunnerRequestID: 1, Owner: "o", Repository: "r", WorkflowRunID: 9,
			DisplayName: "Build and Publish to Stores", QueueTime: queue},
	}}
	binding := Binding{ScaleSetID: 3, Profile: domain.Profile{ID: "small", Route: "tiered", Platform: domain.PlatformLinux}}

	got, err := (DemandCoordinator{Store: store}).QueuedDemands(context.Background(), binding)
	if err != nil || len(got) != 1 {
		t.Fatalf("QueuedDemands() = %#v, %v", got, err)
	}
	if got[0].Priority != (domain.Priority{}) {
		t.Fatalf("priority = %#v, want the zero value", got[0].Priority)
	}
}
