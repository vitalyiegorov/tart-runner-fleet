package app

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/executor"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

type LiveInstanceStore interface {
	LiveInstances(context.Context) ([]operations.Instance, error)
}

// ExecutorInventory is the enumeration half of executor.Backend: what the node's
// execution technology can see, which is the only evidence that an owned
// instance vanished or that an untracked one exists.
type ExecutorInventory interface {
	List(context.Context) ([]executor.Instance, error)
}

// GuestProbe answers whether one running instance's guest is still executing
// anything at all. It is the only host-side signal that distinguishes a VM the
// backend enumerates as `running` from a VM whose kernel has stopped scheduling
// userspace, which is the condition issue #236's eight dead runners were in for
// minutes before GitHub noticed (ADR 0040).
//
// It returns a three-valued observation rather than an error, because the caller
// has no use for a reason: only a refused transport counts against a guest, and
// everything else — an answered probe, a probe that ran out of its deadline
// against a saturated guest, a probe that could not run — is either alive or
// unknown. Collapsing those into an error and treating any error as death is
// exactly how this mechanism would kill healthy jobs.
type GuestProbe interface {
	Probe(context.Context, string) domain.GuestLiveness
}

// GuestLivenessTracker carries the per-instance probe accumulator between ticks
// and applies the node's bound to it. It is the one impure part of this
// mechanism: the probing is I/O, the accumulation is state, and everything that
// judges either lives in domain and scheduler.
//
// Its memory is deliberately in-process rather than durable. A daemon restart
// forgets every run of refusals and starts again, which is fail-open — the same
// dead guest is re-declared within one window, and a restart can never inherit a
// verdict it did not observe.
type GuestLivenessTracker struct {
	Probe  GuestProbe
	Policy domain.GuestLivenessPolicy
	// Now must be the SAME clock the scheduler plans on. The instants this
	// accumulator stamps are compared against the tick's instant by
	// domain.GuestLivenessPolicy.Confirmed, and a run of refusals recorded on one clock
	// and judged on another is not measurable at all — it fails closed and the
	// mechanism silently never fires. Nil is the wall clock, which is what every
	// production node runs on.
	Now   func() time.Time
	mu    sync.Mutex
	state map[string]domain.GuestLivenessState
}

func (t *GuestLivenessTracker) now() time.Time {
	if t.Now == nil {
		return time.Now().UTC()
	}
	return t.Now().UTC()
}

// Observe probes every named instance and returns the accumulated state for each.
// Instances absent from ids are forgotten, so the accumulator cannot outlive the
// instances it describes.
//
// The probes run concurrently because they are independent waits on independent
// guests, and a node's instance count is bounded by its own configuration. Serial
// probing would spend one deadline per instance inside a single tick, which on a
// saturated host is how a liveness check becomes the thing that stops the control
// loop.
func (t *GuestLivenessTracker) Observe(ctx context.Context, ids []string) map[string]domain.GuestLivenessState {
	if t == nil || t.Probe == nil || !t.Policy.Enabled() || len(ids) == 0 {
		return nil
	}
	now := t.now()
	outcomes := make([]domain.GuestLiveness, len(ids))
	var group sync.WaitGroup
	for index, id := range ids {
		group.Add(1)
		go func() {
			defer group.Done()
			outcomes[index] = t.Probe.Probe(ctx, id)
		}()
	}
	group.Wait()
	t.mu.Lock()
	defer t.mu.Unlock()
	next := make(map[string]domain.GuestLivenessState, len(ids))
	for index, id := range ids {
		next[id] = t.Policy.Observe(t.state[id], outcomes[index], now)
	}
	t.state = next
	return next
}

// PowerCorroborator carries the per-instance run of backend power readings
// between ticks, so the scheduler can tell a VM the backend has said is off three
// times running from a VM it said was off once.
//
// It is the power half of what GuestLivenessTracker does for probes, and it is
// deliberately a separate object rather than a field on it: guest liveness is a
// configured, disable-able mechanism, and this bound must hold on every node
// whether or not that one is wired. Like the tracker its memory is in-process, so
// a daemon restart forgets every run and starts again — fail-open in the safe
// direction, because forgetting can only delay a reclaim, never authorize one.
type PowerCorroborator struct {
	// Now must be the SAME clock the scheduler plans on, for the reason ADR 0040
	// states about the accumulator this one reuses: a run stamped on one clock and
	// judged against another is not measurable at all, so the bound fails closed
	// forever and the mechanism silently never fires. Nil is the wall clock, which
	// is what every production node runs on.
	//
	// It is a field rather than a parameter because the instant must be the
	// corroborator's own on both halves of the judgement, and because the
	// inventory's instant is the wall clock even where the fleet's is not — which
	// is exactly how this bound was unreachable in simulation until issue #247
	// measured it.
	Now  func() time.Time
	mu   sync.Mutex
	runs map[string]domain.ObservationRun
}

