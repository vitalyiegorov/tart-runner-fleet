// Package scheduler implements the deterministic, side-effect-free fleet
// planner. Adapters provide immutable observations and execute its operations.
package scheduler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

type Config struct {
	LinuxCapacity         domain.Resources
	FairnessAge           time.Duration
	AssignedTimeout       time.Duration
	RepoCaps              map[string]int
	RepoSchedulingClasses map[string]domain.SchedulingClass
	Profiles              map[domain.ProfileID]domain.Profile
	MacOSExclusive        bool
	// MixedPlatformAdmission relaxes the per-tick platform-exclusive choice so
	// Linux runners fill the residual host envelope beside a live macOS cohort
	// (and a compatible macOS profile fills it beside live Linux), within the
	// same capacity, MaxActive, single-cohort, and drain-safety invariants.
	// Default false preserves the platform-exclusive behavior byte-for-byte.
	MixedPlatformAdmission bool
	// MixedProfileCohorts lets two macOS profiles run side by side when their
	// exact vectors fit, under the same law as everything else: the physical
	// bound, profile MaxActive, repository caps, and drain safety. It completes
	// what ADR 0012 did for platforms -- profile identity, like platform
	// identity, is not itself a resource. Default false preserves the
	// single-cohort behavior byte-for-byte: one macOS profile at a time, with
	// drain-and-switch between them.
	MixedProfileCohorts bool
	// ElasticHostEnvelope makes the fleet a second pilot on its own host: the
	// fleet-wide bound becomes the observed physical host (Host.Capacity minus
	// live instances, clamped by the measured Host.Available residual) instead of
	// a static configured constant, and LinuxCapacity reverts to what its name
	// says -- a Linux-only cap that only Linux instances consume.
	//
	// This lets the fleet expand into an idle host and yield as the host's own
	// tenant gets busy, which a static envelope cannot do in either direction.
	// Default false keeps LinuxCapacity as the shared cross-platform envelope
	// and preserves ADR 0012 behavior byte-for-byte.
	ElasticHostEnvelope bool
	// HostBudget is an explicit operator ceiling on the TOTAL admission envelope
	// of this node, every platform charged against it together. It exists for a
	// machine the fleet shares with work it does not own and cannot measure: the
	// pressure guardrails narrow admission as a co-tenant gets busy, but nothing
	// stops the fleet taking the whole machine while the co-tenant is quiet, and
	// a co-tenant that is quiet now is not a co-tenant that has gone away.
	//
	// It composes with every other bound by minimum and can only ever narrow one.
	// The zero vector is unset and imposes no bound, so an omitted setting is
	// today's behavior byte-for-byte.
	HostBudget domain.Resources
	// PriorityEscalation is how long a demand waits before it climbs one
	// priority tier (issue #224). It is the mandatory other half of tiers: a
	// declared tier is a way for one class of work to overtake another, and
	// without escalation a stream of high-tier arrivals would starve the default
	// tier forever. With it, a waiting demand outranks anything of tier rank N
	// once it is N*PriorityEscalation older, so the extra wait a tier can impose
	// is bounded by configuration rather than by luck.
	//
	// Zero disables escalation, which is only ever correct when no tier is
	// declared -- every demand is then rank zero and the order is aged FIFO.
	// `fleet config validate` refuses tiers without a threshold.
	PriorityEscalation time.Duration
}

type State struct {
	// Reservation is deliberately singular: aged global FIFO permits only its
	// head to reserve a vector. Backfill may use only the vector remainder.
	Reservation  *domain.Reservation
	DRRCursor    string
	MacHandoff   *MacHandoff
	LinuxHandoff *LinuxHandoff
}

// MacHandoff durably bounds residual-capacity admission while Linux prevents
// a selected macOS demand from starting. BackfillAdmitted is monotonic for one
// selected demand: repeated ticks and newly arriving Linux work cannot create
// an unbounded backfill stream ahead of macOS.
type MacHandoff struct {
	Demand           domain.DemandKey
	Profile          domain.ProfileID
	Since            time.Time
	BackfillAdmitted bool
}

// LinuxHandoff durably bounds work-conserving admission while a busy macOS
// instance prevents an older selected Linux demand from starting. Exactly one
// compatible same-profile macOS backfill wave may use otherwise stranded host
// capacity; subsequent ticks preserve the Linux handoff instead of admitting
// an unbounded stream.
type LinuxHandoff struct {
	Demand           domain.DemandKey
	Since            time.Time
	BackfillAdmitted bool
}

type Input struct {
	Now       time.Time
	Config    Config
	Demands   domain.Observation[[]domain.Demand]
	Instances domain.Observation[[]domain.Instance]
	Host      domain.Observation[domain.Host]
	Prior     State
}

type OperationKind string

const (
	OperationSpawn OperationKind = "spawn"
	OperationDrain OperationKind = "drain"
)

type Operation struct {
	ID                string
	Kind              OperationKind
	Demand            domain.DemandKey
	Instance          string
	Profile           domain.ProfileID
	Route             domain.Route
	DependsOn         []string
	Recovery          bool `json:"recovery,omitempty"`
	ConfirmedInactive bool `json:"confirmedInactive,omitempty"`
	StalledAssignment bool `json:"stalledAssignment,omitempty"`
	LingeringRunner   bool `json:"lingeringRunner,omitempty"`
	// OccupancyExceeded marks the one reclaim whose premise is not that the
	// runner is idle but that it has held its profile's vector too long. It is
	// part of the content address like every other cause, so a budget reap and a
	// lingering-runner reap of one instance are distinct attempts (ADR 0028).
	OccupancyExceeded bool `json:"occupancyExceeded,omitempty"`
}

type PlanStatus string

const (
	PlanReady              PlanStatus = "ready"
	PlanBlockedObservation PlanStatus = "blocked_observation"
	PlanInvalidObservation PlanStatus = "invalid_observation"
)

type Plan struct {
	ID         string
	Status     PlanStatus
	Operations []Operation
	Next       State
	// Reason carries a bounded, credential-free diagnostic for non-ready plans.
	// It is composed only from observation reasons (adapter-authored safe
	// strings) and static scheduler error text, never from wrapped errors.
	Reason string
	// ReservationAxis names WHY this plan is holding Next.Reservation, at the
	// moment and in the envelope the decision was made. It is empty when the plan
	// holds no reservation, and empty when a reservation is merely carried
	// through a plan that decided nothing (a blocked observation, for instance):
	// a plan that did not judge the head must not publish a judgement.
	//
	// This exists because issue #226 was invisible on a live fleet. Nothing
	// published named the held reservation, its repository, or which of the two
	// axes was holding it, so a defect that strands a vector for the whole
	// runtime of a blocking job left no artifact at all and only a simulator
	// found it. The axis is the diagnostic: a `vector` hold is waiting on live
	// instances to release, and a `repository_cap` hold is waiting on one of its
	// own repository's instances to exit, which no amount of freed CPU can
	// shorten.
	ReservationAxis ReservationAxis
}

// ReservationAxis is the closed vocabulary of reasons a reserved head is not
// admitted. It is closed deliberately: it is published as a metric label, and an
// open vocabulary there is unbounded cardinality.
type ReservationAxis string

const (
	// ReservationAxisVector is ADR 0017's axis: the head's vector does not fit
	// the starvation envelope, so it is waiting on live instances to release.
	ReservationAxisVector ReservationAxis = "vector"
	// ReservationAxisRepositoryCap is ADR 0038's axis: the vector fits and the
	// head's own repository is at its cap, so it is waiting on one of that
	// repository's instances to exit and nothing else.
	ReservationAxisRepositoryCap ReservationAxis = "repository_cap"
	// ReservationAxisBoth is both at once.
	ReservationAxisBoth ReservationAxis = "both"
	// ReservationAxisNone is a reservation held for a head that neither axis
	// refuses. The scheduler does not produce it — a reservation is minted only
	// when `feasible` is false — so it is published for the case an operator
	// most needs named: a fleet reserving a vector for work it could have
	// started, which is issue #125's wedge wearing a reservation.
	ReservationAxisNone ReservationAxis = "none"
)

// reservationAxis names which of `feasible`'s two terms refused the head, judged
// in the envelope and against the occupancy `planLinux` used to decide.
//
// It is a diagnostic and never a decision: nothing in the planner reads it, so a
// wrong answer here can misinform an operator but can never misplan a tick.
func reservationAxis(config Config, resources, agedFree domain.Resources, repo string, occupied map[string]int) ReservationAxis {
	vector := !agedFree.CanFit(resources)
	cap := occupied[repo] >= repoCapLimit(config.RepoCaps, repo)
	switch {
	case vector && cap:
		return ReservationAxisBoth
	case vector:
		return ReservationAxisVector
	case cap:
		return ReservationAxisRepositoryCap
	}
	return ReservationAxisNone
}

// PlanTick computes a complete plan without performing side effects.
func PlanTick(in Input) Plan {
	if !in.Demands.Usable() || !in.Instances.Usable() || !in.Host.Usable() {
		return finish(Plan{Status: PlanBlockedObservation, Next: in.Prior, Reason: blockedReason(in)})
	}
	mode, err := domain.DeriveHostMode(in.Instances.Value)
	if err != nil {
		return finish(Plan{Status: PlanInvalidObservation, Next: in.Prior, Reason: err.Error()})
	}

	demands := normalizedDemands(in)
	plan := Plan{Status: PlanReady, Next: in.Prior}
	if recoveries := assignmentRecoveries(in.Now, in.Config, in.Instances.Value); len(recoveries) > 0 {
		plan.Operations = recoveries
		return finish(plan)
	}
	if len(demands) == 0 {
		plan.Next.Reservation = nil
		plan.Next.MacHandoff = nil
		plan.Next.LinuxHandoff = nil
		return finish(plan)
	}

	ordered := priorityOrder(in, demands)
	linux, macos := partition(ordered)
	if in.Config.MacOSExclusive {
		return finish(planExclusiveAdmission(in, plan, linux, macos))
	}
	switch ordered[0].Platform {
	case domain.PlatformMacOS:
		plan.Next.LinuxHandoff = nil
		if mode == domain.HostLinux || mode == domain.HostMixed {
			plan = planMacWithCoexistence(in, plan, linux, macos)
			plan = fillLinuxRemainder(in, plan, linux)
		} else {
			plan.Next.MacHandoff = nil
			attempted := planMacOS(in, plan, macos)
			switch {
			case containsSpawn(attempted.Operations):
				plan = fillLinuxRemainder(in, attempted, linux)
			case mode == domain.HostIdle:
				// On a fully idle host a macOS head that will not spawn is resource-
				// infeasible: nothing is live to drain and make room for it, so it is
				// NOT waiting on drainable work. The bounded one-shot handoff latch is
				// wrong here — it drains the queue by a single job and re-wedges.
				// Admit feasible work behind it in the residual envelope every tick.
				plan = planBehindInfeasibleMacHead(in, plan, linux, macos)
			case len(linux) == 0:
				plan = attempted
			case in.Config.MixedPlatformAdmission:
				// A live macOS cohort blocks the head, but nothing Linux is live to
				// drain: the head simply does not fit beside the cohort. Fill the
				// residual envelope with Linux every tick instead of the bounded
				// one-shot handoff backfill. The infeasible-head remainder planner
				// admits feasible Linux (and a feasible macOS profile behind the head
				// when no Linux fits) without ever latching or draining.
				plan = planBehindInfeasibleMacHead(in, plan, linux, macos)
			default:
				// A busy macOS instance (a live foreign cohort) blocks the head while
				// Linux waits. Bounded-drain one aged Linux job so the drain is not
				// starved by an unbounded backfill stream.
				plan = planMacHandoff(in, plan, linux, macos)
			}
		}
	case domain.PlatformLinux:
		plan.Next.MacHandoff = retainedMacHandoff(in.Prior.MacHandoff, macos)
		if mode == domain.HostMacOS || mode == domain.HostMixed {
			plan = planLinuxWithCoexistence(in, plan, linux, macos)
		} else {
			plan.Next.LinuxHandoff = nil
			plan = planLinux(in, plan, linux)
		}
		plan = fillMacRemainder(in, plan, macos)
	}
	return finish(plan)
}

