package app

import (
	"context"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

func tierDemand(jobID int64, age time.Duration, priority domain.Priority) domain.Demand {
	return domain.Demand{Key: domain.DemandKey{Repo: "o/r", RunID: 9, Attempt: 1, JobID: jobID},
		CreatedAt: tierNow.Add(-age), Profile: "builder", Priority: priority}
}

var tierNow = time.Date(2026, 8, 9, 18, 0, 0, 0, time.UTC)

// TestQueueSummaryReportsWhichTierWaitingDemandLandedIn is the auditability
// requirement of issue #224: the classification has to be visible in the moment
// it matters, or an operator cannot tell a mis-declared tier from a busy queue.
func TestQueueSummaryReportsWhichTierWaitingDemandLandedIn(t *testing.T) {
	coordinator := DemandCoordinator{Store: &fakeDemandStore{}, Priority: releasePriority().Policy()}
	queued := []domain.Demand{
		tierDemand(1, 65*time.Minute, domain.Priority{Tier: "release", Rank: 1}),
		tierDemand(2, 81*time.Minute, domain.Priority{}),
		tierDemand(3, 20*time.Minute, domain.Priority{}),
	}

	summary, err := coordinator.QueueSummary(context.Background(), Binding{ScaleSetID: 1}, queued)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Tiers) != 2 {
		t.Fatalf("tiers = %#v, want two rows", summary.Tiers)
	}
	if summary.Tiers[0].Tier != "release" || summary.Tiers[0].Count != 1 || !summary.Tiers[0].Oldest.Equal(tierNow.Add(-65*time.Minute)) {
		t.Fatalf("release row = %#v", summary.Tiers[0])
	}
	if summary.Tiers[1].Tier != domain.DefaultPriorityTier || summary.Tiers[1].Count != 2 ||
		!summary.Tiers[1].Oldest.Equal(tierNow.Add(-81*time.Minute)) {
		t.Fatalf("default row = %#v", summary.Tiers[1])
	}
}

// TestQueueSummaryPublishesNoTiersWhenNoneAreDeclared keeps the operator surface
// identical for a fleet that declared no policy.
func TestQueueSummaryPublishesNoTiersWhenNoneAreDeclared(t *testing.T) {
	coordinator := DemandCoordinator{Store: &fakeDemandStore{}}
	summary, err := coordinator.QueueSummary(context.Background(), Binding{ScaleSetID: 1},
		[]domain.Demand{tierDemand(1, time.Minute, domain.Priority{})})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Tiers != nil {
		t.Fatalf("tiers = %#v, want none", summary.Tiers)
	}
}

// TestQueueTiersCountRestObservedJobsTheBrokerHasNotDelivered mirrors the whole
// summary's rule one level down: REST sees the complete queue, the broker sees
// what it has delivered, and the larger of the two is the truth per tier.
func TestQueueTiersCountRestObservedJobsTheBrokerHasNotDelivered(t *testing.T) {
	store := &fakeDemandStore{githubJobs: map[int64][]operations.GitHubJobObservation{1: {
		{WorkflowJobID: 1, Owner: "o", Repository: "r", WorkflowRunID: 9, RunAttempt: 1,
			DisplayName: "Build and Publish to Stores", CreatedAt: tierNow.Add(-90 * time.Minute), QueueTimeExact: true},
		{WorkflowJobID: 2, Owner: "o", Repository: "r", WorkflowRunID: 9, RunAttempt: 1,
			DisplayName: "Build and Publish to Stores", CreatedAt: tierNow.Add(-70 * time.Minute), QueueTimeExact: true},
		{WorkflowJobID: 3, Owner: "o", Repository: "r", WorkflowRunID: 9, RunAttempt: 1,
			DisplayName: "unit", CreatedAt: tierNow.Add(-10 * time.Minute), QueueTimeExact: true},
	}}}
	coordinator := DemandCoordinator{Store: store, Priority: releasePriority().Policy()}
	queued := []domain.Demand{tierDemand(1, 65*time.Minute, domain.Priority{Tier: "release", Rank: 1})}

	summary, err := coordinator.QueueSummary(context.Background(), Binding{ScaleSetID: 1}, queued)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Tiers) != 2 || summary.Tiers[0].Tier != "release" || summary.Tiers[0].Count != 2 {
		t.Fatalf("release row = %#v (all rows %#v)", summary.Tiers[0], summary.Tiers)
	}
	if !summary.Tiers[0].Oldest.Equal(tierNow.Add(-90 * time.Minute)) {
		t.Fatalf("release oldest = %s, want the REST-observed 90 minutes", summary.Tiers[0].Oldest)
	}
	if summary.Tiers[1].Tier != domain.DefaultPriorityTier || summary.Tiers[1].Count != 1 {
		t.Fatalf("default row = %#v", summary.Tiers[1])
	}
}

// TestQueueTiersOfEqualRankAreOrderedByName keeps the view deterministic when a
// configuration reload leaves two names sharing a rank: a map iteration must
// never decide what an operator reads.
func TestQueueTiersOfEqualRankAreOrderedByName(t *testing.T) {
	coordinator := DemandCoordinator{Store: &fakeDemandStore{}, Priority: releasePriority().Policy()}
	queued := []domain.Demand{
		tierDemand(1, time.Minute, domain.Priority{Tier: "zulu", Rank: 1}),
		tierDemand(2, time.Minute, domain.Priority{Tier: "alpha", Rank: 1}),
	}

	summary, err := coordinator.QueueSummary(context.Background(), Binding{ScaleSetID: 1}, queued)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Tiers) != 2 || summary.Tiers[0].Tier != "alpha" || summary.Tiers[1].Tier != "zulu" {
		t.Fatalf("tiers = %#v, want alpha before zulu", summary.Tiers)
	}
}
