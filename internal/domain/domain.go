// Package domain contains the side-effect-free vocabulary shared by the fleet
// scheduler and its adapters.
package domain

import (
	"errors"
	"fmt"
	"time"
)

// DemandKey identifies one concrete GitHub Actions job attempt. Run IDs alone
// are intentionally insufficient because sibling jobs and reruns coexist.
type DemandKey struct {
	Repo    string
	RunID   int64
	Attempt int
	JobID   int64
}

func (k DemandKey) String() string {
	return fmt.Sprintf("%s/%d/%d/%d", k.Repo, k.RunID, k.Attempt, k.JobID)
}

func (k DemandKey) Validate() error {
	if k.Repo == "" || k.RunID <= 0 || k.Attempt <= 0 || k.JobID <= 0 {
		return errors.New("demand key requires repo and positive run, attempt, and job IDs")
	}
	return nil
}

type ObservationState string

const (
	ObservationFresh       ObservationState = "fresh"
	ObservationStale       ObservationState = "stale"
	ObservationUnavailable ObservationState = "unavailable"
)

// Observation forces callers to distinguish known empty state from stale or
// unavailable state. Only fresh observations are safe scheduling inputs.
type Observation[T any] struct {
	State      ObservationState
	Value      T
	ObservedAt time.Time
	Reason     string
}

func Fresh[T any](value T, observedAt time.Time) Observation[T] {
	return Observation[T]{State: ObservationFresh, Value: value, ObservedAt: observedAt}
}

func Stale[T any](value T, observedAt time.Time, reason string) Observation[T] {
	return Observation[T]{State: ObservationStale, Value: value, ObservedAt: observedAt, Reason: reason}
}

func Unavailable[T any](reason string) Observation[T] {
	return Observation[T]{State: ObservationUnavailable, Reason: reason}
}

func (o Observation[T]) Usable() bool { return o.State == ObservationFresh }

// Resources is a component-wise resource vector. Slots makes the physical VM
// limit explicit instead of inferring it from CPU or memory.
type Resources struct {
	CPU      int
	MemoryMB int
	Slots    int
}

func (r Resources) Add(other Resources) Resources {
	return Resources{CPU: r.CPU + other.CPU, MemoryMB: r.MemoryMB + other.MemoryMB, Slots: r.Slots + other.Slots}
}

func (r Resources) CanFit(required Resources) bool {
	return r.CPU >= required.CPU && r.MemoryMB >= required.MemoryMB && r.Slots >= required.Slots
}

func (r Resources) Sub(other Resources) (Resources, bool) {
	if !r.CanFit(other) {
		return Resources{}, false
	}
	return Resources{CPU: r.CPU - other.CPU, MemoryMB: r.MemoryMB - other.MemoryMB, Slots: r.Slots - other.Slots}, true
}

type Platform string

const (
	PlatformLinux Platform = "linux"
	PlatformMacOS Platform = "macos"
)

type Route string
type ProfileID string

// SchedulingClass separates fleet-repair work from ordinary application work
// without exposing an unbounded numeric priority. Aging remains the absolute
// starvation guard in the scheduler.
type SchedulingClass string

const (
	SchedulingStandard     SchedulingClass = "standard"
	SchedulingControlPlane SchedulingClass = "control-plane"
)

type Profile struct {
	ID        ProfileID
	Platform  Platform
	Route     Route
	Resources Resources
	MaxActive int
}

type Event string

const (
	EventPullRequest Event = "pull_request"
	EventPush        Event = "push"
	EventSchedule    Event = "schedule"
)

type RunStatus string

const (
	RunQueued     RunStatus = "queued"
	RunInProgress RunStatus = "in_progress"
)

type Demand struct {
	Key       DemandKey
	CreatedAt time.Time
	NotBefore time.Time
	Profile   ProfileID
	Route     Route
	Platform  Platform
	Event     Event
	RunStatus RunStatus
}

type InstanceState string

const (
	InstancePlanned       InstanceState = "planned"
	InstanceCloning       InstanceState = "cloning"
	InstanceBooting       InstanceState = "booting"
	InstanceReachable     InstanceState = "reachable"
	InstanceRegistering   InstanceState = "registering"
	InstanceOnlineIdle    InstanceState = "online_idle"
	InstanceAssigned      InstanceState = "assigned"
	InstanceRunning       InstanceState = "running"
	InstanceDraining      InstanceState = "draining"
	InstanceDeregistering InstanceState = "deregistering"
	InstanceStopping      InstanceState = "stopping"
	InstanceDeleted       InstanceState = "deleted"
	InstanceFailed        InstanceState = "failed"
)