// blockedReason names which observations are unusable and their adapter
// authored reasons. Every input is a closed safe-vocabulary string: the
// observation name is static, the state is an enum, and the reason is written
// by adapters from a credential-free set. No wrapped error text can enter here.
func blockedReason(in Input) string {
	parts := make([]string, 0, 3)
	for _, observation := range []struct {
		name   string
		state  domain.ObservationState
		reason string
	}{
		{"demands", in.Demands.State, in.Demands.Reason},
		{"instances", in.Instances.State, in.Instances.Reason},
		{"host", in.Host.State, in.Host.Reason},
	} {
		if observation.state == domain.ObservationFresh {
			continue
		}
		part := observation.name + " " + string(observation.state)
		if observation.reason != "" {
			part += ": " + observation.reason
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "; ")
}

func planExclusiveAdmission(in Input, plan Plan, linuxDemands, macDemands []domain.Demand) Plan {
	activeProfile, activeProfileOK := activeMacProfile(in.Instances.Value)
	hasActiveMac := hasConsumingPlatform(in.Instances.Value, domain.PlatformMacOS)
	if hasActiveMac && !activeProfileOK {
		// Multiple live macOS profiles violate the single-cohort invariant. Do
		// not guess which profile to drain or admit; observation must converge.
		return plan
	}
	if len(macDemands) > 0 {
		plan.Next.LinuxHandoff = nil
		target := chosenMacProfile(in, macDemands)
		if hasActiveMac && len(demandsForProfile(macDemands, activeProfile)) > 0 {
			target = activeProfile
		}
		macDemands = macProfileFirst(in, macDemands, target)
		return planExclusiveMacHandoff(in, plan, macDemands)
	}
	plan.Next.MacHandoff = nil
	if hasActiveMac {
		return planLinuxHandoff(in, plan, linuxDemands, nil)
	}
	plan.Next.LinuxHandoff = nil
	return planLinux(in, plan, linuxDemands)
}

func planExclusiveMacHandoff(in Input, plan Plan, macDemands []domain.Demand) Plan {
	target := macDemands[0].Profile
	if in.Prior.MacHandoff != nil && in.Prior.MacHandoff.Profile == target {
		macDemands = stableMacHandoffOrder(in.Prior.MacHandoff, macDemands)
	}
	handoff := macHandoffFor(in.Prior.MacHandoff, macDemands[0], in.Now)
	var drains []Operation
	allForeignIdle := true
	filtered := append([]domain.Instance(nil), in.Instances.Value...)
	for _, instance := range sortedInstances(in.Instances.Value) {
		if !instance.ConsumesHostResources() || (instance.Platform == domain.PlatformMacOS && instance.Profile == target) {
			continue
		}
		if instance.State != domain.InstanceOnlineIdle {
			allForeignIdle = false
			continue
		}
		drains = append(drains, drainOperation(instance))
		filtered = removeInstance(filtered, instance.ID)
	}
	plan.Operations = append(plan.Operations, drains...)
	if !allForeignIdle {
		plan.Next.MacHandoff = &handoff
		return plan
	}
	plan.Next.MacHandoff = nil
	nextInput := in
	nextInput.Instances = domain.Fresh(filtered, in.Instances.ObservedAt)
	return appendMacSpawns(nextInput, plan, demandsForProfile(macDemands, target), operationIDs(drains))
}

func macProfileFirst(in Input, demands []domain.Demand, profile domain.ProfileID) []domain.Demand {
	selected := priorityOrder(in, demandsForProfile(demands, profile))
	for _, demand := range priorityOrder(in, demands) {
		if demand.Profile != profile {
			selected = append(selected, demand)
		}
	}
	return selected
}

func hasConsumingPlatform(instances []domain.Instance, platform domain.Platform) bool {
	for _, instance := range instances {
		if instance.Platform == platform && instance.ConsumesHostResources() {
			return true
		}
	}
	return false
}

func assignmentRecoveries(now time.Time, config Config, instances []domain.Instance) []Operation {
	var recoveries []Operation
	for _, instance := range sortedInstances(instances) {
		if instance.State != domain.InstanceAssigned && instance.State != domain.InstanceRunning {
			continue
		}
		confirmedInactive := instance.Power == domain.InstancePowerRunning && instance.RecoveryReady
		stalled := stalledAssignment(now, config.AssignedTimeout, instance)
		lingering := lingeringRunner(now, config.AssignedTimeout, instance)
		overBudget := occupancyExceeded(now, config.Profiles, instance)
		// The stopped gate is an exact power comparison, not domain.ProvenIdle: an
		// owned VM merely missing from a Tart enumeration must never plan a kill.
		// Absence cannot even reach here for an assigned or running instance — that
		// observation is still host-wide unavailable (internal/app/inventory.go) — and
		// if it ever could, no destructive operation may be derived from it.
		if instance.Power != domain.InstancePowerStopped && !confirmedInactive && !stalled && !lingering && !overBudget {
			continue
		}
		// The causes are mutually exclusive in the durable phase they select, and
		// the occupancy budget is deliberately last: every other cause is evidence
		// that no work is happening, and each re-verifies that at execution time.
		// A budget breach claims only that the hold is too long, so whenever a
		// safer cause also applies it is the one that should act.
		overBudget = overBudget && !confirmedInactive && !stalled && !lingering &&
			instance.Power != domain.InstancePowerStopped
		operation := Operation{Kind: OperationDrain, Instance: instance.ID, Profile: instance.Profile, Route: instance.Route,
			Recovery: true, ConfirmedInactive: confirmedInactive, StalledAssignment: stalled, LingeringRunner: lingering,
			OccupancyExceeded: overBudget}
		if overBudget {
			// A budget reap ends a job that GitHub still believes is running, so the
			// durable operation must name that job. Every other drain cause implies
			// there is no job left to name (ADR 0036).
			operation.Demand = instance.Demand
		}
		operation.ID = stableID("op", operation)
		recoveries = append(recoveries, operation)
	}
	return recoveries
}

// stalledAssignment reports whether an assigned instance has exceeded the
// assignment deadline with no evidence a job ever started. The instance FSM is
// the evidence: only a JobStarted demand event advances Assigned -> Running, so
// an instance still Assigned past the deadline provably never began a job. It
// deliberately narrows to a powered-on, unconfirmed assignment — a stopped or
// already-confirmed-inactive instance is handled by the pre-existing recovery
// gates — and stays fail-closed when the entry time or deadline is unknown, so
// it can only open the guarded recovery path, never bypass its evidence check.
func stalledAssignment(now time.Time, assignedTimeout time.Duration, instance domain.Instance) bool {
	return instance.State == domain.InstanceAssigned && instance.Power == domain.InstancePowerRunning &&
		!instance.RecoveryReady && assignedTimeout > 0 && !instance.AssignedSince.IsZero() &&
		now.Sub(instance.AssignedSince) >= assignedTimeout
}

// lingeringRunner reports whether a Running instance has exceeded the
// idle-runner deadline while its bound demand shows no active job. Unlike a
// stalled assignment — whose FSM state alone proves no job started — a Running
// instance's state is ambiguous, so JobInactive (demand status is not
// JobStarted: terminal, cancelled-before-start, or completed) is the evidence
// that no work is executing. It narrows to a powered-on, unconfirmed runner —
// a stopped or already-confirmed-inactive instance is handled by the
// pre-existing recovery gates — and stays fail-closed when the entry time or
// deadline is unknown, so it can only open the guarded recovery path (whose
// execution-time JobActive re-check is the real gate), never bypass it. The
// deadline is deliberately generous so a job completing normally drains through
// the ordinary completion path long before this fires. The busy-drain
// invariant is preserved: an instance with an active job has JobInactive false
// and is never a candidate, regardless of age.
func lingeringRunner(now time.Time, idleTimeout time.Duration, instance domain.Instance) bool {
	return instance.State == domain.InstanceRunning && instance.Power == domain.InstancePowerRunning &&
		!instance.RecoveryReady && instance.JobInactive && idleTimeout > 0 && !instance.RunningSince.IsZero() &&
		now.Sub(instance.RunningSince) >= idleTimeout
}

// OccupancyWarnFraction is the share of a profile's occupancy budget at which a
// hold becomes worth saying out loud. It exists so the condition is visible
// while it is happening rather than in the post-mortem that produced ADR 0036:
// three quarters of the budget leaves an operator a quarter of it to look, and
// is late enough that an ordinary long job does not cry wolf on every tick.
const OccupancyWarnFraction = 0.75

// occupancyExceeded reports whether an instance has held its profile's resource
// vector past that profile's wall-clock ceiling. Unlike stalledAssignment and
// lingeringRunner it does NOT claim the runner is idle — the incident it exists
// for was a genuinely executing job — so it is deliberately narrow: it needs a
// configured ceiling, a powered-on VM, and a measurable occupancy, and it stays
// fail-closed on every one of them. An unconfigured ceiling is no ceiling, never
// a zero one.
func occupancyExceeded(now time.Time, profiles map[domain.ProfileID]domain.Profile, instance domain.Instance) bool {
	profile, known := profiles[instance.Profile]
	if !known || profile.OccupancyBudget <= 0 || instance.Power != domain.InstancePowerRunning {
		return false
	}
	age, measured := instance.Occupancy(now)
	return measured && age >= profile.OccupancyBudget
}

// Occupancy is one instance's hold on the resource vector its profile reserves,
// with everything an operator needs to judge it: how long, against what ceiling,
// and whether work is waiting that the held vector would fit. It is a pure
// projection of the same inputs the scheduler plans from, so the warning, the
// metric, the doctor finding, and the reap can never disagree about a hold.
type Occupancy struct {
	Instance  string
	Profile   domain.ProfileID
	Repo      string
	Demand    domain.DemandKey
	Resources domain.Resources
	Age       time.Duration
	// Budget is the profile's ceiling, or zero when the profile sets none. Warned
	// and OverBudget are always false in that case: an unbounded hold cannot be
	// past a bound.
	Budget     time.Duration
	Warned     bool
	OverBudget bool
	// StarvesQueuedDemand reports that at least one demand the fleet has queued
	// and not yet incarnated would fit inside this instance's vector. It is what
	// separates a slow job from a fleet incident, and it is the condition
	// `fleet doctor` names.
	StarvesQueuedDemand bool
}

// Occupancies reports every live instance that is holding a vector, ordered by
// instance identity so a rendering, a metric, and a log line are stable.
func Occupancies(now time.Time, config Config, instances []domain.Instance, demands []domain.Demand) []Occupancy {
	waiting := queuedVectors(config, instances, demands)
	reports := make([]Occupancy, 0, len(instances))
	for _, instance := range sortedInstances(instances) {
		age, measured := instance.Occupancy(now)
		if !measured {
			continue
		}
		report := Occupancy{Instance: instance.ID, Profile: instance.Profile, Repo: instance.Repo,
			Demand: instance.Demand, Resources: instance.Resources, Age: age,
			Budget: config.Profiles[instance.Profile].OccupancyBudget}
		if report.Budget > 0 {
			report.Warned = age >= time.Duration(float64(report.Budget)*OccupancyWarnFraction)
			report.OverBudget = age >= report.Budget
		}
		for _, required := range waiting {
			if instance.Resources.CanFit(required) {
				report.StarvesQueuedDemand = true
				break
			}
		}
		reports = append(reports, report)
	}
	return reports
}

// queuedVectors is the resource vector of every demand that is waiting: queued,
// configured, and not already incarnated by a live instance. A demand whose own
// instance exists is not waiting on anybody's vector.
func queuedVectors(config Config, instances []domain.Instance, demands []domain.Demand) []domain.Resources {
	incarnated := make(map[domain.DemandKey]bool, len(instances))
	for _, instance := range instances {
		if instance.IncarnatesDemand() {
			incarnated[instance.Demand] = true
		}
	}
	vectors := make([]domain.Resources, 0, len(demands))
	for _, demand := range demands {
		profile, known := config.Profiles[demand.Profile]
		if !known || incarnated[demand.Key] {
			continue
		}
		vectors = append(vectors, profile.Resources)
	}
	return vectors
}

func normalizedDemands(in Input) []domain.Demand {
	seen := make(map[domain.DemandKey]bool)
	result := make([]domain.Demand, 0, len(in.Demands.Value))
	for _, demand := range in.Demands.Value {
		profile, ok := in.Config.Profiles[demand.Profile]
		if seen[demand.Key] || demand.Key.Validate() != nil || !ok || profile.Platform != demand.Platform || profile.Route != demand.Route {
			continue
		}
		if !demand.NotBefore.IsZero() && demand.NotBefore.After(in.Now) {
			continue
		}
		seen[demand.Key] = true
		result = append(result, demand)
	}
	sort.Slice(result, func(i, j int) bool { return demandLess(result[i], result[j]) })
	return result
}

func demandLess(a, b domain.Demand) bool {
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.Before(b.CreatedAt)
	}
	return a.Key.String() < b.Key.String()
}

