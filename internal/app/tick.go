package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
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
	At      time.Time
	Plan    scheduler.Plan
	Applied bool
	Demands []domain.Demand
	Queues  map[domain.ProfileID]QueueSummary
	// ScopeQueues reports the same demand without collapsing the scope. The
	// per-profile aggregate above cannot distinguish an idle scope from a busy one
	// sharing its profile, which is the question an incident actually asks.
	ScopeQueues []ScopeQueue
	Instances   []domain.Instance
	HostMode    domain.HostMode
	Host        domain.Host
}

// commitFailureReason names why a commit failed. A non-ready plan never reached
// the durable write at all, so reporting it as a commit failure would send an
// operator to the database instead of to the inventory that produced the plan.
//
// Beyond that split the commit path itself reports four different incidents
// through one error value, and until now all four logged one token. A lost
// compare-and-set is routine and self-healing; a plan the durable layer refuses
// as malformed repeats forever and needs an operator; anything else is the
// store. Classify by the sentinel the durable layer already returns so the
// closed vocabulary keeps the promise its own doc comment makes.
func commitFailureReason(status scheduler.PlanStatus, err error) string {
	switch {
	case status != scheduler.PlanReady:
		return ReasonPlanInvalid
	case errors.Is(err, operations.ErrConflict):
		return ReasonPlanCommitContended
	case errors.Is(err, operations.ErrInvalid):
		return ReasonPlanCommitRejected
	default:
		return ReasonPlanCommitFailed
	}
}

// trickle degrades a fail-closed binding by admitting a single demand while
// statistics are unavailable. queued is age-sorted oldest-first, so it returns
// the oldest demand that has no live incarnation, letting the freshly spawned
// runner draw new broker statistics. Demands already served by a live instance
// are skipped rather than respawned; if every queued demand is already live it
// admits nothing, since there is no idle work to un-livelock.
func trickle(queued []domain.Demand, liveIncarnations map[domain.DemandKey]bool) []domain.Demand {
	for _, demand := range queued {
		if !liveIncarnations[demand.Key] {
			return []domain.Demand{demand}
		}
	}
	return nil
}

func (e Engine) Tick(ctx context.Context) (TickResult, error) {
	if e.Store == nil || e.Inventory == nil || e.ControllerID == "" || !e.Mode.Valid() {
		return TickResult{}, classifyTick(ReasonEngineInvalid, operations.ErrInvalid)
	}
	now := time.Now().UTC()
	if e.Now != nil {
		now = e.Now().UTC()
	}
	if now.IsZero() {
		return TickResult{}, classifyTick(ReasonEngineInvalid, operations.ErrInvalid)
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
			return TickResult{}, classifyTick(ReasonSchedulerStateReseedFailed, repairErr)
		}
		priorRecord, err = e.Store.SchedulerState(ctx)
	}
	if err != nil {
		return TickResult{}, classifyTick(ReasonSchedulerStateUnreadable, err)
	}
	var prior scheduler.State
	if len(priorRecord.Data) > 0 {
		if err := json.Unmarshal(priorRecord.Data, &prior); err != nil {
			return TickResult{}, classifyTick(ReasonSchedulerStateCorrupt, fmt.Errorf("decode scheduler state: %w", err))
		}
	}
	demands := make([]domain.Demand, 0)
	queues := make(map[domain.ProfileID]QueueSummary, len(e.Bindings))
	scopeQueues := make([]ScopeQueue, 0, len(e.Bindings))
	coordinator := e.Demand
	if coordinator.Store == nil {
		coordinator.Store = e.Store
	}
	if coordinator.Now == nil {
		coordinator.Now = func() time.Time { return now }
	}
	instances, host := e.Inventory.Observe(ctx)
	// Demand keys already owned by a live, non-terminal instance incarnation.
	// The statistics-fresh path never plans these — the Available/Assigned
	// bounds account for their assigned/running instances — but the degraded
	// trickle below bypasses those bounds entirely, so it needs this guard to
	// avoid double-spawning a demand whose runner is already live.
	liveIncarnations := make(map[domain.DemandKey]bool, len(instances.Value))
	for _, instance := range instances.Value {
		if instance.IncarnatesDemand() {
			liveIncarnations[instance.Demand] = true
		}
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
			//
			// Trickle the oldest demand that has no live incarnation: a demand
			// whose runner is already assigned (waiting on GitHub) must not be
			// respawned, or the plan collides with the live instance and every
			// recycled tick wedges the fleet. Skipping only advances to the
			// next candidate, so the binding still un-livelocks.
			schedulable = trickle(queued, liveIncarnations)
		} else if err != nil {
			return TickResult{}, classifyTick(ReasonDemandUnreadable, err)
		}
		demands = append(demands, schedulable...)
		summary, err := coordinator.QueueSummary(ctx, binding, queued)
		if err != nil {
			return TickResult{}, classifyTick(ReasonQueueSummaryUnreadable, err)
		}
		current := queues[binding.Profile.ID]
		current.Count += summary.Count
		if current.Oldest.IsZero() || (!summary.Oldest.IsZero() && summary.Oldest.Before(current.Oldest)) {
			current.Oldest = summary.Oldest
		}
		queues[binding.Profile.ID] = current
		scopeQueues = append(scopeQueues, ScopeQueue{Scope: binding.Scope, Profile: binding.Profile.ID,
			ScaleSetID: binding.ScaleSetID, Count: summary.Count, Oldest: summary.Oldest})
	}
	// Deterministic order so operators, JSON consumers, and replay fixtures never
	// depend on map or binding iteration order.
	sort.Slice(scopeQueues, func(i, j int) bool {
		if scopeQueues[i].Scope != scopeQueues[j].Scope {
			return scopeQueues[i].Scope < scopeQueues[j].Scope
		}
		if scopeQueues[i].Profile != scopeQueues[j].Profile {
			return scopeQueues[i].Profile < scopeQueues[j].Profile
		}
		return scopeQueues[i].ScaleSetID < scopeQueues[j].ScaleSetID
	})
	plan := scheduler.PlanTick(scheduler.Input{Now: now, Config: e.Config, Demands: domain.Fresh(demands, now), Instances: instances, Host: host, Prior: prior})
	applied, err := (reconcile.Controller{Store: e.Store, ControllerID: e.ControllerID, Mode: e.Mode, Profiles: e.Config.Profiles}).Commit(ctx, plan, "", now)
	mode, _ := domain.DeriveHostMode(instances.Value)
	return TickResult{At: now, Plan: plan, Applied: applied, Demands: append([]domain.Demand(nil), demands...), Queues: queues, ScopeQueues: scopeQueues,
			Instances: append([]domain.Instance(nil), instances.Value...), HostMode: mode, Host: host.Value},
		classifyTick(commitFailureReason(plan.Status, err), err)
}
