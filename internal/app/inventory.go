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
	// JobActive reports whether the durable demand bound to a Running instance
	// shows a job currently executing. Its negation is the lingering-runner
	// evidence the scheduler measures against the idle-runner deadline.
	JobActive(context.Context, operations.Instance) (bool, error)
}

type ProductionInventory struct {
	Store                      LiveInstanceStore
	Tart                       TartInventory
	Host                       HostInventory
	Recovery                   RecoveryObserver
	RecoveryConfirmationMaxAge time.Duration
	Capacity                   domain.Resources
	Guards                     macos.Guardrails
	// ElasticHostEnvelope reports the physical machine and measured idle CPU so
	// the scheduler can size the fleet against the host it shares rather than a
	// static constant. Default false preserves the configured-envelope model.
	ElasticHostEnvelope bool
}

func (p ProductionInventory) Observe(ctx context.Context) (domain.Observation[[]domain.Instance], domain.Observation[domain.Host]) {
	if p.Store == nil || p.Tart == nil || p.Host == nil {
		return domain.Unavailable[[]domain.Instance]("inventory adapters are required"), domain.Unavailable[domain.Host]("inventory adapters are required")
	}
	now := time.Now().UTC()
	hostSnapshot := p.Host.Snapshot(ctx)
	host := hostObservation(hostSnapshot, p.Capacity, p.Guards, p.ElasticHostEnvelope)
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
		// A Running instance's dwell time is the age measured against the
		// idle-runner deadline; JobInactive is the evidence that pairs with it —
		// no active job on the durable demand. Both stay zero/false (fail-closed)
		// for non-running instances or when the demand evidence cannot be read.
		runningSince := time.Time{}
		jobInactive := false
		if instance.State == operations.StateRunning {
			runningSince = instance.UpdatedAt
			if power == domain.InstancePowerRunning && p.Recovery != nil {
				if active, activeErr := p.Recovery.JobActive(ctx, instance); activeErr == nil {
					jobInactive = !active
				}
			}
		}
		result = append(result, domain.Instance{ID: instance.ID, Repo: instance.Repo, Demand: instance.Demand, Platform: instance.Platform, Profile: instance.Profile,
			Route: instance.Route, Resources: instance.Resources, State: instance.State, Power: power, RecoveryReady: recoveryReady,
			AssignedSince: assignedSince, RunningSince: runningSince, JobInactive: jobInactive})
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

// hostObservation converts a host probe into the scheduler's admission facts.
//
// When elastic is false the CPU dimension echoes the configured capacity and no
// physical total is advertised, preserving the static envelope byte-for-byte.
// When true the observation additionally reports the real machine so aggregate
// reservations can be bounded by it, and derives available CPU from measured
// idle so the fleet yields as the host's own tenant gets busy.
func hostObservation(snapshot macos.Snapshot, capacity domain.Resources, guards macos.Guardrails, elastic bool) domain.Observation[domain.Host] {
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
	available := domain.Resources{CPU: capacity.CPU, MemoryMB: availableMemory, Slots: capacity.Slots}
	physical := domain.Resources{}
	if elastic {
		// The measured residual memory figure is not capped by the configured
		// envelope here: in elastic mode the physical total below is the bound, and
		// clamping to configuration is what prevented the fleet from using the
		// machine it actually runs on.
		available.MemoryMB = max(0, int(snapshot.AvailableMemoryMB-guards.MinAvailableMemoryMB))
		physical = physicalCapacity(snapshot, capacity, guards)
		if physical.CPU > 0 {
			available.CPU = idleCores(snapshot)
		}
	}
	return domain.Fresh(domain.Host{Available: available, Capacity: physical, Pressure: pressure}, snapshot.ObservedAt)
}

// physicalCapacity reports the machine's real totals as an admission bound.
// Dimensions the probe could not read stay zero, which the scheduler reads as
// not-observed and ignores, so an unreadable fact degrades to the configured
// envelope instead of closing admission. Slots have no physical analogue and
// stay configured.
func physicalCapacity(snapshot macos.Snapshot, capacity domain.Resources, guards macos.Guardrails) domain.Resources {
	physical := domain.Resources{Slots: capacity.Slots}
	if snapshot.PhysicalCPU > 0 {
		physical.CPU = int(snapshot.PhysicalCPU)
	}
	if snapshot.PhysicalMemoryMB > 0 {
		physical.MemoryMB = max(0, int(snapshot.PhysicalMemoryMB-guards.MinAvailableMemoryMB))
	}
	return physical
}

// idleCores converts measured CPU idle into whole free cores, truncating so a
// partially busy core is never advertised as available. This is what makes the
// fleet a second pilot: it claims only the share the host is demonstrably not
// using, and drops to zero on a saturated machine.
func idleCores(snapshot macos.Snapshot) int {
	idle := snapshot.CPUidlePercent
	if idle <= 0 {
		return 0
	}
	if idle > 100 {
		idle = 100
	}
	return int(float64(snapshot.PhysicalCPU) * idle / 100)
}