func partition(demands []domain.Demand) (linux, macos []domain.Demand) {
	for _, demand := range demands {
		switch demand.Platform {
		case domain.PlatformMacOS:
			macos = append(macos, demand)
		case domain.PlatformLinux:
			linux = append(linux, demand)
		}
	}
	return linux, macos
}

func planLinux(in Input, plan Plan, demands []domain.Demand) Plan {
	demands = consumeCompatibleIdle(demands, in.Instances.Value)
	if len(demands) == 0 {
		plan.Next.Reservation = nil
		return plan
	}
	free := linuxFree(in)
	// The exact Linux allocator deliberately has a tiny, auditable search
	// envelope. Production owns at most four Linux VM slots even if an adapter
	// accidentally reports a larger host slot count.
	free.Slots = min(free.Slots, 4)
	// Demands past the fairness age are measured against the starvation-guard
	// envelope, which lifts only the advisory CPU-idle clamp. Young admission and
	// backfill keep the throttled envelope: politeness stays for fresh work.
	agedFree := linuxFreeAged(in)
	agedFree.Slots = min(agedFree.Slots, 4)
	baseCounts := activeRepoCounts(in.Instances.Value)

	if reserved, ok := reservedDemand(in.Prior.Reservation, demands); ok && reservationStillHeadsQueue(in, demands, reserved) {
		profile := in.Config.Profiles[reserved.Profile]
		// A reservation is only ever held by an aged head, so its feasibility is
		// judged against the starvation envelope.
		if feasible(profile.Resources, agedFree, reserved.Key.Repo, baseCounts, nil, in.Config.RepoCaps) {
			plan.Operations = append(plan.Operations, spawnOperation(reserved, nil))
			plan.Next.Reservation = nil
		} else {
			plan.Next.Reservation = copyReservation(in.Prior.Reservation)
			plan.ReservationAxis = reservationAxis(in.Config, profile.Resources, agedFree, reserved.Key.Repo, baseCounts)
			backfill := safeBackfill(in, demands, free, baseCounts, plan.Next.Reservation, nil)
			plan.Operations = append(plan.Operations, backfill...)
			if len(backfill) > 0 {
				plan.Next.DRRCursor = backfill[len(backfill)-1].Demand.Repo
			}
		}
		return plan
	}
	plan.Next.Reservation = nil

	aged, young := splitAged(in.Now, in.Config.FairnessAge, demands)
	if len(aged) > 0 {
		for _, candidate := range aged {
			profile := in.Config.Profiles[candidate.Profile]
			selected := spawnedDemands(plan.Operations)
			if !feasible(profile.Resources, agedFree, candidate.Key.Repo, baseCounts, selected, in.Config.RepoCaps) {
				plan.Next.Reservation = &domain.Reservation{Demand: candidate.Key, Profile: candidate.Profile, Resources: profile.Resources, Since: in.Now}
				occupied := cloneCounts(baseCounts)
				for _, chosen := range selected {
					occupied[chosen.Key.Repo]++
				}
				plan.ReservationAxis = reservationAxis(in.Config, profile.Resources, agedFree, candidate.Key.Repo, occupied)
				backfill := safeBackfill(in, demands, free, baseCounts, plan.Next.Reservation, selected)
				plan.Operations = append(plan.Operations, backfill...)
				if len(backfill) > 0 {
					plan.Next.DRRCursor = backfill[len(backfill)-1].Demand.Repo
				}
				break
			}
			plan.Operations = append(plan.Operations, spawnOperation(candidate, nil))
			// An admitted aged spawn consumes real capacity in both envelopes.
			agedFree, _ = agedFree.Sub(profile.Resources)
			if remaining, ok := free.Sub(profile.Resources); ok {
				free = remaining
			} else {
				free = domain.Resources{}
			}
		}
		return plan
	}

	ordered := youngPriorityOrder(in, young, in.Prior.DRRCursor, in.Config)
	selected := exactSelect(in.Now, ordered, free, baseCounts, in.Config)
	for _, candidate := range selected {
		plan.Operations = append(plan.Operations, spawnOperation(candidate, nil))
	}
	if len(selected) > 0 {
		plan.Next.DRRCursor = selected[len(selected)-1].Key.Repo
	}
	return plan
}

func linuxFree(in Input) domain.Resources {
	return freeCapacity(in, in.Instances.Value, false)
}

// linuxFreeAged is the starvation-guard envelope for demands past the fairness
// age. It differs from linuxFree in exactly one term: the advisory CPU-idle
// clamp does not apply. Every hard bound is untouched -- the Linux-only cap, the
// physical total net of live reservations, the measured memory residual, and the
// slot ceiling -- so aged work can wait on real capacity but never on a quiet
// moment that a host with an active tenant may never produce.
func linuxFreeAged(in Input) domain.Resources {
	return freeCapacity(in, in.Instances.Value, true)
}

// freeCapacity is the admission envelope given a set of occupying instances.
// Taking the instances as a parameter lets callers ask the same question about a
// hypothetical occupancy under whichever capacity model is configured, so the
// static and elastic models can never drift apart.
// A configured host budget applies last and to both models, because it is a
// ceiling on the total rather than a term of either one: it must narrow the
// elastic model's physical bound and the static model's configured constant
// alike, and it must survive the aged CPU-idle lift below. Aged work escapes an
// advisory throttle; it does not escape a declared ceiling.
func freeCapacity(in Input, instances []domain.Instance, aged bool) domain.Resources {
	live := liveResources(instances)
	free := staticFree(in, instances)
	if in.Config.ElasticHostEnvelope {
		free = elasticFree(in, instances, live, aged)
	}
	return budgetBound(free, in.Config.HostBudget, live)
}

// budgetBound narrows an envelope by the operator's declared ceiling less what
// live instances of every platform already hold. It reuses the physical-total
// arithmetic deliberately: a budget is a physical total the operator asserts
// instead of one the probe measured, so an unset dimension imposes no bound for
// exactly the reason an unobserved one does not.
func budgetBound(free, budget, live domain.Resources) domain.Resources {
	return domain.Resources{
		CPU:      physicalBound(free.CPU, budget.CPU, live.CPU),
		MemoryMB: physicalBound(free.MemoryMB, budget.MemoryMB, live.MemoryMB),
		Slots:    physicalBound(free.Slots, budget.Slots, live.Slots),
	}
}

// staticFree is ADR 0012's envelope: one configured cross-platform constant that
// every live instance is subtracted from, clamped by the measured residual.
func staticFree(in Input, instances []domain.Instance) domain.Resources {
	free := in.Config.LinuxCapacity
	for _, instance := range instances {
		if instance.ConsumesHostResources() {
			var ok bool
			free, ok = free.Sub(instance.Resources)
			if !ok {
				return domain.Resources{}
			}
		}
	}
	return minResources(free, in.Host.Value.Available)
}

// liveResources totals what instances hold on the host right now, regardless of
// platform. It is the term every total bound is charged against.
func liveResources(instances []domain.Instance) domain.Resources {
	live := domain.Resources{}
	for _, instance := range instances {
		if !instance.ConsumesHostResources() {
			continue
		}
		live = live.Add(instance.Resources)
	}
	return live
}

// elasticFree bounds admission by the observed physical host instead of a static
// configured constant, so the fleet can expand into an idle host and yield as the
// host's own tenant gets busy. Three bounds apply together:
//
//   - LinuxCapacity caps Linux only, which is what its name says. A macOS VM no
//     longer consumes the Linux budget.
//   - The observed physical total (Host.Capacity) is charged for every live
//     instance regardless of platform, so aggregate reservations can never exceed
//     the real machine even during a boot burst.
//   - The measured residual (Host.Available) clamps the result. It is already net
//     of everything running, so live instances are not charged against it twice.
func elasticFree(in Input, instances []domain.Instance, live domain.Resources, aged bool) domain.Resources {
	free := in.Config.LinuxCapacity
	for _, instance := range instances {
		if !instance.ConsumesHostResources() || instance.Platform != domain.PlatformLinux {
			continue
		}
		remaining, ok := free.Sub(instance.Resources)
		if !ok {
			return domain.Resources{}
		}
		free = remaining
	}
	total := in.Host.Value.Capacity
	free = domain.Resources{
		CPU:      physicalBound(free.CPU, total.CPU, live.CPU),
		MemoryMB: physicalBound(free.MemoryMB, total.MemoryMB, live.MemoryMB),
		Slots:    physicalBound(free.Slots, total.Slots, live.Slots),
	}
	available := in.Host.Value.Available
	// Instantaneous CPU idle is an advisory throttle, and the probe's own design
	// forbids advisory signals from fail-closing admission indefinitely. For a
	// demand past the fairness age the clamp is lifted: ADR 0012 promises aging
	// prevents starvation, and FIFO position is worthless if the envelope never
	// opens. vCPUs time-share, so admitting an aged job against the physical
	// bound degrades gracefully; memory does not time-share, so its measured
	// residual stays hard for aged work too.
	if aged {
		available.CPU = free.CPU
	}
	return minResources(free, available)
}

// physicalBound narrows one configured dimension by the observed physical total
// less what live instances already hold. A non-positive total means the adapter
// did not observe that dimension, so the configured cap stands alone: an
// unobserved physical total must never masquerade as a zero measurement and
// silently close admission.
func physicalBound(configured, total, live int) int {
	if total <= 0 {
		return configured
	}
	return min(configured, max(0, total-live))
}

func minResources(a, b domain.Resources) domain.Resources {
	return domain.Resources{CPU: min(a.CPU, b.CPU), MemoryMB: min(a.MemoryMB, b.MemoryMB), Slots: min(a.Slots, b.Slots)}
}

func activeRepoCounts(instances []domain.Instance) map[string]int {
	counts := make(map[string]int)
	for _, instance := range instances {
		if !instance.Live() || instance.State == domain.InstanceOnlineIdle || teardownState(instance.State) {
			continue
		}
		counts[instance.Repo]++
	}
	return counts
}

func teardownState(state domain.InstanceState) bool {
	switch state {
	case domain.InstanceDraining, domain.InstanceDeregistering, domain.InstanceStopping, domain.InstanceDeleted, domain.InstanceFailed:
		return true
	default:
		return false
	}
}

func consumeCompatibleIdle(demands []domain.Demand, instances []domain.Instance) []domain.Demand {
	idle := make(map[string]int)
	for _, instance := range instances {
		if instance.State == domain.InstanceOnlineIdle {
			idle[matchKey(instance.Repo, instance.Profile, instance.Route)]++
		}
	}
	result := make([]domain.Demand, 0, len(demands))
	for _, demand := range demands {
		key := matchKey(demand.Key.Repo, demand.Profile, demand.Route)
		if idle[key] > 0 {
			idle[key]--
			continue
		}
		result = append(result, demand)
	}
	return result
}