func (c *PowerCorroborator) now() time.Time {
	if c.Now == nil {
		return time.Now().UTC()
	}
	return c.Now().UTC()
}

// Observe folds each instance's RAW power reading into its run, attaches the run,
// and replaces the reading with the one the fleet may act on. Instances absent
// from the slice are forgotten, so the accumulator cannot outlive what it
// describes.
//
// Every consumer of Power downstream of here sees the corroborated value, which
// is the point: a stopped reading does not only plan a kill, it also stops
// charging the host for the instance, and an uncorroborated one must do neither.
//
// The instant is read ONCE for the whole slice rather than per instance, for the
// reason ADR 0040 records: a run stamped on one clock and judged on another is
// not measurable at all, and the verdict then fails closed forever without
// saying so.
func (c *PowerCorroborator) Observe(instances []domain.Instance) []domain.Instance {
	if c == nil {
		return instances
	}
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	next := make(map[string]domain.ObservationRun, len(instances))
	for index, instance := range instances {
		run := domain.PowerCorroboration.Observe(c.runs[instance.ID], domain.PowerSignal(instance.Power), now)
		next[instance.ID] = run
		instances[index].PowerRun = run
		instances[index].Power = domain.CorroboratedPower(instance.Power, instance.State, run, instance.PowerRetracted, now)
	}
	c.runs = next
	return instances
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
	Executor                   ExecutorInventory
	Host                       executor.HostProbe
	Recovery                   RecoveryObserver
	RecoveryConfirmationMaxAge time.Duration
	Capacity                   domain.Resources
	Guards                     executor.Guardrails
	// ElasticHostEnvelope reports the physical machine and measured idle CPU so
	// the scheduler can size the fleet against the host it shares rather than a
	// static constant. Default false preserves the configured-envelope model.
	ElasticHostEnvelope bool
	// HostBudget is the operator's declared ceiling on this node's total
	// admission envelope. The zero vector is unset and imposes no bound.
	HostBudget domain.Resources
	// Guest probes the guests of running instances and accumulates their answers,
	// so the scheduler can tell a VM that is running from a VM whose kernel has
	// stopped (ADR 0040). A nil tracker probes nothing and reports nothing, which
	// is what a node with the mechanism disabled wants.
	Guest *GuestLivenessTracker
	// Power corroborates the backend's claim that a live instance's VM is off
	// before that claim may destroy the instance (issue #246). A nil corroborator
	// accumulates nothing, so no instance is ever corroborated stopped — which is
	// fail-closed: it can only withhold a reclaim, never authorize one.
	Power *PowerCorroborator
}

