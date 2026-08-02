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
	ReconcileGitHubJobSnapshot(context.Context, time.Time, map[int64][]operations.GitHubJobObservation) (bool, error)
	QueuedGitHubJobs(context.Context, int64) ([]operations.GitHubJobObservation, error)
}

// GhostDemandStore retires demand that complete REST snapshots have proven
// absent. It is optional: a store without it keeps every durable demand, which
// is the pre-issue-#113 behaviour and always the safe direction.
type GhostDemandStore interface {
	ExpireGhostDemands(context.Context, int64, operations.GhostDemandCriteria) (int64, error)
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
	ScaleSetLabels []string
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
		if strings.EqualFold(target, repo) {
			return true
		}
	}
	return false
}

func (b Binding) acceptsLabels(labels []string) bool {
	for _, required := range b.RequiredLabels {
		found := false
		for _, label := range labels {
			if strings.EqualFold(label, required) {
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

// matchesRESTLabels mirrors GitHub's scale-set label compatibility rule: a
// queued job is eligible when every label it requests is advertised by the
// scale set. RequiredLabels is an additional controller-side isolation guard
// used by exact-scope canaries. Bindings constructed by older callers without
// ScaleSetLabels retain the narrow profile-route fallback.
func (b Binding) matchesRESTLabels(labels []string) bool {
	if !b.acceptsLabels(labels) {
		return false
	}
	if len(b.ScaleSetLabels) == 0 {
		return containsFold(labels, string(b.Profile.Route))
	}
	for _, requested := range labels {
		if !containsFold(b.ScaleSetLabels, requested) {
			return false
		}
	}
	return true
}

type DemandCoordinator struct {
	Store            DemandStore
	Projector        DemandProjector
	Now              func() time.Time
	StatisticsMaxAge time.Duration
	GhostAbsence     time.Duration
	StrictJobRouting bool
}

const defaultStatisticsMaxAge = 2 * time.Minute

// defaultGhostAbsence and minGhostObservations bound how much REST evidence
// retires demand GitHub still advertises. The REST scope is polled at most
// every 30s, so the default window is dozens of complete snapshots in which a
// genuinely queued job would have appeared; the minimum observation count is
// the independent second bound that keeps one lonely snapshot after a long
// outage from concluding anything on its own.
const (
	defaultGhostAbsence  = 15 * time.Minute
	minGhostObservations = 3
)

func (c DemandCoordinator) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

func (c DemandCoordinator) statisticsMaxAge() time.Duration {
	if c.StatisticsMaxAge > 0 {
		return c.StatisticsMaxAge
	}
	return defaultStatisticsMaxAge
}

func (c DemandCoordinator) ghostAbsence() time.Duration {
	if c.GhostAbsence > 0 {
		return c.GhostAbsence
	}
	return defaultGhostAbsence
}

// ErrDemandStatisticsUnavailable marks a binding whose scale-set statistics
// are missing, stale, or ahead of the clock. Broker statistics arrive only
// with message activity, so a quiet queue ages past any freshness budget
// while its demand is still durable; callers must fail closed for that
// binding alone instead of failing the whole reconciliation tick, or the
// scheduler deadlocks fleet-wide until new messages arrive (issue #67).
// It wraps operations.ErrUncertain so existing uncertainty handling holds.
var ErrDemandStatisticsUnavailable = fmt.Errorf("demand statistics unavailable: %w", operations.ErrUncertain)

type QueueSummary struct {
	Count  int
	Oldest time.Time
}

// ScopeQueue is one binding's queue depth attributed to the scope and scale set
// that own it, so an idle scope stays distinguishable from a busy one sharing the
// same profile.
type ScopeQueue struct {
	Scope      string
	Profile    domain.ProfileID
	ScaleSetID int64
	Count      int
	Oldest     time.Time
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
		if job.QueueTimeExact && (summary.Oldest.IsZero() || job.CreatedAt.Before(summary.Oldest)) {
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
	observations := make(map[int64][]operations.GitHubJobObservation, len(bindings))
	for _, binding := range bindings {
		if !binding.valid() {
			return false, operations.ErrInvalid
		}
		observations[binding.durableKey()] = nil
	}
	for _, job := range snapshot.QueuedJobs() {
		var matched *Binding
		for i := range bindings {
			binding := &bindings[i]
			if !binding.accepts(job.Repository.Owner+"/"+job.Repository.Name) || !binding.matchesRESTLabels(job.Labels) {
				continue
			}
			if matched != nil {
				return false, fmt.Errorf("GitHub job %d matches scale sets %d and %d: %w",
					job.ID, matched.ScaleSetID, binding.ScaleSetID, operations.ErrConflict)
			}
			matched = binding
		}
		if matched == nil {
			if c.StrictJobRouting && containsFold(job.Labels, "self-hosted") {
				return false, fmt.Errorf("self-hosted GitHub job %d matches no configured scale set: %w", job.ID, operations.ErrUncertain)
			}
			continue
		}
		key := matched.durableKey()
		observations[key] = append(observations[key], operations.GitHubJobObservation{WorkflowJobID: job.ID,
			Owner: job.Repository.Owner, Repository: job.Repository.Name, WorkflowRunID: job.RunID,
			RunAttempt: job.RunAttempt, DisplayName: job.Name, Labels: append([]string(nil), job.Labels...),
			Status: job.Status, CreatedAt: job.CreatedAt, QueueTimeExact: job.QueueTimeExact})
	}
	changed, err := store.ReconcileGitHubJobSnapshot(ctx, snapshot.ObservedAt(), observations)
	if err != nil {
		return changed, err
	}
	expired, err := c.expireGhostDemand(ctx, bindings, snapshot.ObservedAt())
	return changed || expired, err
}

// expireGhostDemand retires demand this snapshot has now proven absent for
// longer than the ghost window. It runs only behind a committed, complete REST
// scope observation, so freshness is a property of the caller rather than a
// clock comparison: the same snapshot that failed to find the job is the one
// that dates the conclusion.
func (c DemandCoordinator) expireGhostDemand(ctx context.Context, bindings []Binding, observedAt time.Time) (bool, error) {
	store, ok := c.Store.(GhostDemandStore)
	if !ok {
		return false, nil
	}
	observed := observedAt.UTC()
	criteria := operations.GhostDemandCriteria{ObservedAt: observed, AbsentBefore: observed.Add(-c.ghostAbsence()),
		MinObservations: minGhostObservations}
	expired := false
	for _, binding := range bindings {
		count, err := store.ExpireGhostDemands(ctx, binding.durableKey(), criteria)
		if err != nil {
			return expired, err
		}
		expired = expired || count > 0
	}
	return expired, nil
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
				ObservedAt: c.now(),
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
	if len(selected) == 0 {
		return []domain.Demand{}, nil
	}
	statisticsStore, ok := c.Store.(DemandStatisticsStore)
	if !ok {
		return nil, fmt.Errorf("scale-set statistics unavailable: %w", operations.ErrUncertain)
	}
	statistics, statisticsErr := statisticsStore.DemandStatistics(ctx, binding.durableKey())
	if statisticsErr != nil {
		if errors.Is(statisticsErr, operations.ErrNotFound) {
			return convertDemandRecords(binding, selected), fmt.Errorf("scale-set statistics not observed: %w", ErrDemandStatisticsUnavailable)
		}
		return nil, statisticsErr
	}
	now := c.now()
	if !statistics.Valid() || statistics.ObservedAt.IsZero() || statistics.ObservedAt.After(now) ||
		now.Sub(statistics.ObservedAt) > c.statisticsMaxAge() {
		return convertDemandRecords(binding, selected), fmt.Errorf("scale-set statistics are stale or invalid: %w", ErrDemandStatisticsUnavailable)
	}
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
	return convertDemandRecords(binding, bounded), nil
}

func convertDemandRecords(binding Binding, records []operations.DemandRecord) []domain.Demand {
	result := make([]domain.Demand, 0, len(records))
	for _, record := range records {
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
	return result
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
