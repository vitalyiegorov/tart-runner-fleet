package app

import (
	"context"
	"encoding/json"
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
	Instances []domain.Instance
	HostMode  domain.HostMode
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
	coordinator := e.Demand
	if coordinator.Store == nil {
		coordinator.Store = e.Store
	}
	for _, binding := range e.Bindings {
		queued, err := coordinator.QueuedDemands(ctx, binding)
		if err != nil {
			return TickResult{}, err
		}
		demands = append(demands, queued...)
	}
	instances, host := e.Inventory.Observe(ctx)
	plan := scheduler.PlanTick(scheduler.Input{Now: now, Config: e.Config, Demands: domain.Fresh(demands, now), Instances: instances, Host: host, Prior: prior})
	applied, err := (reconcile.Controller{Store: e.Store, ControllerID: e.ControllerID, Mode: e.Mode, Profiles: e.Config.Profiles}).Commit(ctx, plan, "", now)
	mode, _ := domain.DeriveHostMode(instances.Value)
	return TickResult{At: now, Plan: plan, Applied: applied, Demands: append([]domain.Demand(nil), demands...),
		Instances: append([]domain.Instance(nil), instances.Value...), HostMode: mode}, err
}