func (p ProductionInventory) Observe(ctx context.Context) (domain.Observation[[]domain.Instance], domain.Observation[domain.Host]) {
	if p.Store == nil || p.Executor == nil || p.Host == nil {
		return domain.Unavailable[[]domain.Instance]("inventory adapters are required"), domain.Unavailable[domain.Host]("inventory adapters are required")
	}
	now := time.Now().UTC()
	hostSnapshot := p.Host.Snapshot(ctx)
	host := hostObservation(hostSnapshot, p.Capacity, p.Guards, p.ElasticHostEnvelope, p.HostBudget)
	stored, err := p.Store.LiveInstances(ctx)
	if err != nil {
		return domain.Unavailable[[]domain.Instance]("durable instance inventory unavailable"), host
	}
	// The unavailable reason and the missing-VM reason below still say "Tart".
	// They are operator-visible strings carried into status, doctor, and the
	// durable observation, so renaming them is a behaviour change and belongs to
	// the change that gives this node a second backend, not to the port
	// extraction.
	vms, err := p.Executor.List(ctx)
	if err != nil {
		return domain.Unavailable[[]domain.Instance]("Tart inventory unavailable"), host
	}
	byName := make(map[string]executor.Instance, len(vms))
	for _, vm := range vms {
		byName[vm.Name] = vm
	}
	result := make([]domain.Instance, 0, len(stored))
	for _, instance := range stored {
		if !instance.SchedulingMetadataValid() || instance.Repo == "" {
			return domain.Unavailable[[]domain.Instance]("invalid durable scheduling metadata"), host
		}
		vm, exists := byName[instance.ID]
		if !exists && !absenceIsReconcilable(instance.State) {
			return domain.Unavailable[[]domain.Instance](fmt.Sprintf("owned VM %s missing from Tart", instance.ID)), host
		}
		delete(byName, instance.ID)
		power := domain.InstancePowerUnknown
		switch {
		case exists && vm.Running:
			power = domain.InstancePowerRunning
		case exists:
			power = domain.InstancePowerStopped
		case instance.State.TearingDown():
			// The Tart read above SUCCEEDED, so this VM is proven gone rather than
			// unread, and during cleanup that is an expected intermediate fact — see
			// absenceIsReconcilable. A Planned instance keeps Unknown power: its VM has
			// not been cloned yet, so nothing was ever observed about it.
			power = domain.InstancePowerAbsent
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
		// The occupancy clock is the durable row's creation instant, not its last
		// state change: the host has been short these cores since the instance was
		// planned, and a runner moving from Assigned to Running gives none of them
		// back. This is the one per-instance age that must survive a state change,
		// which is exactly why it reads CreatedAt where the two deadlines above
		// read UpdatedAt (ADR 0036).
		result = append(result, domain.Instance{ID: instance.ID, Repo: instance.Repo, Demand: instance.Demand, Platform: instance.Platform, Profile: instance.Profile,
			Route: instance.Route, Resources: instance.Resources, State: instance.State, Power: power, RecoveryReady: recoveryReady,
			// A live row still carrying the stopped-recovery phase is a drain that did
			// not complete, and the only way a stopped recovery does not complete is
			// DrainExecutor.abort: the VM answered `running` at the moment of acting.
			// The durable phase is therefore the fleet's own record of having disproven
			// this instance's power reading, and it costs no query to read (issue #246).
			PowerRetracted: instance.DrainPhase == operations.DrainPhaseStoppedRecovery,
			AssignedSince:  assignedSince, RunningSince: runningSince, JobInactive: jobInactive, OccupiedSince: instance.CreatedAt})
	}
	for name := range byName {
		if strings.HasPrefix(name, "trf-") {
			return domain.Unavailable[[]domain.Instance]("untracked controller VM requires reconciliation"), host
		}
	}
	return domain.Fresh(p.probeGuests(ctx, p.Power.Observe(result)), now), host
}

// probeGuests asks every powered-on Running instance's guest whether it is still
// executing anything, and folds the answer into that instance's accumulator.
//
// It is restricted to Running and powered-on for two reasons. A guest that has
// not reached Running has nothing worth probing for this purpose — the boot
// readiness probe and the assignment deadline already own that ground — and a VM
// the backend reports stopped or absent is already reclaimable through a gate
// that needs no probe at all. The probe therefore never runs against an instance
// the fleet was about to recover anyway.
func (p ProductionInventory) probeGuests(ctx context.Context, instances []domain.Instance) []domain.Instance {
	candidates := make([]string, 0, len(instances))
	for _, instance := range instances {
		if instance.State == domain.InstanceRunning && instance.Power == domain.InstancePowerRunning {
			candidates = append(candidates, instance.ID)
		}
	}
	states := p.Guest.Observe(ctx, candidates)
	if len(states) == 0 {
		return instances
	}
	for index, instance := range instances {
		if state, probed := states[instance.ID]; probed {
			instances[index].Guest = state
		}
	}
	return instances
}

// absenceIsReconcilable reports whether a live durable row whose owned VM a
// successful `tart list` did not enumerate can be carried as a per-instance fact
// instead of collapsing the whole host's instance observation.
//
// Planned precedes the clone, so its VM legitimately does not exist yet. The
// cleanup states are the other half, for two reasons that hold together:
//
//   - Absence there is EXPECTED, not anomalous. DrainExecutor deletes the VM and
//     only then advances the row to deleted, and a tick reads the durable rows and
//     Tart at two different instants, so any interleaving of the two legitimately
//     observes a live cleanup row with no VM. Blocking host-wide made a benign,
//     self-clearing race stop planning for every profile on the machine.
//   - Something is already reconciling it, and absence cannot stop it: the drain's
//     Stop and Delete both treat an absent VM as success, so the row reaches
//     deleted and the observation heals with no operator action.
//
// Every other live state stays host-wide fail-closed. A row leaves Planned only
// after Clone succeeded, so absence there is an out-of-band removal with no
// cleanup operation reconciling it, and reclaiming it would mean deregistering a
// runner GitHub may still be handing a job to — new destructive authority derived
// from an enumeration miss, which this change deliberately does not take. It
// keeps a loud, correct block that an operator resolves with
// `fleet operations discharge`.
func absenceIsReconcilable(state operations.State) bool {
	return state == operations.StatePlanned || state.TearingDown()
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
//
// A configured host budget is checked here rather than in config validation
// because it is the only place both halves of the claim exist: `fleet config
// validate` decodes a file and never probes a machine, so a budget larger than
// the host it runs on cannot be caught until the host is observed.
func hostObservation(snapshot executor.HostSnapshot, capacity domain.Resources, guards executor.Guardrails, elastic bool,
	budget domain.Resources) domain.Observation[domain.Host] {
	switch snapshot.Freshness {
	case executor.Fresh:
		// Continue with explicit guardrails below.
	case executor.Stale:
		return domain.Stale(domain.Host{}, snapshot.ObservedAt, "host probe is stale")
	case executor.Unavailable:
		return domain.Unavailable[domain.Host]("host probe unavailable")
	default:
		return domain.Unavailable[domain.Host]("host probe freshness is invalid")
	}
	if reason := budgetExceedsHost(snapshot, guards, budget); reason != "" {
		return domain.Unavailable[domain.Host](reason)
	}
	decision := guards.Evaluate(snapshot, executor.AdmissionRequest{})
	pressure := domain.HostPressure{AvailableMemoryMB: snapshot.AvailableMemoryMB, FreeDiskGB: snapshot.FreeDiskGB,
		SwapUsedMB: snapshot.SwapUsedMB, SwapOuts: snapshot.SwapOuts,
		SwapOutRatePerSecond: snapshot.SwapOutRatePerSecond, SwapOutRateObserved: snapshot.SwapOutRateObserved,
		CPUIdlePercent: snapshot.CPUidlePercent,
		LoadAverage:    snapshot.LoadAverage, AdmissionAllowed: decision.Allowed, AdmissionReason: decision.Reason}
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

// budgetExceedsHost reports why a configured ceiling is not one this machine can
// honour, or the empty string when it is. A budget above the host is not a
// narrower envelope -- the physical bound would simply keep binding first -- it
// is an operator who believes this node offers capacity it does not have, and
// on a node whose whole purpose is a promise to a co-tenant that belief is worth
// failing closed over. The reason names both figures so the fix is the message.
//
// A dimension the probe could not read imposes no bound, exactly as it does for
// the physical total in ADR 0018: an unobserved fact must never masquerade as a
// measurement of a zero-resource machine.
func budgetExceedsHost(snapshot executor.HostSnapshot, guards executor.Guardrails, budget domain.Resources) string {
	if budget == (domain.Resources{}) {
		return ""
	}
	if snapshot.PhysicalCPU > 0 && int64(budget.CPU) > snapshot.PhysicalCPU {
		return fmt.Sprintf("host budget of %d CPU exceeds the %d physical cores this host has",
			budget.CPU, snapshot.PhysicalCPU)
	}
	usable := snapshot.PhysicalMemoryMB - guards.MinAvailableMemoryMB
	if snapshot.PhysicalMemoryMB > 0 && int64(budget.MemoryMB) > usable {
		return fmt.Sprintf("host budget of %d MB memory exceeds the %d MB this host can offer (%d MB physical less the %d MB reserve)",
			budget.MemoryMB, usable, snapshot.PhysicalMemoryMB, guards.MinAvailableMemoryMB)
	}
	return ""
}

// physicalCapacity reports the machine's real totals as an admission bound.
// Dimensions the probe could not read stay zero, which the scheduler reads as
// not-observed and ignores, so an unreadable fact degrades to the configured
// envelope instead of closing admission. Slots have no physical analogue and
// stay configured.
func physicalCapacity(snapshot executor.HostSnapshot, capacity domain.Resources, guards executor.Guardrails) domain.Resources {
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
func idleCores(snapshot executor.HostSnapshot) int {
	idle := snapshot.CPUidlePercent
	if idle <= 0 {
		return 0
	}
	if idle > 100 {
		idle = 100
	}
	return int(float64(snapshot.PhysicalCPU) * idle / 100)
}
