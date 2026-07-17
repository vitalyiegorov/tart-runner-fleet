package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

type DemandStore interface {
	ApplyDemandBatch(context.Context, int64, int64, []operations.DemandEvent) (bool, error)
	ActiveDemands(context.Context, int64) ([]operations.DemandRecord, error)
}

type DemandStatisticsStore interface {
	PutDemandStatistics(context.Context, int64, operations.DemandStatistics) (bool, error)
	DemandStatistics(context.Context, int64) (operations.DemandStatistics, error)
}

type GitHubJobStore interface {
	ReconcileGitHubJobs(context.Context, int64, time.Time, []operations.GitHubJobObservation) (bool, error)
	QueuedGitHubJobs(context.Context, int64) ([]operations.GitHubJobObservation, error)
}

type GitHubQueueSnapshot interface {
	ObservedAt() time.Time
	QueuedJobs() []githubscaleset.WorkflowJob
}

type DemandProjector interface {
	ProjectDemandEvent(context.Context, int64, operations.DemandEvent) error
}

type MessageSource interface {
	Handle(context.Context, func(context.Context, githubscaleset.Demand) error) error
}

type Binding struct {
	StoreKey       int64
	ScaleSetID     int64
	Scope          string
	Targets        []string
	RequiredLabels []string
	Profile        domain.Profile
}

func (b Binding) valid() bool {
	return b.durableKey() > 0 && b.ScaleSetID > 0 && b.Profile.ID != "" && b.Profile.Route != "" &&
		(b.Profile.Platform == domain.PlatformLinux || b.Profile.Platform == domain.PlatformMacOS)
}

func (b Binding) durableKey() int64 {
	if b.StoreKey > 0 {
		return b.StoreKey
	}
	return b.ScaleSetID
}

func (b Binding) accepts(repo string) bool {
	if len(b.Targets) == 0 {
		return true
	}
	for _, target := range b.Targets {
		if target == repo {
			return true
		}
	}
	return false
}

