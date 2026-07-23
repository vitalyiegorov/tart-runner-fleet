package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/macos"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/tart"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

type LiveInstanceStore interface {
	LiveInstances(context.Context) ([]operations.Instance, error)
}
type TartInventory interface {
	List(context.Context) ([]tart.VM, error)
}
type HostInventory interface {
	Snapshot(context.Context) macos.Snapshot
}
type RecoveryObserver interface {
	ConfirmDeletion(context.Context, string) (operations.DeletionConfirmation, error)
}

type ProductionInventory struct {
	Store                      LiveInstanceStore
	Tart                       TartInventory
	Host                       HostInventory
	Recovery                   RecoveryObserver
	RecoveryConfirmationMaxAge time.Duration
	Capacity                   domain.Resources
	Guards                     macos.Guardrails
}

func (p ProductionInventory) Observe(ctx context.Context) (domain.Observation[[]domain.Instance], domain.Observation[domain.Host]) {
	if p.Store == nil || p.Tart == nil || p.Host == nil {
		return domain.Unavailable[[]domain.Instance]("inventory adapters are required"), domain.Unavailable[domain.Host]("inventory adapters are required")
	}
	now := time.Now().UTC()
	hostSnapshot := p.Host.Snapshot(ctx)
	host := hostObservation(hostSnapshot, p.Capacity, p.Guards)
	stored, err := p.Store.LiveInstances(ctx)
	if err != nil {
		return domain.Unavailable[[]domain.Instance]("durable instance inventory unavailable"), host
	}
	vms, err := p.Tart.List(ctx)
	if err != nil {
		return domain.Unavailable[[]domain.Instance]("Tart inventory unavailable"), host
	}
	byName := make(map[string]tart.VM, len(vms))
	for _, vm := range vms {
		byName[vm.Name] = vm
	}
	result := make([]domain.Instance, 0, len(stored))
	for _, instance := range stored {
		if !instance.SchedulingMetadataValid() || instance.Repo == "" {
			return domain.Unavailable[[]domain.Instance]("invalid durable scheduling metadata"), host
		}
		vm, exists := byName[instance.ID]
		if !exists && instance.State != operations.StatePlanned {
			return domain.Unavailable[[]domain.Instance](fmt.Sprintf("owned VM %s missing from Tart", instance.ID)), host
		}
		delete(byName, instance.ID)
		power := domain.InstancePowerUnknown
		if exists && vm.Running {
			power = domain.InstancePowerRunning
		} else if exists {
			power = domain.InstancePowerStopped
		}
		recoveryReady := false
		if power == domain.InstancePowerRunning && p.Recovery != nil &&
			(instance.State == operations.StateAssigned || instance.State == operations.StateRunning) {
			confirmation, confirmationErr := p.Recovery.ConfirmDeletion(ctx, instance.ID)
			if confirmationErr == nil {
				recoveryReady = confirmation.Safe(now, p.recoveryConfirmationMaxAge())
			}
		}
		// An Assigned instance advances to Running only when a JobStarted event
		// arrives, so its dwell time in Assigned is the age the scheduler measures
		// against the assignment deadline. UpdatedAt marks entry into the state.
		assignedSince := time.Time{}
		if instance.State == operations.StateAssigned {
			assignedSince = instance.UpdatedAt
		}
		result = append(result, domain.Instance{ID: instance.ID, Repo: instance.Repo, Demand: instance.Demand, Platform: instance.Platform, Profile: instance.Profile,
			Route: instance.Route, Resources: instance.Resources, State: instance.State, Power: power, RecoveryReady: recoveryReady,
			AssignedSince: assignedSince})
	}
	for name := range byName {
		if strings.HasPrefix(name, "trf-") {
			return domain.Unavailable[[]domain.Instance]("untracked controller VM requires reconciliation"), host
		}
	}
	return domain.Fresh(result, now), host
}

func (p ProductionInventory) recoveryConfirmationMaxAge() time.Duration {
	if p.RecoveryConfirmationMaxAge <= 0 {
		return 30 * time.Second
	}
	return p.RecoveryConfirmationMaxAge
}

func hostObservation(snapshot macos.Snapshot, capacity domain.Resources, guards macos.Guardrails) domain.Observation[domain.Host] {
	switch snapshot.Freshness {
	case macos.Fresh:
		// Continue with explicit guardrails below.
	case macos.Stale:
		return domain.Stale(domain.Host{}, snapshot.ObservedAt, "host probe is stale")
	case macos.Unavailable:
		return domain.Unavailable[domain.Host]("host probe unavailable")
	default:
		return domain.Unavailable[domain.Host]("host probe freshness is invalid")
	}
	decision := guards.Evaluate(snapshot, macos.Request{})
	pressure := domain.HostPressure{AvailableMemoryMB: snapshot.AvailableMemoryMB, FreeDiskGB: snapshot.FreeDiskGB,
		SwapUsedMB: snapshot.SwapUsedMB, SwapOuts: snapshot.SwapOuts, CPUIdlePercent: snapshot.CPUidlePercent,
		LoadAverage: snapshot.LoadAverage, AdmissionAllowed: decision.Allowed, AdmissionReason: decision.Reason}
	if !decision.Allowed {
		observation := domain.Fresh(domain.Host{Pressure: pressure}, snapshot.ObservedAt)
		observation.Reason = decision.Reason
		return observation
	}
	availableMemory := max(0, min(int(snapshot.AvailableMemoryMB-guards.MinAvailableMemoryMB), capacity.MemoryMB))
	return domain.Fresh(domain.Host{Available: domain.Resources{CPU: capacity.CPU, MemoryMB: availableMemory, Slots: capacity.Slots},
		Pressure: pressure}, snapshot.ObservedAt)
}