func matchKey(repo string, profile domain.ProfileID, route domain.Route) string {
	return repo + "\x00" + string(profile) + "\x00" + string(route)
}

func reservedDemand(reservation *domain.Reservation, demands []domain.Demand) (domain.Demand, bool) {
	if reservation == nil {
		return domain.Demand{}, false
	}
	for _, demand := range demands {
		if demand.Key == reservation.Demand && demand.Profile == reservation.Profile {
			return demand, true
		}
	}
	return domain.Demand{}, false
}

// reservationStillHeadsQueue reports whether a held reservation still names the
// aged global-FIFO head, which is the only demand a reservation may ever be for.
//
// A reservation is made for the oldest aged demand that does not fit, and it is
// re-checked FIRST on every later tick so it wins the first vector large enough
// for it (ADR 0017, ADR 0029). That contract protects the head from work that is
// YOUNGER than it. It was never a licence to outrank work that is OLDER, and the
// plannable queue is not frozen while a reservation is held: a demand a live
// instance incarnates is absent from it by ADR 0027 and returns to it the instant
// that instance dies -- carrying its GitHub queue time, which is what every aging
// rule in this package measures from.
//
// So a recovery drain can put a demand back in front of the reserved head, and
// before this check the head was admitted anyway: `planLinux` returned from the
// reservation branch without ever consulting the queue it had just been handed,
// even though `priorityOrder` had already ranked the returning demand first. That
// is ADR 0004's rule 1 broken by the very mechanism that exists to keep it, and
// the simulator found it as a repeating cycle -- an assignment that stalls, is
// recovered, releases the oldest demand, and loses the freed vector to the
// standing reservation, every deadline, forever (issue #208, seed 55 of the
// container-node arm).
//
// When the reservation no longer heads the queue it is re-derived instead of
// obeyed: the aged loop below admits the true head and reserves whatever it
// cannot fit, so the demand that lost the reservation is first in line behind the
// older work that displaced it. Nothing younger gains anything either way.
func reservationStillHeadsQueue(in Input, demands []domain.Demand, reserved domain.Demand) bool {
	for _, demand := range demands {
		if demand.Key == reserved.Key || !demandAged(in.Now, in.Config.FairnessAge, demand) {
			continue
		}
		if outranksReservation(in, demand, reserved) {
			return false
		}
	}
	return true
}

// outranksReservation asks the aged band's own question of two aged demands:
// higher priority tier first, then older first. It must be the aged band's rule
// and not age alone, or a reservation made before a tier escalated would keep
// being obeyed after priorityOrder stopped agreeing with it -- which is the
// 2026-08-09 incident reappearing through the mechanism that exists to protect
// the head (issue #224). With no tier declared the tiers are equal and this is
// demandLess, exactly as it was.
func outranksReservation(in Input, candidate, reserved domain.Demand) bool {
	candidateTier := effectiveTier(in.Now, candidate, in.Config)
	reservedTier := effectiveTier(in.Now, reserved, in.Config)
	if candidateTier != reservedTier {
		return candidateTier > reservedTier
	}
	return demandLess(candidate, reserved)
}

func copyReservation(reservation *domain.Reservation) *domain.Reservation {
	if reservation == nil {
		return nil
	}
	copy := *reservation
	return &copy
}

// safeBackfill reserves the full resource vector, then performs exact
// admission only inside the component-wise remainder. Thus backfill can never
// delay the reserved job once its non-resource blocker clears.
//
// When the reserved head does not fit the free envelope AT ALL that remainder is
// empty, and holding the whole residual idle protects nothing: such a head is
// blocked by live instances holding the resources it needs, so it cannot start
// until they release no matter what backfill does — it is NOT waiting on
// backfill to stop. This is the same distinction PR #78/#83 drew for a
// resource-infeasible macOS head, and leaving it unhandled for the Linux head
// froze the queue in the 2026-07-25 incident: an aged 4 CPU / 8192 MB `large`
// head behind a macOS builder wedged in `draining` (7 CPU / 12288 MB, its
// deregister refused because the runner was busy) starved five medium and five
// large jobs for ~45 minutes on a host with 3 CPU / 4096 MB free — room for a
// `medium`. Admission is therefore permitted in the residual envelope, and the
// reservation contract is preserved by ordering, not by idleness: the reserved
// head is re-checked FIRST on every later tick (see planLinux's reservedDemand
// branch), so it wins the first vector large enough for it.
//
// A head its own REPOSITORY CAP holds out lends the same way, and for the same
// reason (issue #226, ADR 0038). `feasible` folds two terms and `planLinux`
// holds the reservation when either one refuses the head; the vector term has
// had this release rule since 2026-07-25 and the cap term never did, although
// ADR 0017's rationale transfers word for word — a head at its repository's cap
// cannot start until one of that repository's own instances exits, whatever
// backfill does. It is not waiting on backfill to stop either, and withholding
// costs an idle vector the size of the starved profile for the entire runtime of
// the blocking job (ADR 0029's units).
//
// On today's envelope arithmetic the two terms are exhaustive, which is worth
// stating because it sizes the change. The head is judged in the starvation
// envelope and backfill plans in the throttled one, and the first always
// contains the second, so a head refused on the VECTOR can never satisfy
// `free.Sub` here. The withheld branch was therefore only ever reachable for a
// CAP-held head: issue #226 is not a corner of this function, it is the whole of
// its remainder path.
//
// What is lent is bounded by ADR 0017's own promise that "no equal-or-larger job
// can jump the queue". That promise is automatic while the lent capacity belongs
// to a head that does not FIT it — a candidate inside `free` must be strictly
// smaller than a head that overflows `free`. A cap-held head fits, so the
// promise becomes `takesTheReservedVector` and is applied to every candidate
// whenever anything beyond the remainder is lent: provably a no-op on the vector
// axis, load-bearing on the cap axis.
func safeBackfill(in Input, demands []domain.Demand, free domain.Resources, baseCounts map[string]int, reservation *domain.Reservation, alreadySelected []domain.Demand) []Operation {
	counts := cloneCounts(baseCounts)
	for _, demand := range alreadySelected {
		counts[demand.Key.Repo]++
	}
	remainder, fits := free.Sub(reservation.Resources)
	backfillCapacity := remainder
	lends := !fits || reservedHeadAtRepositoryCap(in.Config, counts, reservation)
	if lends {
		backfillCapacity = free
	}
	excluded := map[domain.DemandKey]bool{reservation.Demand: true}
	for _, demand := range alreadySelected {
		excluded[demand.Key] = true
	}
	var candidates []domain.Demand
	for _, demand := range demands {
		if excluded[demand.Key] {
			continue
		}
		// Whatever is lent BEYOND `free - reservation` is lent only to work the
		// head outranks. ADR 0017's queue-position guarantee is a rule here, not
		// a by-product of the subtraction that used to imply it.
		if lends && jumpsTheReservedHead(in.Config, reservation, remainder, fits, demand) {
			continue
		}
		candidates = append(candidates, demand)
	}
	// The backfill lane ranks its own candidates by the one order this package
	// has: aged band first, priority tier inside each band.
	ordered := priorityOrder(in, candidates)
	selected := exactSelect(in.Now, ordered, backfillCapacity, counts, in.Config)
	operations := make([]Operation, 0, len(selected))
	for _, demand := range selected {
		operations = append(operations, spawnOperation(demand, nil))
	}
	return operations
}

// reservedHeadAtRepositoryCap reports whether the reserved head's OWN repository
// cap is what holds it out of admission, rather than the resource envelope.
//
// This is the axis ADR 0017 never covered and ADR 0038 adds. The occupancy read
// here is exactly the one `feasible` reads on the tick the head's cap slot
// frees — live instances plus everything this plan already admits — so the two
// predicates can never disagree about which axis is holding the head.
func reservedHeadAtRepositoryCap(config Config, occupied map[string]int, reservation *domain.Reservation) bool {
	return occupied[reservation.Demand.Repo] >= repoCapLimit(config.RepoCaps, reservation.Demand.Repo)
}

// takesTheReservedVector reports whether one candidate is equal to or larger
// than the reserved head's vector, which is the job ADR 0017 promises can never
// jump the queue:
//
//	Because anything admitted in this path is by construction too small to fit
//	the reserved vector, no equal-or-larger job can jump the queue.
//
// "By construction" is exact, and it is a construction of the FIT TEST rather
// than a rule anybody wrote down. It holds automatically while the only capacity
// ever lent belongs to a head that does not fit `free`: such a head overflows
// `free` in some dimension, and a candidate bounded by `free` is strictly
// smaller there, so it cannot contain the head's vector. The moment a head that
// DOES fit lends its vector — which is what a cap-held head does under ADR 0038
// — the construction ends, because `free` now holds the head's whole vector and
// an equal-or-larger peer could take it and invert the aged FIFO the reservation
// exists to protect.
//
// So the construction becomes a predicate, applied wherever capacity beyond
// `free - reservation` is lent, on BOTH axes. On the vector axis it is provably
// a no-op, and keeping it there is the point: the guarantee is now stated in one
// place and tested directly instead of being re-derived from arithmetic every
// time somebody changes what a reservation lends.
func takesTheReservedVector(config Config, reservation *domain.Reservation, demand domain.Demand) bool {
	return config.Profiles[demand.Profile].Resources.CanFit(reservation.Resources)
}

// jumpsTheReservedHead reports whether admitting one candidate would let an
// equal-or-larger job take the vector the reserved head is entitled to.
//
// The distinction that matters is WHICH capacity the candidate lands in. Work
// that fits `free - reservation` sits BESIDE the head and cannot delay it by a
// tick: it is admitted for exactly the reason the remainder has always admitted
// work, and no jump is possible because the head's own vector is untouched.
// Applying the no-jump rule there would be strictly worse than the behaviour
// this decision replaces -- it would refuse a `large` that coexists with a
// cap-held `medium` head, which is the very sterilization issue #226 is about.
//
// The rule binds only on a candidate that must eat INTO the head's vector, and
// there it is ADR 0017's guarantee verbatim: nothing equal or larger takes that
// vector. When the head does not fit `free` at all there is no remainder to sit
// beside, so every candidate is judged by the rule -- where, as above, it is
// provably a no-op.
func jumpsTheReservedHead(config Config, reservation *domain.Reservation, remainder domain.Resources,
	remainderExists bool, demand domain.Demand) bool {
	if remainderExists && remainder.CanFit(config.Profiles[demand.Profile].Resources) {
		return false
	}
	return takesTheReservedVector(config, reservation, demand)
}

// demandAged reports whether one demand has crossed the fairness age, using the
// same rule splitAged applies to whole lanes.
func demandAged(now time.Time, age time.Duration, demand domain.Demand) bool {
	return age > 0 && !demand.CreatedAt.IsZero() && now.Sub(demand.CreatedAt) >= age
}

func splitAged(now time.Time, age time.Duration, demands []domain.Demand) (aged, young []domain.Demand) {
	for _, demand := range demands {
		if demandAged(now, age, demand) {
			aged = append(aged, demand)
		} else {
			young = append(young, demand)
		}
	}
	return aged, young
}

// effectiveTier is a demand's priority tier after aging escalation: the tier
// configuration classified it into, plus one rank for every whole
// PriorityEscalation it has waited.
//
// Escalation is what makes tiers safe. A tier alone is a licence for one class
// of work to overtake another indefinitely; escalation converts that into a
// bounded overtaking window, because a waiting demand outranks anything of rank
// N once it is N*PriorityEscalation older. It is monotonic by construction --
// floor(wait / threshold) never falls -- so a demand can never lose ground it
// has already gained, and two demands of one tier stay in age order because they
// escalate together.
//
// A demand with no creation time cannot be aged; it keeps the rank it was
// classified with, exactly as demandAged refuses to age it.
func effectiveTier(now time.Time, demand domain.Demand, config Config) int {
	if config.PriorityEscalation <= 0 || demand.CreatedAt.IsZero() {
		return demand.Priority.Rank
	}
	waited := now.Sub(demand.CreatedAt)
	if waited <= 0 {
		return demand.Priority.Rank
	}
	return demand.Priority.Rank + int(waited/config.PriorityEscalation)
}