func (b Binding) acceptsLabels(labels []string) bool {
	for _, required := range b.RequiredLabels {
		found := false
		for _, label := range labels {
			if label == required {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

type DemandCoordinator struct {
	Store     DemandStore
	Projector DemandProjector
}

type QueueSummary struct {
	Count  int
	Oldest time.Time
}

func (c DemandCoordinator) QueueSummary(ctx context.Context, binding Binding, executable []domain.Demand) (QueueSummary, error) {
	summary := QueueSummary{Count: len(executable)}
	for _, demand := range executable {
		if summary.Oldest.IsZero() || demand.CreatedAt.Before(summary.Oldest) {
			summary.Oldest = demand.CreatedAt
		}
	}
	store, ok := c.Store.(GitHubJobStore)
	if !ok {
		return summary, nil
	}
	jobs, err := store.QueuedGitHubJobs(ctx, binding.durableKey())
	if err != nil {
		return QueueSummary{}, err
	}
	if len(jobs) > summary.Count {
		summary.Count = len(jobs)
	}
	for _, job := range jobs {
		if summary.Oldest.IsZero() || job.CreatedAt.Before(summary.Oldest) {
			summary.Oldest = job.CreatedAt
		}
	}
	return summary, nil
}

// ReconcileQueuedJobs adds REST's complete queue view without granting it
// lifecycle authority. Jobs are routed through the same scope and profile
// predicates as scale-set demand.
func (c DemandCoordinator) ReconcileQueuedJobs(ctx context.Context, bindings []Binding, snapshot GitHubQueueSnapshot) (bool, error) {
	store, ok := c.Store.(GitHubJobStore)
	if !ok || snapshot == nil || snapshot.ObservedAt().IsZero() {
		return false, operations.ErrInvalid
	}
	queued := snapshot.QueuedJobs()
	changed := false
	for _, binding := range bindings {
		if !binding.valid() {
			return false, operations.ErrInvalid
		}
		observations := make([]operations.GitHubJobObservation, 0)
		for _, job := range queued {
			if !binding.accepts(job.Repository.Owner+"/"+job.Repository.Name) || !binding.acceptsLabels(job.Labels) ||
				!containsFold(job.Labels, string(binding.Profile.Route)) {
				continue
			}
			observations = append(observations, operations.GitHubJobObservation{WorkflowJobID: job.ID,
				Owner: job.Repository.Owner, Repository: job.Repository.Name, WorkflowRunID: job.RunID,
				RunAttempt: job.RunAttempt, DisplayName: job.Name, Labels: append([]string(nil), job.Labels...),
				Status: job.Status, CreatedAt: job.CreatedAt})
		}
		applied, err := store.ReconcileGitHubJobs(ctx, binding.durableKey(), snapshot.ObservedAt(), observations)
		changed = changed || applied
		if err != nil {
			return changed, err
		}
	}
	return changed, nil
}

func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
}

func (c DemandCoordinator) IngestOnce(ctx context.Context, binding Binding, source MessageSource) error {
	_, err := c.IngestOnceResult(ctx, binding, source)
	return err
}

// IngestOnceResult reports whether the source durably committed previously
// unseen demand before it returned. The boolean remains true when a later
// acknowledgement fails, allowing reconciliation to wake for already-durable
// work while message redelivery is retried independently.
func (c DemandCoordinator) IngestOnceResult(ctx context.Context, binding Binding, source MessageSource) (bool, error) {
	if c.Store == nil || source == nil || !binding.valid() {
		return false, operations.ErrInvalid
	}
	changed := false
	err := source.Handle(ctx, func(ctx context.Context, demand githubscaleset.Demand) error {
		if demand.Statistics.MessageID > 0 {
			statisticsStore, ok := c.Store.(DemandStatisticsStore)
			if !ok {
				return fmt.Errorf("demand statistics store unavailable: %w", operations.ErrUncertain)
			}
			statisticsChanged, err := statisticsStore.PutDemandStatistics(ctx, binding.durableKey(), operations.DemandStatistics{
				MessageID: int64(demand.Statistics.MessageID), Available: demand.Statistics.Available,
				Acquired: demand.Statistics.Acquired, Assigned: demand.Statistics.Assigned, Running: demand.Statistics.Running,
				Registered: demand.Statistics.Registered, Busy: demand.Statistics.Busy, Idle: demand.Statistics.Idle,
			})
			changed = changed || statisticsChanged
			if err != nil {
				return err
			}
		}
		events := make([]operations.DemandEvent, 0, len(demand.Events))
		for _, event := range demand.Events {
			if !binding.accepts(event.Owner+"/"+event.Repository) || !binding.acceptsLabels(event.Labels) {
				continue
			}
			events = append(events, convertEvent(event))
		}
		applied, err := c.Store.ApplyDemandBatch(ctx, binding.durableKey(), int64(demand.MessageID), events)
		changed = changed || applied
		if err != nil {
			return err
		}
		projector := c.Projector
		if projector == nil {
			projector, _ = c.Store.(DemandProjector)
		}
		if projector != nil {
			for _, event := range events {
				if err := projector.ProjectDemandEvent(ctx, binding.durableKey(), event); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return changed, err
}

func convertEvent(event githubscaleset.JobEvent) operations.DemandEvent {
	return operations.DemandEvent{Kind: operations.DemandEventKind(event.Kind), RunnerRequestID: event.RunnerRequestID,
		Owner: event.Owner, Repository: event.Repository, WorkflowRunID: event.WorkflowRunID, JobID: event.JobID,
		DisplayName: event.DisplayName, WorkflowRef: event.WorkflowRef, EventName: event.EventName,
		Labels: append([]string(nil), event.Labels...), QueueTime: event.QueueTime,
		RunnerID: event.RunnerID, RunnerName: event.RunnerName, Result: event.Result}
}

func (c DemandCoordinator) QueuedDemands(ctx context.Context, binding Binding) ([]domain.Demand, error) {
	if c.Store == nil || !binding.valid() {
		return nil, operations.ErrInvalid
	}
	records, err := c.Store.ActiveDemands(ctx, binding.durableKey())
	if err != nil {
		return nil, err
	}
	// A scale-set message may rotate request IDs for one logical workflow job.
	// Keep only its newest actionable request while preserving canonical age.
	canonical := make(map[string]operations.DemandRecord, len(records))
	for _, record := range records {
		if record.Status != operations.DemandJobAvailable {
			continue
		}
		if !binding.accepts(record.Owner+"/"+record.Repository) || !binding.acceptsLabels(record.Labels) {
			continue
		}
		createdAt := record.FirstQueueTime
		if createdAt.IsZero() {
			createdAt = record.QueueTime
		}
		if record.RunnerRequestID <= 0 || record.WorkflowRunID <= 0 || record.Owner == "" || record.Repository == "" || createdAt.IsZero() {
			return nil, fmt.Errorf("incomplete durable demand %d: %w", record.RunnerRequestID, operations.ErrUncertain)
		}
		key := record.LogicalKey
		if key == "" {
			key = fmt.Sprintf("request:%d", record.RunnerRequestID)
		}
		previous, exists := canonical[key]
		if !exists || record.UpdatedAt.After(previous.UpdatedAt) || (record.UpdatedAt.Equal(previous.UpdatedAt) && record.RunnerRequestID > previous.RunnerRequestID) {
			canonical[key] = record
		}
	}
	selected := make([]operations.DemandRecord, 0, len(canonical))
	for _, record := range canonical {
		selected = append(selected, record)
	}
	sort.Slice(selected, func(i, j int) bool {
		left, right := selected[i].FirstQueueTime, selected[j].FirstQueueTime
		if left.IsZero() {
			left = selected[i].QueueTime
		}
		if right.IsZero() {
			right = selected[j].QueueTime
		}
		if left.Equal(right) {
			return selected[i].RunnerRequestID < selected[j].RunnerRequestID
		}
		return left.Before(right)
	})
	if statisticsStore, ok := c.Store.(DemandStatisticsStore); ok {
		statistics, statisticsErr := statisticsStore.DemandStatistics(ctx, binding.durableKey())
		if statisticsErr != nil && !errors.Is(statisticsErr, operations.ErrNotFound) {
			return nil, statisticsErr
		}
		if statisticsErr == nil && statistics.Valid() {
			normalLimit := statistics.Available
			preassignedLimit := statistics.Assigned - statistics.Running
			if preassignedLimit < 0 {
				preassignedLimit = 0
			}
			bounded := make([]operations.DemandRecord, 0, min(len(selected), normalLimit+preassignedLimit))
			for _, record := range selected {
				if githubscaleset.IsPreassignedRequestID(record.RunnerRequestID) {
					if preassignedLimit == 0 {
						continue
					}
					preassignedLimit--
				} else {
					if normalLimit == 0 {
						continue
					}
					normalLimit--
				}
				bounded = append(bounded, record)
			}
			selected = bounded
		}
	}
	result := make([]domain.Demand, 0, len(selected))
	for _, record := range selected {
		createdAt := record.FirstQueueTime
		if createdAt.IsZero() {
			createdAt = record.QueueTime
		}
		attempt := record.RunAttempt
		if attempt <= 0 {
			attempt = 1
		}
		result = append(result, domain.Demand{
			Key:       domain.DemandKey{Repo: record.Owner + "/" + record.Repository, RunID: record.WorkflowRunID, Attempt: attempt, JobID: record.RunnerRequestID},
			CreatedAt: createdAt.UTC(), Profile: binding.Profile.ID, Route: binding.Profile.Route, Platform: binding.Profile.Platform,
			Event: event(record.EventName), RunStatus: domain.RunQueued,
		})
	}
	return result, nil
}

func event(name string) domain.Event {
	switch strings.ToLower(name) {
	case "pull_request", "pull_request_target", "merge_group":
		return domain.EventPullRequest
	case "schedule":
		return domain.EventSchedule
	default:
		return domain.EventPush
	}
}