func (s InstanceState) CanTransitionTo(next InstanceState) bool {
	if s != InstanceDeleted && s != InstanceFailed && next == InstanceFailed {
		return true
	}
	switch s {
	case InstancePlanned:
		return next == InstanceCloning || next == InstanceDraining
	case InstanceCloning:
		return next == InstanceBooting || next == InstanceDraining
	case InstanceBooting:
		return next == InstanceReachable || next == InstanceDraining
	case InstanceReachable:
		return next == InstanceRegistering || next == InstanceDraining
	case InstanceRegistering:
		return next == InstanceOnlineIdle || next == InstanceAssigned
	case InstanceOnlineIdle:
		return next == InstanceAssigned || next == InstanceDraining
	case InstanceAssigned:
		return next == InstanceRunning || next == InstanceOnlineIdle || next == InstanceDraining
	case InstanceRunning:
		return next == InstanceDraining
	case InstanceDraining:
		// Draining may roll back to Running: a recovery drain aborts when
		// execution-time evidence disproves its planning-time premise (the VM
		// is powered on, or its runner is still registered).
		return next == InstanceDeregistering || next == InstanceRunning
	case InstanceDeregistering:
		return next == InstanceStopping
	case InstanceStopping:
		return next == InstanceDeleted
	case InstanceFailed:
		return next == InstancePlanned
	default:
		return false
	}
}

type Instance struct {
	ID            string
	Repo          string
	Platform      Platform
	Profile       ProfileID
	Route         Route
	Resources     Resources
	State         InstanceState
	Power         InstancePower
	RecoveryReady bool
	Attempts      int
	RetryAt       time.Time
}

type InstancePower string

const (
	InstancePowerUnknown InstancePower = ""
	InstancePowerRunning InstancePower = "running"
	InstancePowerStopped InstancePower = "stopped"
)

func (i Instance) Live() bool { return i.State != InstanceDeleted }

// ConsumesHostResources distinguishes durable cleanup state from physical
// CPU/RAM occupancy. A VM observed stopped while already tearing down remains
// live until GitHub deregistration and Tart deletion complete, but cannot
// execute again and must not reserve the host's compute envelope forever.
// Unknown power and all non-teardown states remain fail-closed.
func (i Instance) ConsumesHostResources() bool {
	if !i.Live() {
		return false
	}
	if i.Power != InstancePowerStopped {
		return true
	}
	switch i.State {
	case InstanceDraining, InstanceDeregistering, InstanceStopping:
		return false
	default:
		return true
	}
}

func (i Instance) CanRetry(now time.Time, maxAttempts int) bool {
	return i.State == InstanceFailed && maxAttempts > 0 && i.Attempts < maxAttempts && !now.Before(i.RetryAt)
}

type HostMode string

const (
	HostIdle  HostMode = "idle"
	HostLinux HostMode = "linux"
	HostMacOS HostMode = "macos"
	HostMixed HostMode = "mixed"
)

func DeriveHostMode(instances []Instance) (HostMode, error) {
	linux, macos := false, false
	for _, instance := range instances {
		if !instance.ConsumesHostResources() {
			continue
		}
		switch instance.Platform {
		case PlatformLinux:
			linux = true
		case PlatformMacOS:
			macos = true
		default:
			return "", errors.New("unknown live instance platform")
		}
	}
	if linux && macos {
		return HostMixed, nil
	}
	if linux {
		return HostLinux, nil
	}
	if macos {
		return HostMacOS, nil
	}
	return HostIdle, nil
}

type Host struct {
	Available Resources
	Pressure  HostPressure
}

// HostPressure is the bounded, credential-free host snapshot used to explain
// admission decisions. It deliberately carries no process, repository, VM, or
// job labels, so it is safe to expose through the operator API and metrics.
type HostPressure struct {
	AvailableMemoryMB int64
	FreeDiskGB        int64
	SwapUsedMB        int64
	SwapOuts          int64
	CPUIdlePercent    float64
	LoadAverage       float64
	AdmissionAllowed  bool
	AdmissionReason   string
}

type Reservation struct {
	Demand    DemandKey
	Profile   ProfileID
	Resources Resources
	Since     time.Time
}
