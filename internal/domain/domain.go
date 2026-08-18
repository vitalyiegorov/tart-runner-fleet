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
	// OccupancyBudget is the wall-clock ceiling on how long ONE instance of this
	// profile may hold its resource vector, measured from the moment the instance
	// began occupying it. It is a property of the profile because it is a
	// statement about the work that profile runs: a macOS builder legitimately
	// spends forty minutes on an App Store archive, and a one-core Linux job that
	// has been running for an hour is not doing the work it was spawned for.
	//
	// Zero means no ceiling, and that is the fail-open default on purpose: a
	// destructive bound is never inferred from a configuration that does not
	// state one.
	OccupancyBudget time.Duration
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
	// Priority is the tier this demand was classified into when it entered the
	// queue (issue #224). The zero value is the default tier, so a fleet that
	// declares no tiers plans exactly the aged FIFO it always did.
	Priority Priority
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
	// OccupiedSince is when the instance began holding its resource vector: the
	// instant its durable row was created, because a planned instance already
	// charges the host envelope (see ConsumesHostResources) whether or not its VM
	// has booted yet. It is deliberately NOT reset by a state change — the host
	// does not get its cores back when a runner moves from Assigned to Running —
	// which is what distinguishes it from AssignedSince and RunningSince. Zero
	// means unknown, treated fail-closed as "no measurable occupancy".
	OccupiedSince time.Time
	// Guest is what the node's guest-liveness probe has accumulated about this
	// instance: how many consecutive probes the guest refused outright, since
	// when, and when it last answered one. It is the only evidence the fleet has
	// that a VM Tart still enumerates as `running` is in fact a wedged or
	// panicked kernel executing nothing (ADR 0040). The zero value is "never
	// probed", which is fail-closed: it can never declare a guest dead.
	Guest GuestLivenessState
	// PowerRun is what the node's backend enumeration has accumulated about this
	// instance's VM being off: how many consecutive readings said `stopped`, since
	// when, and when one last said `running`. Power alone is a single reading, and
	// a backend that reports its own errors as "not running" makes a single reading
	// insufficient to destroy a runner for (issue #246); PowerStopped is the
	// predicate that judges it. The zero value is "never corroborated", which is
	// fail-closed.
	PowerRun ObservationRun
	// PowerRetracted reports that a stopped recovery of this instance has already
	// been sent back to Running by the drain's own re-read of the power at the
	// moment of acting. It is the fleet's record of having disproven its own
	// premise about this machine, and it raises the bound the premise must meet
	// before it may act again (PowerRetractedFactor). Without it a persistently
	// wrong reading re-derives the identical operation forever, which is exactly
	// what issue #246 measured: eighty-six drains, eighty-six aborts, nine minutes.
	PowerRetracted bool
	Attempts       int
	RetryAt        time.Time
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
// CPU/RAM occupancy. A tearing-down instance whose guest has stopped remains
// live until GitHub deregistration and Tart deletion complete, but cannot
// execute again and must not reserve the host's compute envelope forever.
// Unknown power and all non-cleanup states remain fail-closed.
//
// The vector comes back on evidence the next observation cannot retract, and
// never on a reading alone. That is the whole of issue #247. A `stopped`
// enumeration of a draining instance is a CLAIM about a machine the fleet has
// not yet acted on, and the fleet has two ways to find out it was wrong: the
// drain re-reads the same source at the moment of acting and aborts back to
// Running (ADR 0033), or the next enumeration simply tells the truth. Either way
// the instance goes on holding the host — and the scheduler has already lent
// that capacity to a replacement, because a released vector cannot be taken
// back. The simulator produced exactly that on both paths: five slots against a
// four-slot ceiling, and eight CPU against a four-CPU budget.
//
// Two facts are not retractable, and they are the two this reads:
//
//   - the VM is gone. A successful enumeration that does not list an owned VM is
//     proven absence, and nothing brings it back. It occupies strictly less than
//     a stopped one, so it must free the vector wherever a stop does — otherwise
//     a deleted VM would pin the host to its platform harder than a live one.
//   - the fleet's own stop has landed. `stopping` is entered only after the drain
//     powered the guest off and its deregistration was confirmed, so from there
//     the instance can never return to Running and no reading can put work back
//     on the machine.
//
// The cost is one lifecycle edge of extra hold on a genuinely idle teardown,
// which ADR 0043 measures. What it buys is that the host is never over-admitted
// on a premise the fleet has not itself established.
func (i Instance) ConsumesHostResources() bool {
	if !i.Live() {
		return false
	}
	if !i.State.TearingDown() || !i.Power.ProvenIdle() {
		return true
	}
	return i.Power != InstancePowerAbsent && i.State != InstanceStopping
}

// Occupancy reports how long the instance has been holding its resource vector,
// and whether that is a measurable fact at all. It is false — never a zero
// duration passed off as a measurement — when the instance is not charging the
// host, when the start of the occupancy was never recorded, or when the
// recorded start is ahead of the given instant, which is a clock the caller may
// not reason about. Every judgement about an over-long hold reads this one
// answer so none of them can disagree about when occupancy began.
func (i Instance) Occupancy(now time.Time) (time.Duration, bool) {
	if !i.ConsumesHostResources() || i.OccupiedSince.IsZero() || now.Before(i.OccupiedSince) {
		return 0, false
	}
	return now.Sub(i.OccupiedSince), true
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
	// SwapOutRatePerSecond and SwapOutRateObserved are the signal the swap
	// guardrail decides on. ADR 0018 demoted SwapUsedMB from a gate to a
	// necessary-but-insufficient condition, because macOS does not eagerly
	// reclaim swap and the level is closer to a high-water mark than to current
	// pressure; admission is refused only when the host is also paging out.
	//
	// Publishing the level without the rate is what made a correct decision
	// unreadable on node C: swap 6.6x the ceiling beside "admission allowed",
	// with the fact that reconciles them nowhere in the API. Observed=false is a
	// rate that could not be measured, never a quiet host -- the guard fails
	// closed on the level there -- so the two must travel together.
	SwapOutRatePerSecond float64
	SwapOutRateObserved  bool
	CPUIdlePercent       float64
	LoadAverage          float64
	AdmissionAllowed     bool
	AdmissionReason      string
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
