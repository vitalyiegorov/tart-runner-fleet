package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

type DemandStore interface {
	ApplyDemandBatch(context.Context, int64, int64, []operations.DemandEvent) (bool, error)
	ActiveDemands(context.Context, int64) ([]operations.DemandRecord, error)
}

type DemandProjector interface {
	ProjectDemandEvent(context.Context, int64, operations.DemandEvent) error
}

type MessageSource interface {
	Handle(context.Context, func(context.Context, githubscaleset.Demand) error) error
}

type Binding struct {
	StoreKey   int64
	ScaleSetID int64
	Scope      string
	Targets    []string
	Profile    domain.Profile
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

type DemandCoordinator struct {
	Store     DemandStore
	Projector DemandProjector
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
		events := make([]operations.DemandEvent, 0, len(demand.Events))
		for _, event := range demand.Events {
			if !binding.accepts(event.Owner + "/" + event.Repository) {
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
		EventName: event.EventName, Labels: append([]string(nil), event.Labels...), QueueTime: event.QueueTime,
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
	result := make([]domain.Demand, 0, len(records))
	for _, record := range records {
		if record.Status != operations.DemandJobAvailable {
			continue
		}
		if !binding.accepts(record.Owner + "/" + record.Repository) {
			continue
		}
		if record.RunnerRequestID <= 0 || record.WorkflowRunID <= 0 || record.Owner == "" || record.Repository == "" || record.QueueTime.IsZero() {
			return nil, fmt.Errorf("incomplete durable demand %d: %w", record.RunnerRequestID, operations.ErrUncertain)
		}
		result = append(result, domain.Demand{
			Key:       domain.DemandKey{Repo: record.Owner + "/" + record.Repository, RunID: record.WorkflowRunID, Attempt: 1, JobID: record.RunnerRequestID},
			CreatedAt: record.QueueTime.UTC(), Profile: binding.Profile.ID, Route: binding.Profile.Route, Platform: binding.Profile.Platform,
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