// priorityOrder sorts by (tier, age) INSIDE each of ADR 0004's bands. The band
// structure is untouched -- aged global FIFO, then young control-plane, then
// young standard -- and the priority tier is the first key within a band, with
// age the second.
//
// Aging stays the outermost key deliberately. ADR 0004 calls aging the absolute
// starvation guard and this change does not demote it: a declared tier decides
// between demands that have waited comparably, not between a fresh job and one
// that has been queued past the fairness age. That is also what keeps the
// allocators honest -- planLinux re-derives this same aged/young split from the
// list priorityOrder hands it, so a tier that reordered the bands themselves
// would be silently discarded there (the simulator found exactly that, as a
// tier_inversion on the tiered arm's seed 1).
//
// Within the aged band a tier is still decisive, which is the 2026-08-09
// incident: both the release and the pull request's E2E build had waited over an
// hour, so both were aged, and only the tier could tell them apart.
//
// With no tier declared every demand has effective tier zero, every band has one
// group, and this is byte-for-byte the order this function produced before
// issue #224.
func priorityOrder(in Input, demands []domain.Demand) []domain.Demand {
	aged, young := splitAged(in.Now, in.Config.FairnessAge, demands)
	return append(byTier(in, aged), youngPriorityOrder(in, young, in.Prior.DRRCursor, in.Config)...)
}

// byTier orders one band by effective priority tier, highest first. The sort is
// stable, so demands of one tier keep the order the band already had -- which is
// age for the aged band and the throughput lane's own order for a young one.
func byTier(in Input, demands []domain.Demand) []domain.Demand {
	if len(demands) < 2 {
		return demands
	}
	ordered := append([]domain.Demand(nil), demands...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return effectiveTier(in.Now, ordered[i], in.Config) > effectiveTier(in.Now, ordered[j], in.Config)
	})
	return ordered
}

// youngPriorityOrder implements two bounded lanes. Control-plane work can
// bypass only young standard work; aged global FIFO is assembled by
// priorityOrder before either lane and therefore remains absolute.
func youngPriorityOrder(in Input, demands []domain.Demand, cursor string, config Config) []domain.Demand {
	controlPlane := make([]domain.Demand, 0, len(demands))
	standard := make([]domain.Demand, 0, len(demands))
	for _, demand := range demands {
		if config.RepoSchedulingClasses[demand.Key.Repo] == domain.SchedulingControlPlane {
			controlPlane = append(controlPlane, demand)
		} else {
			standard = append(standard, demand)
		}
	}
	return append(byTier(in, throughputOrder(controlPlane, cursor, config)),
		byTier(in, throughputOrder(standard, cursor, config))...)
}

type throughputBucket struct {
	event     int
	resources domain.Resources
	demands   []domain.Demand
}

// throughputOrder is a bounded shortest-resource-first policy for young work.
// Aging is applied before this function, so a large job becomes globally FIFO
// before this optimization can starve it. Within an event/resource band the
// existing deterministic repository round-robin remains authoritative.
func throughputOrder(demands []domain.Demand, cursor string, config Config) []domain.Demand {
	var buckets []throughputBucket
	for _, demand := range demands {
		resources := config.Profiles[demand.Profile].Resources
		bucket := -1
		for index := range buckets {
			if buckets[index].event == eventRank(demand.Event) && buckets[index].resources == resources {
				bucket = index
				break
			}
		}
		if bucket < 0 {
			buckets = append(buckets, throughputBucket{event: eventRank(demand.Event), resources: resources})
			bucket = len(buckets) - 1
		}
		buckets[bucket].demands = append(buckets[bucket].demands, demand)
	}
	sort.Slice(buckets, func(i, j int) bool {
		if buckets[i].event != buckets[j].event {
			return buckets[i].event < buckets[j].event
		}
		return resourceCostLess(buckets[i].resources, buckets[j].resources, config.LinuxCapacity)
	})
	var ordered []domain.Demand
	for _, bucket := range buckets {
		ordered = append(ordered, fairOrder(bucket.demands, cursor)...)
	}
	return ordered
}

func resourceCostLess(a, b, capacity domain.Resources) bool {
	an, ad := dominantShare(a, capacity)
	bn, bd := dominantShare(b, capacity)
	if an*bd != bn*ad {
		return an*bd < bn*ad
	}
	if a.CPU != b.CPU {
		return a.CPU < b.CPU
	}
	if a.MemoryMB != b.MemoryMB {
		return a.MemoryMB < b.MemoryMB
	}
	return a.Slots < b.Slots
}

func dominantShare(resources, capacity domain.Resources) (int64, int64) {
	numerator, denominator := int64(resources.CPU), int64(max(capacity.CPU, 1))
	for _, share := range [][2]int{{resources.MemoryMB, max(capacity.MemoryMB, 1)}, {resources.Slots, max(capacity.Slots, 1)}} {
		nextNumerator, nextDenominator := int64(share[0]), int64(share[1])
		if nextNumerator*denominator > numerator*nextDenominator {
			numerator, denominator = nextNumerator, nextDenominator
		}
	}
	return numerator, denominator
}

func fairOrder(demands []domain.Demand, cursor string) []domain.Demand {
	byRepo := make(map[string][]domain.Demand)
	var repos []string
	for _, demand := range demands {
		if _, exists := byRepo[demand.Key.Repo]; !exists {
			repos = append(repos, demand.Key.Repo)
		}
		byRepo[demand.Key.Repo] = append(byRepo[demand.Key.Repo], demand)
	}
	sort.Strings(repos)
	for repo := range byRepo {
		sort.Slice(byRepo[repo], func(i, j int) bool {
			a, b := byRepo[repo][i], byRepo[repo][j]
			if eventRank(a.Event) != eventRank(b.Event) {
				return eventRank(a.Event) < eventRank(b.Event)
			}
			return demandLess(a, b)
		})
	}
	if len(repos) > 1 && cursor != "" {
		start := sort.SearchStrings(repos, cursor)
		if start < len(repos) && repos[start] == cursor {
			start = (start + 1) % len(repos)
			repos = append(append([]string{}, repos[start:]...), repos[:start]...)
		}
	}
	var result []domain.Demand
	for round := 0; ; round++ {
		added := false
		for _, repo := range repos {
			if round < len(byRepo[repo]) {
				result = append(result, byRepo[repo][round])
				added = true
			}
		}
		if !added {
			return result
		}
	}
}

func eventRank(event domain.Event) int {
	if event == domain.EventPullRequest {
		return 0
	}
	return 1
}

// exactSelect enumerates all feasible combinations while selecting at most the
// configured slot vector (four in production). That makes feasibility exact
// without an unbounded 2^N search.
func exactSelect(now time.Time, candidates []domain.Demand, free domain.Resources, baseCounts map[string]int, config Config) []domain.Demand {
	// Priority band per candidate is ADR 0004's scheduling order: lower band is
	// higher priority. Count maximization is a throughput optimization that
	// must apply only WITHIN a band; it must never admit a larger count of
	// lower-priority work while a feasible higher-priority demand is deferred.
	bands := admissionBands(now, candidates, config)
	numBands := 0
	for _, band := range bands {
		if band+1 > numBands {
			numBands = band + 1
		}
	}
	var best []int
	var search func(index int, remaining domain.Resources, counts map[string]int, chosen []int)
	search = func(index int, remaining domain.Resources, counts map[string]int, chosen []int) {
		if betterAdmission(chosen, best, bands, numBands) {
			best = append([]int(nil), chosen...)
		}
		if index >= len(candidates) || len(chosen) >= free.Slots {
			return
		}
		for i := index; i < len(candidates); i++ {
			candidate := candidates[i]
			profile := config.Profiles[candidate.Profile]
			if counts[candidate.Key.Repo] >= repoCapLimit(config.RepoCaps, candidate.Key.Repo) ||
				!remaining.CanFit(profile.Resources) {
				continue
			}
			nextRemaining, _ := remaining.Sub(profile.Resources)
			nextCounts := cloneCounts(counts)
			nextCounts[candidate.Key.Repo]++
			search(i+1, nextRemaining, nextCounts, append(chosen, i))
		}
	}
	search(0, free, cloneCounts(baseCounts), nil)
	selected := make([]domain.Demand, 0, len(best))
	for _, index := range best {
		selected = append(selected, candidates[index])
	}
	return selected
}

// schedulingBand assigns a demand its priority band for exact admission. The
// bands ARE ADR 0004's scheduling order, in its order: aged work first, in one
// class-blind global FIFO band, then young control-plane, then young standard.
//
// Aging led that list before this function existed, and it must lead it here
// too. Bands used to carry the scheduling class alone, on the reasoning that
// aging is guarded by the reservation head outside exactSelect. It is not: the
// reservation protects ONE head, while safeBackfill and the bounded drain
// backfill hand exactSelect aged and young candidates in the same slice, and
// band coverage is compared before anything else. A young control-plane demand
// therefore outranked an aged standard one — ADR 0004's rule 2 ahead of its
// rule 1 — and the aged demand's global FIFO position bought it nothing
// (FINDING 3 of ADR 0031).
//
// priorityOrder builds exactly these three groups in exactly this order, so a
// candidate list and its band vector now say the same thing.
// admissionBands is the band vector priorityOrder's ordering implies: ADR 0004's
// three bands are the major key and the priority tier is the minor one, which is
// exactly the composition priorityOrder makes -- split by age, order each band by
// tier.
//
// Tiers are compressed to the ranks actually present before they are folded in,
// so the band space stays proportional to the queue rather than to how long its
// oldest demand has been escalating. With one tier present every band index is
// schedulingBand's own, which is what keeps an undeclared policy from moving a
// single admission.
func admissionBands(now time.Time, candidates []domain.Demand, config Config) []int {
	tiers := make([]int, len(candidates))
	present := make([]int, 0, len(candidates))
	seen := make(map[int]bool, len(candidates))
	for i := range candidates {
		tiers[i] = effectiveTier(now, candidates[i], config)
		if !seen[tiers[i]] {
			seen[tiers[i]] = true
			present = append(present, tiers[i])
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(present)))
	rank := make(map[int]int, len(present))
	for index, tier := range present {
		rank[tier] = index
	}
	bands := make([]int, len(candidates))
	for i := range candidates {
		bands[i] = schedulingBand(now, candidates[i], config)*len(present) + rank[tiers[i]]
	}
	return bands
}

func schedulingBand(now time.Time, demand domain.Demand, config Config) int {
	if demandAged(now, config.FairnessAge, demand) {
		return 0
	}
	if config.RepoSchedulingClasses[demand.Key.Repo] == domain.SchedulingControlPlane {
		return 1
	}
	return 2
}

// betterAdmission ranks two feasible index selections by ADR 0004's scheduling
// order. Bands are compared in priority order (band 0 highest), and each band is
// compared exactly as a single lane always was: more admitted demands first,
// then the earlier candidates. The candidate list is already in priority order,
// so a lower index is older work, and one band's answer is settled before the
// next band is consulted at all.
//
// Comparing a band's MEMBERS and not merely its count is what keeps rule 1 above
// rule 2. Counts alone tie whenever two selections admit the same NUMBER of aged
// demands, and that tie used to be broken by the young control-plane band: an
// aged `large` heading the residual lost it to an aged `small` plus a
// one-minute-old control-plane `medium`, because the pair covered one aged
// demand too and added a band-1 admission. Rule 1 is a FIFO over aged work, so
// WHICH aged demand is admitted is decided before any young lane may weigh in.
//
// With every candidate in one band this is byte-for-byte the original
// count/index-lexicographic preference, which is what keeps throughput and
// determinism unchanged for the common case.
func betterAdmission(candidate, incumbent, bands []int, numBands int) bool {
	for band := range numBands {
		candidateBand := bandMembers(candidate, bands, band)
		incumbentBand := bandMembers(incumbent, bands, band)
		if len(candidateBand) != len(incumbentBand) {
			return len(candidateBand) > len(incumbentBand)
		}
		for i := range candidateBand {
			if candidateBand[i] != incumbentBand[i] {
				return candidateBand[i] < incumbentBand[i]
			}
		}
	}
	return false
}

