package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/reconcile"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

type EngineStore interface {
	DemandStore
	reconcile.PlanStore
	ReseedSchedulerState(context.Context) error
}

type Inventory interface {
	Observe(context.Context) (domain.Observation[[]domain.Instance], domain.Observation[domain.Host])
}

type Engine struct {
	Store        EngineStore
	Demand       DemandCoordinator
	Inventory    Inventory
	Config       scheduler.Config
	Bindings     []Binding
	ControllerID string
	Mode         reconcile.Mode
	Now          func() time.Time
}

type TickResult struct {
	At        time.Time
	Plan      scheduler.Plan
	Applied   bool
	Demands   []domain.Demand
	Queues    map[domain.ProfileID]QueueSummary
	Instances []domain.Instance
	HostMode  domain.HostMode
	Host      domain.Host
}

func (e Engine) Tick(ctx context.Context) (TickResult, error) {
	if e.Store == nil || e.Inventory == nil || e.ControllerID == "" || !e.Mode.Valid() {
		return TickResult{}, operations.ErrInvalid
	}
	now := time.Now().UTC()
	if e.Now != nil {
		now = e.Now().UTC()
	}
	if now.IsZero() {
		return TickResult{}, operations.ErrInvalid
	}
	priorRecord, err := e.Store.SchedulerState(ctx)
	if errors.Is(err, operations.ErrSchedulerStateMissing) {
		// Incident 2026-07-22: an operator DELETEd the seeded scheduler_state
		// singleton, and every tick then failed forever on the missing row.
		// Reseed the cold-start row once and retry the read so this tick recovers
		// instead of wedging the fleet. Version 0 with empty reservation/DRR is
		// safe optimization-state loss the scheduler rebuilds from durable demand;
		// authoritative instance and operation state is never resynthesized here.
		if repairErr := e.Store.ReseedSchedulerState(ctx); repairErr != nil {
			return TickResult{}, repairErr
		}
		priorRecord, err = e.Store.SchedulerState(ctx)
	}
	if err != nil {
		return TickResult{}, err
	}
	var prior scheduler.State
	if len(priorRecord.Data) > 0 {
		if err := json.Unmarshal(priorRecord.Data, &prior); err != nil {
			return TickResult{}, fmt.Errorf("decode scheduler state: %w", err)
		}
	}
	demands := make([]domain.Demand, 0)
	queues := make(map[domain.ProfileID]QueueSummary, len(e.Bindings))
	coordinator := e.Demand
	if coordinator.Store == nil {
		coordinator.Store = e.Store
	}
	if coordinator.Now == nil {
		coordinator.Now = func() time.Time { return now }
	}
	for _, binding := range e.Bindings {
		queued, err := coordinator.QueuedDemands(ctx, binding)
		schedulable := queued
		if errors.Is(err, ErrDemandStatisticsUnavailable) {
			// Statistics refresh only with new broker messages, and a stalled
			// queue never sends any, so a fully fail-closed binding livelocks
			// itself: durable demand waits forever on statistics that need the
			// demand to run first. A durable JobAvailable demand is itself
			// evidence of capacity need, so trickle the oldest single demand
			// per tick — the spawned runner picks the job up, the broker
			// delivers fresh statistics, and the binding leaves degradation.
			// The queue stays fully visible to the SLO monitor either way.
			schedulable = queued[:min(1, len(queued))]
		} else if err != nil {
			return TickResult{}, err
		}
		demands = append(demands, schedulable...)
		summary, err := coordinator.QueueSummary(ctx, binding, queued)
		if err != nil {
			return TickResult{}, err
		}
		current := queues[binding.Profile.ID]
		current.Count += summary.Count
		if current.Oldest.IsZero() || (!summary.Oldest.IsZero() && summary.Oldest.Before(current.Oldest)) {
			current.Oldest = summary.Oldest
		}
		queues[binding.Profile.ID] = current
	}
	instances, host := e.Inventory.Observe(ctx)
	plan := scheduler.PlanTick(scheduler.Input{Now: now, Config: e.Config, Demands: domain.Fresh(demands, now), Instances: instances, Host: host, Prior: prior})
	applied, err := (reconcile.Controller{Store: e.Store, ControllerID: e.ControllerID, Mode: e.Mode, Profiles: e.Config.Profiles}).Commit(ctx, plan, "", now)
	mode, _ := domain.DeriveHostMode(instances.Value)
	return TickResult{At: now, Plan: plan, Applied: applied, Demands: append([]domain.Demand(nil), demands...), Queues: queues,
		Instances: append([]domain.Instance(nil), instances.Value...), HostMode: mode, Host: host.Value}, err
}
