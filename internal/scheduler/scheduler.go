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
	if recoveries := assignmentRecoveries(in.Now, in.Config.AssignedTimeout, in.Instances.Value); len(recoveries) > 0 {
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
		} else {
			plan.Next.MacHandoff = nil
			attempted := planMacOS(in, plan, macos)
			switch {
			case containsSpawn(attempted.Operations):
				plan = attempted
			case mode == domain.HostIdle:
				// On a fully idle host a macOS head that will not spawn is resource-
				// infeasible: nothing is live to drain and make room for it, so it is
				// NOT waiting on drainable work. The bounded one-shot handoff latch is
				// wrong here — it drains the queue by a single job and re-wedges.
				// Admit feasible work behind it in the residual envelope every tick.
				plan = planBehindInfeasibleMacHead(in, plan, linux, macos)
			case len(linux) == 0:
				plan = attempted
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

func assignmentRecoveries(now time.Time, assignedTimeout time.Duration, instances []domain.Instance) []Operation {
	var recoveries []Operation
	for _, instance := range sortedInstances(instances) {
		if instance.State != domain.InstanceAssigned && instance.State != domain.InstanceRunning {
			continue
		}
		confirmedInactive := instance.Power == domain.InstancePowerRunning && instance.RecoveryReady
		stalled := stalledAssignment(now, assignedTimeout, instance)
		if instance.Power != domain.InstancePowerStopped && !confirmedInactive && !stalled {
			continue
		}
		operation := Operation{Kind: OperationDrain, Instance: instance.ID, Profile: instance.Profile, Route: instance.Route,
			Recovery: true, ConfirmedInactive: confirmedInactive, StalledAssignment: stalled}
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
	baseCounts := activeRepoCounts(in.Instances.Value)

	if reserved, ok := reservedDemand(in.Prior.Reservation, demands); ok {
		profile := in.Config.Profiles[reserved.Profile]
		if feasible(profile.Resources, free, reserved.Key.Repo, baseCounts, nil, in.Config.RepoCaps) {
			plan.Operations = append(plan.Operations, spawnOperation(reserved, nil))
			plan.Next.Reservation = nil
		} else {
			plan.Next.Reservation = copyReservation(in.Prior.Reservation)
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
			if !feasible(profile.Resources, free, candidate.Key.Repo, baseCounts, selected, in.Config.RepoCaps) {
				plan.Next.Reservation = &domain.Reservation{Demand: candidate.Key, Profile: candidate.Profile, Resources: profile.Resources, Since: in.Now}
				backfill := safeBackfill(in, demands, free, baseCounts, plan.Next.Reservation, selected)
				plan.Operations = append(plan.Operations, backfill...)
				if len(backfill) > 0 {
					plan.Next.DRRCursor = backfill[len(backfill)-1].Demand.Repo
				}
				break
			}
			plan.Operations = append(plan.Operations, spawnOperation(candidate, nil))
			free, _ = free.Sub(profile.Resources)
		}
		return plan
	}

	ordered := youngPriorityOrder(young, in.Prior.DRRCursor, in.Config)
	selected := exactSelect(ordered, free, baseCounts, in.Config)
	for _, candidate := range selected {
		plan.Operations = append(plan.Operations, spawnOperation(candidate, nil))
	}
	if len(selected) > 0 {
		plan.Next.DRRCursor = selected[len(selected)-1].Key.Repo
	}
	return plan
}

func linuxFree(in Input) domain.Resources {
	free := in.Config.LinuxCapacity
	for _, instance := range in.Instances.Value {
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
func safeBackfill(in Input, demands []domain.Demand, free domain.Resources, baseCounts map[string]int, reservation *domain.Reservation, alreadySelected []domain.Demand) []Operation {
	backfillCapacity, ok := free.Sub(reservation.Resources)
	if !ok {
		return nil
	}
	excluded := map[domain.DemandKey]bool{reservation.Demand: true}
	for _, demand := range alreadySelected {
		excluded[demand.Key] = true
	}
	var candidates []domain.Demand
	for _, demand := range demands {
		if !excluded[demand.Key] {
			candidates = append(candidates, demand)
		}
	}
	aged, young := splitAged(in.Now, in.Config.FairnessAge, candidates)
	ordered := append(aged, youngPriorityOrder(young, in.Prior.DRRCursor, in.Config)...)
	counts := cloneCounts(baseCounts)
	for _, demand := range alreadySelected {
		counts[demand.Key.Repo]++
	}
	selected := exactSelect(ordered, backfillCapacity, counts, in.Config)
	operations := make([]Operation, 0, len(selected))
	for _, demand := range selected {
		operations = append(operations, spawnOperation(demand, nil))
	}
	return operations
}

func splitAged(now time.Time, age time.Duration, demands []domain.Demand) (aged, young []domain.Demand) {
	for _, demand := range demands {
		if age > 0 && !demand.CreatedAt.IsZero() && now.Sub(demand.CreatedAt) >= age {
			aged = append(aged, demand)
		} else {
			young = append(young, demand)
		}
	}
	return aged, young
}

func priorityOrder(in Input, demands []domain.Demand) []domain.Demand {
	aged, young := splitAged(in.Now, in.Config.FairnessAge, demands)
	return append(aged, youngPriorityOrder(young, in.Prior.DRRCursor, in.Config)...)
}

// youngPriorityOrder implements two bounded lanes. Control-plane work can
// bypass only young standard work; aged global FIFO is assembled by
// priorityOrder before either lane and therefore remains absolute.
func youngPriorityOrder(demands []domain.Demand, cursor string, config Config) []domain.Demand {
	controlPlane := make([]domain.Demand, 0, len(demands))
	standard := make([]domain.Demand, 0, len(demands))
	for _, demand := range demands {
		if config.RepoSchedulingClasses[demand.Key.Repo] == domain.SchedulingControlPlane {
			controlPlane = append(controlPlane, demand)
		} else {
			standard = append(standard, demand)
		}
	}
	return append(throughputOrder(controlPlane, cursor, config), throughputOrder(standard, cursor, config)...)
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
func exactSelect(candidates []domain.Demand, free domain.Resources, baseCounts map[string]int, config Config) []domain.Demand {
	// Priority band per candidate mirrors youngPriorityOrder's lanes: lower band
	// is higher priority. Count maximization is a throughput optimization that
	// must apply only WITHIN a band; it must never admit a larger count of
	// lower-priority work while a feasible higher-priority demand is deferred.
	bands := make([]int, len(candidates))
	numBands := 0
	for i := range candidates {
		bands[i] = schedulingBand(candidates[i], config)
		if bands[i]+1 > numBands {
			numBands = bands[i] + 1
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
			cap := config.RepoCaps[candidate.Key.Repo]
			if cap <= 0 {
				cap = 1
			}
			if counts[candidate.Key.Repo] >= cap || !remaining.CanFit(profile.Resources) {
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

// schedulingBand assigns a demand its priority band for exact admission. It
// mirrors youngPriorityOrder: control-plane work occupies the highest-priority
// band, standard work the next. Aging remains the absolute starvation guard and
// is handled by the reservation head outside exactSelect, so it is not a band
// here.
func schedulingBand(demand domain.Demand, config Config) int {
	if config.RepoSchedulingClasses[demand.Key.Repo] == domain.SchedulingControlPlane {
		return 0
	}
	return 1
}

// betterAdmission ranks two feasible index selections while respecting priority
// bands. It first compares their per-band coverage vectors lexicographically
// (band 0 highest priority): more admitted demands in a higher-priority band
// beats any count of lower-priority bands, so exact admission never inverts
// priority to maximize raw count. When the coverage vectors are identical —
// always the case when every candidate shares one band — total count is equal,
// so it defers to betterSelection's count/index-lexicographic tie-break,
// preserving the original throughput behavior and determinism.
func betterAdmission(candidate, incumbent, bands []int, numBands int) bool {
	candidateCover := bandCoverage(candidate, bands, numBands)
	incumbentCover := bandCoverage(incumbent, bands, numBands)
	for b := 0; b < numBands; b++ {
		if candidateCover[b] != incumbentCover[b] {
			return candidateCover[b] > incumbentCover[b]
		}
	}
	return betterSelection(candidate, incumbent)
}

// bandCoverage counts, per priority band, how many chosen candidates fall in
// that band. bands maps a candidate index to its band.
func bandCoverage(chosen, bands []int, numBands int) []int {
	coverage := make([]int, numBands)
	for _, index := range chosen {
		coverage[bands[index]]++
	}
	return coverage
}

func betterSelection(candidate, incumbent []int) bool {
	if len(candidate) != len(incumbent) {
		return len(candidate) > len(incumbent)
	}
	for i := range candidate {
		if candidate[i] != incumbent[i] {
			return candidate[i] < incumbent[i]
		}
	}
	return false
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
	cap := caps[repo]
	if cap <= 0 {
		cap = 1
	}
	return count < cap
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
			return false
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
	selected := exactSelect(priorityOrder(in, eligible), free, activeRepoCounts(in.Instances.Value), in.Config)
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
				before := len(plan.Operations)
				plan = appendMacSpawns(in, plan, demandsForProfile(macDemands, profile), nil)
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
	ordered := priorityOrder(in, demands)
	selected := make([]domain.Demand, 0, available)
	for _, demand := range ordered {
		if len(selected) >= available || !free.CanFit(profile.Resources) {
			break
		}
		free, _ = free.Sub(profile.Resources)
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