// bandMembers is the chosen candidates that fall in one band, in candidate
// order. bands maps a candidate index to its band.
func bandMembers(chosen, bands []int, band int) []int {
	members := make([]int, 0, len(chosen))
	for _, index := range chosen {
		if bands[index] == band {
			members = append(members, index)
		}
	}
	return members
}

func cloneCounts(source map[string]int) map[string]int {
	result := make(map[string]int, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func feasible(resources, free domain.Resources, repo string, base map[string]int, selected []domain.Demand, caps map[string]int) bool {
	if !free.CanFit(resources) {
		return false
	}
	count := base[repo]
	for _, demand := range selected {
		if demand.Key.Repo == repo {
			count++
		}
	}
	return count < repoCapLimit(caps, repo)
}

// repoCapLimit is one repository's configured concurrency cap. An omitted or
// non-positive cap normalizes to one, so an unconfigured repository is bounded
// rather than unbounded. Every admission path reads the cap through here, which
// is what keeps the bound identical for Linux exact admission, macOS admission,
// and ADR 0030's reserved-slack arithmetic.
func repoCapLimit(caps map[string]int, repo string) int {
	if limit := caps[repo]; limit > 0 {
		return limit
	}
	return 1
}

func spawnedDemands(operations []Operation) []domain.Demand {
	result := make([]domain.Demand, 0, len(operations))
	for _, operation := range operations {
		if operation.Kind == OperationSpawn {
			result = append(result, domain.Demand{Key: operation.Demand})
		}
	}
	return result
}

func planMacHandoff(in Input, plan Plan, linuxDemands, macDemands []domain.Demand) Plan {
	macDemands = stableMacHandoffOrder(in.Prior.MacHandoff, macDemands)
	handoff := macHandoffFor(in.Prior.MacHandoff, macDemands[0], in.Now)
	var drains []Operation
	allIdle := true
	filtered := append([]domain.Instance(nil), in.Instances.Value...)
	for _, instance := range sortedInstances(in.Instances.Value) {
		if instance.Platform != domain.PlatformLinux || !instance.ConsumesHostResources() {
			continue
		}
		if instance.State != domain.InstanceOnlineIdle {
			allIdle = false
			continue
		}
		drains = append(drains, drainOperation(instance))
		filtered = removeInstance(filtered, instance.ID)
	}
	plan.Operations = append(plan.Operations, drains...)
	if allIdle {
		nextInput := in
		nextInput.Instances = domain.Fresh(filtered, in.Instances.ObservedAt)
		spawned := appendMacSpawns(nextInput, plan, macDemands, operationIDs(drains))
		if containsSpawn(spawned.Operations) {
			spawned.Next.MacHandoff = nil
			return spawned
		}
		// The macOS head cannot be admitted even with every Linux instance idle
		// or drained (an idle host without headroom for the VM). Fall through to
		// reserve it and bounded-drain feasible Linux instead of stalling — a
		// resource-infeasible head must not deadlock the queue.
	}
	plan.Next.MacHandoff = &handoff
	if !handoff.BackfillAdmitted {
		backfill := boundedDrainBackfill(in, linuxDemands)
		plan.Operations = append(plan.Operations, backfill...)
		if len(backfill) > 0 {
			plan.Next.MacHandoff.BackfillAdmitted = true
		}
	}
	return plan
}

// planBehindInfeasibleMacHead admits feasible work behind a macOS head an idle
// host cannot fit. Unlike planMacHandoff the head is not waiting for live work to
// drain for a profile switch, so the bounded one-shot latch is wrong: it would
// drain the queue by a single job and re-wedge. Instead this mirrors planLinux's
// reservation+backfill and admits feasible work in the residual envelope EVERY
// tick: feasible Linux through the exact allocator (respecting Slots and
// RepoCaps), and — only when no Linux is admitted this tick, so the shared
// envelope is never double-counted — the next feasible macOS profile behind the
// head. The head keeps its FIFO priority and spawns the moment the host can fit
// it. MacHandoff is retained for state continuity but the BackfillAdmitted latch
// is never set here.
func planBehindInfeasibleMacHead(in Input, plan Plan, linuxDemands, macDemands []domain.Demand) Plan {
	handoff := macHandoffFor(in.Prior.MacHandoff, priorityOrder(in, macDemands)[0], in.Now)
	before := len(plan.Operations)
	plan = planLinux(in, plan, linuxDemands)
	plan.Next.MacHandoff = &handoff
	if containsSpawn(plan.Operations[before:]) {
		return plan
	}
	remaining := demandsExcludingProfile(macDemands, chosenMacProfile(in, macDemands))
	if len(remaining) > 0 {
		plan = planMacOS(in, plan, remaining)
	}
	return plan
}

// mixedRemainderInput gates and prepares a second, complementary admission pass.
// It returns ok=false unless mixed admission is safe this tick: the flag is on,
// the plan is ready, and no drain is in flight. Refusing to run while a drain is
// pending is what preserves every platform switch and handoff — a mac->linux or
// linux->mac profile switch (or a bounded handoff drain) must complete without a
// new admission refilling the platform being drained. When ok, the returned
// Input treats this tick's planned spawns as live occupancy, so the second pass
// plans strictly inside the residual envelope and combined consumption can never
// exceed the shared capacity, Host.Available, or the four-slot ceiling.
func mixedRemainderInput(in Input, plan Plan) (Input, bool) {
	if !in.Config.MixedPlatformAdmission || plan.Status != PlanReady || containsDrain(plan.Operations) {
		return in, false
	}
	augmented := in
	augmented.Instances = domain.Fresh(appendPlannedSpawns(in.Instances.Value, in.Config, plan.Operations), in.Instances.ObservedAt)
	return augmented, true
}

// demandsAwaitingAdmission drops the demands this plan already spawns.
//
// mixedRemainderInput teaches a complementary pass what the first pass COST —
// appendPlannedSpawns models the planned spawns as live occupancy — but never
// which demands it already claimed, and both passes receive the same queue. A
// demand admitted by a bounded handoff backfill and then again by the remainder
// pass yields two operations with one content address and two instance intents
// with one instance name. reconcile.Controller.Commit translates both and the
// durable layer refuses the second INSERT with a UNIQUE violation, which the
// tick reports as plan_commit_failed. Because the plan is a pure function of its
// inputs, every later tick rebuilds the same collision: the scheduler wedges
// until the demand or the instance disappears (incident 2026-08-02, ADR 0027).
//
// Filtering here rather than deduplicating the finished plan keeps the second
// pass's own budget honest: a demand the first pass claimed must not be counted
// again against slots, repo caps, or the residual envelope.
func demandsAwaitingAdmission(demands []domain.Demand, operations []Operation) []domain.Demand {
	spawned := make(map[domain.DemandKey]bool, len(operations))
	for _, operation := range operations {
		if operation.Kind == OperationSpawn {
			spawned[operation.Demand] = true
		}
	}
	if len(spawned) == 0 {
		return demands
	}
	result := make([]domain.Demand, 0, len(demands))
	for _, demand := range demands {
		if !spawned[demand.Key] {
			result = append(result, demand)
		}
	}
	return result
}

// reservedRepoSlack reports how many demands in the reserved head's own
// repository a complementary pass may still admit without ever costing the head
// the cap slot it is waiting for.
//
// Resources are not the only way to delay a reserved head. Repository caps gate
// admission too, and activeRepoCounts counts EVERY live instance regardless of
// platform, so a macOS spawn in the reserved head's repository can consume the
// exact cap slot the head is waiting for and block it again the moment its
// vector frees. Remainder arithmetic cannot see that blocker.
//
// The answer is a count, not a veto. The occupancy is exactly the one
// planLinux's feasible() will read on the tick the head's vector frees — live
// instances plus everything this plan already admits (appendPlannedSpawns is how
// both passes charge planned spawns) — and the head's own future slot is
// subtracted before anything else may bid. What remains is genuinely spare:
// admitting it leaves the cap answer for the head unchanged.
func reservedRepoSlack(in Input, plan Plan, repo string) int {
	limit := repoCapLimit(in.Config.RepoCaps, repo)
	occupied := activeRepoCounts(appendPlannedSpawns(in.Instances.Value, in.Config, plan.Operations))
	return limit - occupied[repo] - 1
}

// reservedRemainderDemands drops the demands a complementary pass must not
// admit while a reservation is held, whatever the envelope says.
//
// Everything outside the reserved head's repository passes through untouched.
// Inside it, reservedRepoSlack decides how many demands may bid, and the
// highest-priority ones take those slots — aged before young, the same global
// FIFO order every other admission obeys.
//
// A wholesale exclusion of the repository was ADR 0029's first answer and it
// over-protected: on 2026-08-03 a head whose repository cap was 4 with one live
// instance had three spare slots, and macOS work from that repository aged 4h39m
// was refused the residual its own head could not use, while YOUNGER work from
// another repository was admitted. That inverts the aged FIFO this pass exists
// to protect. A head cannot lose a slot that is still free once its own is set
// aside, so refusing there bought nothing and cost the queue its oldest work.
//
// The reservation is Linux-authored and singular, so the head's own demand is
// never in a macOS candidate list; slack is what the head does not need, never
// what it holds.
func reservedRemainderDemands(in Input, plan Plan, demands []domain.Demand) []domain.Demand {
	reservation := plan.Next.Reservation
	if reservation == nil {
		return demands
	}
	// When the head lends its vector instead of withholding it, the arithmetic
	// that used to keep an equal-or-larger peer out is gone, and ADR 0017's
	// no-jump rule takes its place explicitly: nothing that could take the head's
	// vector whole may bid for the part of it that is lent. Work that still fits
	// BESIDE the head is untouched, because it cannot delay the head at all.
	if reservedHeadLendsItsVector(in, reservation) {
		remainder, remainderExists := linuxFreeAged(in).Sub(reservation.Resources)
		kept := make([]domain.Demand, 0, len(demands))
		for _, demand := range demands {
			if !jumpsTheReservedHead(in.Config, reservation, remainder, remainderExists, demand) {
				kept = append(kept, demand)
			}
		}
		demands = kept
	}
	var sameRepo []domain.Demand
	for _, demand := range demands {
		if demand.Key.Repo == reservation.Demand.Repo {
			sameRepo = append(sameRepo, demand)
		}
	}
	if len(sameRepo) == 0 {
		return demands
	}
	slack := reservedRepoSlack(in, plan, reservation.Demand.Repo)
	admissible := make(map[domain.DemandKey]bool, len(sameRepo))
	for _, demand := range priorityOrder(in, sameRepo) {
		if len(admissible) >= slack {
			break
		}
		admissible[demand.Key] = true
	}
	result := make([]domain.Demand, 0, len(demands))
	for _, demand := range demands {
		if demand.Key.Repo != reservation.Demand.Repo || admissible[demand.Key] {
			result = append(result, demand)
		}
	}
	return result
}

// chargeReservedHead models a held reservation as occupancy for a complementary
// admission pass, exactly as appendPlannedSpawns models this tick's spawns. The
// pass then plans inside free - reservation and can never take a vector the
// reserved head is waiting for.
//
// When the reserved vector does not fit the free envelope AT ALL, nothing is
// charged and the pass plans inside the full residual. That is ADR 0017's rule,
// extended from safeBackfill to the second admission pass: such a head is
// blocked by live instances holding what it needs, so it cannot start until they
// release no matter what this pass admits — it is NOT waiting on the pass to
// stop. The reservation contract is then preserved by ordering, not by idleness:
// the head is re-checked first on every later tick, so it wins the first vector
// large enough for it.
//
// A head its own REPOSITORY CAP holds out is charged nothing either, by the same
// rule and for the same reason (ADR 0038): freeing the vector cannot admit such
// a head, so withholding the vector protects nothing. What replaces the charge
// is not nothing — reservedRemainderDemands drops any candidate that could take
// the head's vector whole, which is ADR 0017's no-jump construction stated
// rather than inherited.
//
// Feasibility is judged against the starvation-guard envelope, the same one
// planLinux judges the head in, because only an aged head ever holds a
// reservation. Judging a head as fitting is the fail-safe answer here: it
// withholds the head's vector.
func chargeReservedHead(in Input, reservation *domain.Reservation) Input {
	if reservation == nil || reservedHeadLendsItsVector(in, reservation) {
		return in
	}
	profile := in.Config.Profiles[reservation.Profile]
	charged := append([]domain.Instance(nil), in.Instances.Value...)
	charged = append(charged, domain.Instance{
		ID: "reserved-" + reservation.Demand.String(), Repo: reservation.Demand.Repo, Demand: reservation.Demand,
		Platform: profile.Platform, Profile: reservation.Profile, Route: profile.Route, Resources: reservation.Resources,
		State: domain.InstancePlanned, Power: domain.InstancePowerRunning,
	})
	in.Instances = domain.Fresh(charged, in.Instances.ObservedAt)
	return in
}

// reservedHeadLendsItsVector reports whether a held reservation lends the vector
// it names to a complementary pass instead of withholding it, on either of the
// two axes that can hold a head out of admission.
//
// It is the single predicate chargeReservedHead and reservedRemainderDemands
// both read, so the charge and the no-jump filter can never disagree about which
// world they are in: whenever the vector is not charged, the filter is on.
func reservedHeadLendsItsVector(in Input, reservation *domain.Reservation) bool {
	if !linuxFreeAged(in).CanFit(reservation.Resources) {
		return true
	}
	return reservedHeadAtRepositoryCap(in.Config, activeRepoCounts(in.Instances.Value), reservation)
}

// fillLinuxRemainder admits Linux work in the envelope left after the macOS head
// has planned. It never drains and never latches, so it is safe to run beside a
// live macOS cohort or a freshly planned macOS spawn. The Linux allocator owns
// the reservation and DRR cursor, so the second pass adopts them.
func fillLinuxRemainder(in Input, plan Plan, linux []domain.Demand) Plan {
	linux = demandsAwaitingAdmission(linux, plan.Operations)
	if len(linux) == 0 {
		return plan
	}
	augmented, ok := mixedRemainderInput(in, plan)
	if !ok {
		return plan
	}
	sub := planLinux(augmented, Plan{Status: PlanReady, Next: plan.Next}, linux)
	plan.Operations = append(plan.Operations, sub.Operations...)
	plan.Next.Reservation = sub.Next.Reservation
	// The sub-pass owns the reservation, so it owns the diagnosis of why it is
	// held. Carrying one without the other would publish a reservation nothing
	// can explain.
	plan.ReservationAxis = sub.ReservationAxis
	plan.Next.DRRCursor = sub.Next.DRRCursor
	return plan
}

// fillMacRemainder admits a compatible macOS profile in the envelope left after
// the Linux head has planned, turning "N Linux" into "N Linux AND a macOS
// cohort" within MaxActive and the single-cohort invariant. remainderMacProfile
// names which profile that is: the live cohort under the single-cohort rule, and
// otherwise the highest-priority queued profile that still has room.
// appendMacSpawns performs no drains, so no macOS profile switch can be started
// as a side effect of a Linux tick; the Linux head keeps ownership of the DRR
// fairness cursor.
//
// A held Linux reservation constrains this pass instead of cancelling it. The
// reserved head's whole vector is withheld (chargeReservedHead) and its
// repository may be bid for only up to the cap slack the head does not need
// (reservedRemainderDemands), so nothing admitted here can delay it; when that
// vector does not fit the free envelope at all the head is not waiting on this
// pass, and ADR 0029 admits inside the full residual rather than stranding it.
// Returning early on any reservation at all is what starved a maestro that fit
// the four free cores exactly for over an hour on 2026-08-02, behind an xl head
// that could not have used them.
func fillMacRemainder(in Input, plan Plan, macos []domain.Demand) Plan {
	macos = reservedRemainderDemands(in, plan, demandsAwaitingAdmission(macos, plan.Operations))
	if len(macos) == 0 {
		return plan
	}
	augmented, ok := mixedRemainderInput(in, plan)
	if !ok {
		return plan
	}
	augmented = chargeReservedHead(augmented, plan.Next.Reservation)
	target, admissible := remainderMacProfile(augmented, macos)
	if !admissible {
		return plan
	}
	profileDemands := demandsForProfile(macos, target)
	if len(profileDemands) == 0 {
		return plan
	}
	savedCursor := plan.Next.DRRCursor
	sub := appendMacSpawns(augmented, Plan{Status: PlanReady, Next: plan.Next}, profileDemands, nil)
	plan.Operations = append(plan.Operations, sub.Operations...)
	plan.Next.DRRCursor = savedCursor
	return plan
}

// remainderMacProfile names the macOS profile a remainder pass may admit, or
// reports that none may.
//
// Under the single-cohort rule the answer is the live cohort and nothing else:
// profile identity is the veto there, so a queue of any other profile is
// inadmissible however much room the Linux head left over.
//
// With `mixedProfileCohorts` the veto is the ENVELOPE, not identity — ADR 0024
// states that as "growth loses only the identity veto" — and that is exactly
// what this pass had wrong. It read the live cohort as the target whatever the
// queue held, so a `builder` sitting at its own `maxActive` of one answered
// "cannot grow" and the whole pass returned, with a queued `maestro` that fit the
// free cores exactly and a configuration that explicitly permits the two to
// coexist. ADR 0024 gave `planMacOS` coexistence-first admission and left the
// complementary remainder pass on the old rule; that is the same asymmetry
// between a first and a second admission pass that ADR 0029 repaired for the
// reservation veto, and it wedged the fleet in issue #208, seed 210: twelve
// consecutive ticks admitting nothing while four idle cores held an aged maestro.
//
// The target is therefore the highest-priority queued demand this pass can
// actually admit, in the aged-FIFO order `priorityOrder` gives every other
// admission, and its profile. A candidate that cannot be admitted ends its own
// turn rather than the pass, which is how `appendMacSpawns` already treats an
// exhausted repository cap.
//
// "Cannot be admitted" is every veto `appendMacSpawns` will apply, read against
// the same inputs it reads — profile `maxActive`, the aged-or-throttled
// envelope for that demand, and the repository cap over live instances plus this
// tick's planned spawns. Naming the target on `maxActive` alone was the third
// instance of this seam's one bug: a `builder` (6 CPU / 12288 MB) with no
// sibling live has `maxActive` room in a four-core residual it cannot fit, so it
// won the target, was refused on the envelope, and the pass returned having
// never offered the queued `maestro` that fit those four cores exactly. That
// stalled the production Mac mini on 2026-08-09 at ~18:10Z with a quarter of the
// machine idle behind a held reservation every other condition had released.
//
// Skipping such a candidate does not invert aged FIFO, because a demand the
// residual cannot hold is not waiting on this pass at all: it is waiting on the
// live instances holding what it needs, which is ADR 0017's rule and the same
// reason a reserved head that does not fit lends its vector.
func remainderMacProfile(in Input, macos []domain.Demand) (domain.ProfileID, bool) {
	if !in.Config.MixedProfileCohorts {
		if active, live := activeMacProfile(in.Instances.Value); live {
			return active, macProfileCanGrow(in, active)
		}
	}
	free, agedFree := linuxFree(in), linuxFreeAged(in)
	counts := activeRepoCounts(in.Instances.Value)
	for _, demand := range priorityOrder(in, macos) {
		if !macProfileCanGrow(in, demand.Profile) {
			continue
		}
		if !demandEnvelope(in, demand, free, agedFree).CanFit(in.Config.Profiles[demand.Profile].Resources) {
			continue
		}
		if counts[demand.Key.Repo] >= repoCapLimit(in.Config.RepoCaps, demand.Key.Repo) {
			continue
		}
		return demand.Profile, true
	}
	return "", false
}

// demandEnvelope names the envelope a macOS demand is judged against. A demand
// past the fairness age is judged against the starvation-guard envelope, which
// lifts only the advisory CPU-idle clamp; young demands keep the throttled one.
// Both consume from both, so a young spawn this tick cannot double-spend
// capacity an aged spawn already took.
func demandEnvelope(in Input, demand domain.Demand, free, agedFree domain.Resources) domain.Resources {
	if demandAged(in.Now, in.Config.FairnessAge, demand) {
		return agedFree
	}
	return free
}

// appendPlannedSpawns models this tick's planned spawn operations as live,
// resource-consuming instances so a complementary admission pass sees the same
// reduced envelope the executor will. Planned+running matches the initial state
// the reconcile controller commits, and is exactly how ConsumesHostResources,
// activeRepoCounts, and the macOS MaxActive accounting read occupancy.
func appendPlannedSpawns(instances []domain.Instance, config Config, operations []Operation) []domain.Instance {
	result := append([]domain.Instance(nil), instances...)
	for _, operation := range operations {
		if operation.Kind != OperationSpawn {
			continue
		}
		profile := config.Profiles[operation.Profile]
		result = append(result, domain.Instance{
			ID: "planned-" + operation.ID, Repo: operation.Demand.Repo, Demand: operation.Demand,
			Platform: profile.Platform, Profile: profile.ID, Route: profile.Route, Resources: profile.Resources,
			State: domain.InstancePlanned, Power: domain.InstancePowerRunning,
		})
	}
	return result
}

func containsDrain(operations []Operation) bool {
	for _, operation := range operations {
		if operation.Kind == OperationDrain {
			return true
		}
	}
	return false
}

func demandsExcludingProfile(demands []domain.Demand, profile domain.ProfileID) []domain.Demand {
	var result []domain.Demand
	for _, demand := range demands {
		if demand.Profile != profile {
			result = append(result, demand)
		}
	}
	return result
}

func planMacWithCoexistence(in Input, plan Plan, linuxDemands, macDemands []domain.Demand) Plan {
	attempted := planMacOS(in, plan, macDemands)
	if containsSpawn(attempted.Operations) {
		attempted.Next.MacHandoff = nil
		return attempted
	}
	// An existing macOS profile switch has to complete before Linux capacity
	// can be considered for the target profile.
	if len(attempted.Operations) > len(plan.Operations) || !macProfileCanGrow(in, chosenMacProfile(in, macDemands)) {
		return attempted
	}
	return planMacHandoff(in, attempted, linuxDemands, macDemands)
}

func macProfileCanGrow(in Input, target domain.ProfileID) bool {
	profile := in.Config.Profiles[target]
	active := 0
	for _, instance := range in.Instances.Value {
		if !instance.ConsumesHostResources() || instance.Platform != domain.PlatformMacOS {
			continue
		}
		if instance.Profile != target {
			// A live foreign profile vetoes growth only under the single-cohort
			// rule. With mixed cohorts the veto is the envelope, not identity.
			if !in.Config.MixedProfileCohorts {
				return false
			}
			continue
		}
		active++
	}
	return active < macProfileLimit(profile)
}

func macHandoffFor(prior *MacHandoff, demand domain.Demand, now time.Time) MacHandoff {
	if prior != nil && prior.Demand == demand.Key && prior.Profile == demand.Profile {
		return *prior
	}
	return MacHandoff{Demand: demand.Key, Profile: demand.Profile, Since: now}
}

func retainedMacHandoff(prior *MacHandoff, demands []domain.Demand) *MacHandoff {
	if prior == nil {
		return nil
	}
	for _, demand := range demands {
		if demand.Key == prior.Demand && demand.Profile == prior.Profile {
			copy := *prior
			return &copy
		}
	}
	return nil
}

func stableMacHandoffOrder(prior *MacHandoff, demands []domain.Demand) []domain.Demand {
	if prior == nil {
		return demands
	}
	for index, demand := range demands {
		if demand.Key != prior.Demand || demand.Profile != prior.Profile || index == 0 {
			continue
		}
		ordered := append([]domain.Demand(nil), demand)
		ordered = append(ordered, demands[:index]...)
		ordered = append(ordered, demands[index+1:]...)
		return ordered
	}
	return demands
}

// boundedDrainBackfill uses residual Linux capacity while an already-running
// Linux job prevents a selected macOS handoff. Only one aged job from the
// smallest configured Linux tier may start, so fresh or heavyweight work
// cannot form an unbounded stream ahead of macOS. Mac spawning remains
// impossible until a later tick observes zero live Linux instances.
func boundedDrainBackfill(in Input, demands []domain.Demand) []Operation {
	ceiling, ok := smallestLinuxResources(in.Config.Profiles)
	if !ok {
		return nil
	}
	eligible := make([]domain.Demand, 0, len(demands))
	for _, demand := range demands {
		profile := in.Config.Profiles[demand.Profile]
		aged := in.Config.FairnessAge > 0 && !demand.CreatedAt.IsZero() && in.Now.Sub(demand.CreatedAt) >= in.Config.FairnessAge
		controlPlane := in.Config.RepoSchedulingClasses[demand.Key.Repo] == domain.SchedulingControlPlane
		if (aged || controlPlane) && profile.Platform == domain.PlatformLinux && profile.Resources == ceiling {
			eligible = append(eligible, demand)
		}
	}
	if len(eligible) == 0 {
		return nil
	}
	free := linuxFree(in)
	free.Slots = min(free.Slots, 1)
	selected := exactSelect(in.Now, priorityOrder(in, eligible), free, activeRepoCounts(in.Instances.Value), in.Config)
	if len(selected) == 0 {
		return nil
	}
	return []Operation{spawnOperation(selected[0], nil)}
}

func smallestLinuxResources(profiles map[domain.ProfileID]domain.Profile) (domain.Resources, bool) {
	var smallest domain.Profile
	found := false
	for _, profile := range profiles {
		if profile.Platform != domain.PlatformLinux || (found && !profileLess(profile, smallest)) {
			continue
		}
		smallest = profile
		found = true
	}
	return smallest.Resources, found
}

func profileLess(a, b domain.Profile) bool {
	if a.Resources.CPU != b.Resources.CPU {
		return a.Resources.CPU < b.Resources.CPU
	}
	if a.Resources.MemoryMB != b.Resources.MemoryMB {
		return a.Resources.MemoryMB < b.Resources.MemoryMB
	}
	if a.Resources.Slots != b.Resources.Slots {
		return a.Resources.Slots < b.Resources.Slots
	}
	return a.ID < b.ID
}

func planLinuxHandoff(in Input, plan Plan, demands, macDemands []domain.Demand) Plan {
	handoff := linuxHandoffFor(in.Prior.LinuxHandoff, priorityOrder(in, demands)[0], in.Now)
	plan.Next.LinuxHandoff = &handoff
	var drains []Operation
	allIdle := true
	filtered := append([]domain.Instance(nil), in.Instances.Value...)
	for _, instance := range sortedInstances(in.Instances.Value) {
		if instance.Platform != domain.PlatformMacOS || !instance.ConsumesHostResources() {
			continue
		}
		if instance.State != domain.InstanceOnlineIdle {
			allIdle = false
			continue
		}
		drains = append(drains, drainOperation(instance))
		filtered = removeInstance(filtered, instance.ID)
	}
	plan.Operations = append(plan.Operations, drains...)
	if !allIdle {
		if !handoff.BackfillAdmitted {
			if profile, ok := activeMacProfile(in.Instances.Value); ok {
				// planLinux ran first and may have reserved a vector for the aged
				// Linux head this wave is meant to unblock. A bounded wave is still
				// a second admission pass, so ADR 0029 binds it too: withhold the
				// reserved head's vector, and bid for its repository only up to the
				// cap slack that leaves the head's own slot free.
				lending := chargeReservedHead(in, plan.Next.Reservation)
				candidates := reservedRemainderDemands(in, plan, demandsForProfile(macDemands, profile))
				before := len(plan.Operations)
				plan = appendMacSpawns(lending, plan, candidates, nil)
				if containsSpawn(plan.Operations[before:]) {
					handoff.BackfillAdmitted = true
					plan.Next.LinuxHandoff = &handoff
				}
			}
		}
		return plan
	}
	plan.Next.LinuxHandoff = nil
	nextInput := in
	nextInput.Instances = domain.Fresh(filtered, in.Instances.ObservedAt)
	next := planLinux(nextInput, Plan{Status: plan.Status, Next: plan.Next}, demands)
	dependencies := operationIDs(drains)
	for _, operation := range next.Operations {
		operation.DependsOn = append([]string(nil), dependencies...)
		operation.ID = stableID("op", operation)
		plan.Operations = append(plan.Operations, operation)
	}
	plan.Next = next.Next
	return plan
}

func planLinuxWithCoexistence(in Input, plan Plan, demands, macDemands []domain.Demand) Plan {
	remaining := consumeCompatibleIdle(demands, in.Instances.Value)
	attempted := planLinux(in, plan, demands)
	if len(remaining) == 0 || containsSpawn(attempted.Operations) {
		attempted.Next.LinuxHandoff = nil
		return attempted
	}
	return planLinuxHandoff(in, attempted, demands, macDemands)
}

func linuxHandoffFor(prior *LinuxHandoff, demand domain.Demand, now time.Time) LinuxHandoff {
	if prior != nil && prior.Demand == demand.Key {
		return *prior
	}
	return LinuxHandoff{Demand: demand.Key, Since: now}
}

func activeMacProfile(instances []domain.Instance) (domain.ProfileID, bool) {
	var selected domain.ProfileID
	for _, instance := range instances {
		if !instance.ConsumesHostResources() || instance.Platform != domain.PlatformMacOS {
			continue
		}
		if selected != "" && selected != instance.Profile {
			return "", false
		}
		selected = instance.Profile
	}
	return selected, selected != ""
}

func containsSpawn(operations []Operation) bool {
	for _, operation := range operations {
		if operation.Kind == OperationSpawn {
			return true
		}
	}
	return false
}

func removeInstance(instances []domain.Instance, id string) []domain.Instance {
	result := make([]domain.Instance, 0, len(instances))
	for _, instance := range instances {
		if instance.ID != id {
			result = append(result, instance)
		}
	}
	return result
}

func planMacOS(in Input, plan Plan, demands []domain.Demand) Plan {
	profile := chosenMacProfile(in, demands)
	// Coexistence first: when profiles may mix and the chosen profile fits
	// beside the live foreign cohort, spawn without draining anything. The
	// envelope inside appendMacSpawns already charges every live instance
	// against the physical bound, so a spawn here can never overcommit. Only
	// when nothing fits does the drain-and-switch fallback below run, exactly
	// as it always has -- and it still never touches a busy instance.
	if in.Config.MixedProfileCohorts {
		attempted := appendMacSpawns(in, plan, demandsForProfile(demands, profile), nil)
		if containsSpawn(attempted.Operations[len(plan.Operations):]) {
			return attempted
		}
	}
	var drains []Operation
	allSwitchable := true
	filtered := append([]domain.Instance(nil), in.Instances.Value...)
	for _, instance := range sortedInstances(in.Instances.Value) {
		if instance.Platform != domain.PlatformMacOS || !instance.ConsumesHostResources() || instance.Profile == profile {
			continue
		}
		if instance.State != domain.InstanceOnlineIdle {
			allSwitchable = false
			continue
		}
		drains = append(drains, drainOperation(instance))
		filtered = removeInstance(filtered, instance.ID)
	}
	plan.Operations = append(plan.Operations, drains...)
	if allSwitchable {
		nextInput := in
		nextInput.Instances = domain.Fresh(filtered, in.Instances.ObservedAt)
		plan = appendMacSpawns(nextInput, plan, demandsForProfile(demands, profile), operationIDs(drains))
	}
	return plan
}

func chosenMacProfile(in Input, demands []domain.Demand) domain.ProfileID {
	return priorityOrder(in, demands)[0].Profile
}

func demandsForProfile(demands []domain.Demand, profile domain.ProfileID) []domain.Demand {
	var result []domain.Demand
	for _, demand := range demands {
		if demand.Profile == profile {
			result = append(result, demand)
		}
	}
	return result
}

func appendMacSpawns(in Input, plan Plan, demands []domain.Demand, dependencies []string) Plan {
	if len(demands) == 0 {
		return plan
	}
	profile := in.Config.Profiles[demands[0].Profile]
	limit := macProfileLimit(profile)
	active := 0
	free := linuxFree(in)
	for _, instance := range in.Instances.Value {
		if instance.ConsumesHostResources() && instance.Platform == domain.PlatformMacOS && instance.Profile == profile.ID {
			active++
		}
	}
	available := limit - active
	if available <= 0 {
		return plan
	}
	agedFree := linuxFreeAged(in)
	// The repository cap is a platform-wide bound (ADR 0012), and ADR 0030's
	// reserved-slack arithmetic depends on it being one: activeRepoCounts charges
	// a macOS instance to its repository exactly like a Linux one, so a cap this
	// pass ignored would be a cap the Linux head could not rely on. Occupancy is
	// the same one every other pass reads -- live instances plus whatever this
	// plan already admits.
	counts := activeRepoCounts(appendPlannedSpawns(in.Instances.Value, in.Config, plan.Operations))
	ordered := priorityOrder(in, demands)
	selected := make([]domain.Demand, 0, available)
	for _, demand := range ordered {
		if len(selected) >= available {
			break
		}
		if !demandEnvelope(in, demand, free, agedFree).CanFit(profile.Resources) {
			break
		}
		// A capped repository is skipped, never a stop: every candidate here shares
		// one profile vector, so an exhausted envelope ends the pass but an
		// exhausted cap only ends that repository's turn. The next repository's
		// work takes the vector, which is what exactSelect already does for Linux.
		if counts[demand.Key.Repo] >= repoCapLimit(in.Config.RepoCaps, demand.Key.Repo) {
			continue
		}
		counts[demand.Key.Repo]++
		agedFree, _ = agedFree.Sub(profile.Resources)
		if remaining, ok := free.Sub(profile.Resources); ok {
			free = remaining
		} else {
			free = domain.Resources{}
		}
		selected = append(selected, demand)
	}
	for _, demand := range selected {
		plan.Operations = append(plan.Operations, spawnOperation(demand, dependencies))
	}
	if len(selected) > 0 {
		plan.Next.DRRCursor = selected[len(selected)-1].Key.Repo
	}
	return plan
}

func macProfileLimit(profile domain.Profile) int {
	limit := profile.MaxActive
	if limit <= 0 {
		limit = 1
	}
	return limit
}

func sortedInstances(instances []domain.Instance) []domain.Instance {
	result := append([]domain.Instance(nil), instances...)
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func spawnOperation(demand domain.Demand, dependencies []string) Operation {
	operation := Operation{Kind: OperationSpawn, Demand: demand.Key, Profile: demand.Profile, Route: demand.Route, DependsOn: append([]string(nil), dependencies...)}
	operation.ID = stableID("op", operation)
	return operation
}

func drainOperation(instance domain.Instance) Operation {
	operation := Operation{Kind: OperationDrain, Instance: instance.ID, Profile: instance.Profile, Route: instance.Route}
	operation.ID = stableID("op", operation)
	return operation
}

func operationIDs(operations []Operation) []string {
	result := make([]string, 0, len(operations))
	for _, operation := range operations {
		result = append(result, operation.ID)
	}
	return result
}

func finish(plan Plan) Plan {
	if len(plan.Operations) == 0 {
		plan.Operations = nil
	}
	plan.ID = stableID("plan", struct {
		Status     PlanStatus
		Operations []Operation
		Next       State
	}{plan.Status, plan.Operations, plan.Next})
	return plan
}

func stableID(prefix string, value any) string {
	encoded, _ := json.Marshal(value)
	sum := sha256.Sum256(encoded)
	return prefix + "-" + hex.EncodeToString(sum[:12])
}
