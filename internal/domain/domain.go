// Package domain contains the side-effect-free vocabulary shared by the fleet
// scheduler and its adapters.
package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
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
	Demand        DemandKey
	Platform      Platform
	Profile       ProfileID
	Route         Route
	Resources     Resources
	State         InstanceState
	Power         InstancePower
	RecoveryReady bool
	// AssignedSince is when the instance most recently entered the Assigned
	// state. It bounds the assignment deadline: an instance still Assigned long
	// after this never had a job start (only a JobStarted event advances
	// Assigned -> Running). Zero means unknown, which the scheduler treats
	// fail-closed as "never past the deadline".
	AssignedSince time.Time
	// RunningSince is when the instance most recently entered the Running state.
	// Paired with JobInactive it bounds the idle-runner deadline: a Running
	// instance whose bound demand carries no active job for this long is a
	// lingerer holding capacity behind a job that already ended (or was
	// cancelled). Zero means unknown, treated fail-closed as "never past the
	// deadline".
	RunningSince time.Time
	// JobInactive reports that the durable demand bound to a Running instance
	// shows no active job — its status is not JobStarted (terminal/completed,
	// pre-start, or the run was cancelled before work began). A healthy Running
	// instance executing a job has JobInactive false, so it is never a lingerer
	// candidate regardless of age. Fail-closed default false when the demand
	// evidence cannot be read.
	JobInactive bool
	Attempts    int
	RetryAt     time.Time
}

type InstancePower string

const (
	InstancePowerUnknown InstancePower = ""
	InstancePowerRunning InstancePower = "running"
	InstancePowerStopped InstancePower = "stopped"
	// InstancePowerAbsent means a SUCCESSFUL Tart enumeration did not list the
	// owned VM at all: absence proven, never inferred from a failed probe, which
	// stays an unavailable observation. It is deliberately distinct from Stopped
	// (the VM exists and is powered off) and from Unknown (nothing was read), so
	// no caller can confuse "gone" with "unread".
	InstancePowerAbsent InstancePower = "absent"
)

// ProvenIdle reports that a successful host observation established the VM is
// executing nothing: it was enumerated powered off, or it was not enumerated at
// all. Unknown is excluded by construction, because an unread power state is
// never proof of anything. Callers use it for non-destructive judgements about
// what the host is doing; it is deliberately NOT the predicate that authorizes a
// destructive recovery, which keys on an explicitly stopped VM so an enumeration
// miss can never plan a kill (see internal/scheduler.assignmentRecoveries).
func (p InstancePower) ProvenIdle() bool {
	return p == InstancePowerStopped || p == InstancePowerAbsent
}

func (i Instance) Live() bool { return i.State != InstanceDeleted }

// IncarnatesDemand reports whether the instance is a live, non-terminal
// incarnation of a demand: one that still owns its demand key and will
// consume its GitHub job. Terminal incarnations (deleted, failed) have
// released the demand for legitimate (re)spawning, so they never block it;
// every other state — from planned through the teardown chain — still holds
// the demand's content-addressed identity, so planning a fresh spawn for the
// same demand would collide with this instance. Callers guard on a non-zero
// Demand key, since not every observed instance carries scheduling metadata.
func (i Instance) IncarnatesDemand() bool {
	return i.Demand != (DemandKey{}) && i.State != InstanceDeleted && i.State != InstanceFailed
}

// TearingDown reports whether the instance state is one of the cleanup states:
// it can never execute work again, and a durable drain operation is the actor
// that finishes it. Every judgement that treats cleanup differently from live
// work shares this one list so they cannot drift apart.
func (s InstanceState) TearingDown() bool {
	switch s {
	case InstanceDraining, InstanceDeregistering, InstanceStopping:
		return true
	default:
		return false
	}
}

// ConsumesHostResources distinguishes durable cleanup state from physical
// CPU/RAM occupancy. A VM proven idle while already tearing down remains live
// until GitHub deregistration and Tart deletion complete, but cannot execute
// again and must not reserve the host's compute envelope forever. A VM proven
// absent occupies strictly less than one proven stopped, so it must free the
// vector wherever stopped does — otherwise a deleted VM would pin the host to
// its platform harder than a live one. Unknown power and all non-cleanup states
// remain fail-closed.
func (i Instance) ConsumesHostResources() bool {
	if !i.Live() {
		return false
	}
	if !i.Power.ProvenIdle() {
		return true
	}
	return !i.State.TearingDown()
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
	// Available is measured residual headroom: what the host has spare right
	// now, already net of everything running on it, VMs included. It is a clamp
	// and live instances are never subtracted from it a second time.
	Available Resources
	// Capacity is the physical host total that live instances ARE subtracted
	// from. It bounds what the fleet may hand out in aggregate, independently of
	// any configured envelope, so a burst of freshly booted VMs cannot exceed the
	// real machine before their load registers in Available.
	//
	// A zero dimension means "not observed" and imposes no bound. Never
	// synthesize it from configuration: an unobserved physical total must not
	// masquerade as a measurement.
	Capacity Resources
	Pressure HostPressure
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

// instanceName is the fleet's one instance-identity grammar. It was Tart's VM
// name rule and is now the whole fleet's, because the same string is the guest's
// name, the GitHub runner's name, and the durable row's primary key, and those
// three must agree on every backend. The rule is already legal as an OCI
// container name, so a container node inherits it unchanged.
var instanceName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ValidateInstanceName reports whether a string may name an instance. It is
// deliberately in the dependency-free domain package: every layer from the
// executor port down to the durable store validates names, and none of them may
// have to import a backend adapter to do it.
func ValidateInstanceName(name string) error {
	if !instanceName.MatchString(name) || name == "." || name == ".." {
		return errors.New("invalid instance name")
	}
	return nil
}

// ValidateImageReference is the neutral rule for what an instance may be made
// from. It is deliberately weaker than ValidateInstanceName, because the two
// backends disagree about what an image is: on node A it is a Tart base VM name,
// which is an instance name, and on node B it is an OCI reference whose slashes,
// colons, and digest `@` are exactly the characters an instance name forbids.
//
// So this checks only what is true of an image on every backend and knowable
// without a registry: it exists, it is not padded, it carries no whitespace or
// NUL, and it cannot be read as a command-line option. A backend narrows it
// further -- internal/adapters/tart requires a full instance name -- and only a
// registry or a hypervisor can say whether the image is really there.
func ValidateImageReference(image string) error {
	if image == "" || strings.TrimSpace(image) != image {
		return errors.New("image reference is empty or padded")
	}
	if strings.HasPrefix(image, "-") {
		return errors.New("image reference cannot be read as a command-line option")
	}
	if strings.ContainsAny(image, " \t\n\r\x00") {
		return errors.New("image reference contains whitespace or NUL")
	}
	return nil
}
