// Package scheduler implements the deterministic, side-effect-free fleet
// planner. Adapters provide immutable observations and execute its operations.
package scheduler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

type Config struct {
	LinuxCapacity         domain.Resources
	FairnessAge           time.Duration
	RepoCaps              map[string]int
	RepoSchedulingClasses map[string]domain.SchedulingClass
	Profiles              map[domain.ProfileID]domain.Profile
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
	ID        string
	Kind      OperationKind
	Demand    domain.DemandKey
	Instance  string
	Profile   domain.ProfileID
	Route     domain.Route
	DependsOn []string
	Recovery  bool `json:"recovery,omitempty"`
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
}

// PlanTick computes a complete plan without performing side effects.
func PlanTick(in Input) Plan {
	if !in.Demands.Usable() || !in.Instances.Usable() || !in.Host.Usable() {
		return finish(Plan{Status: PlanBlockedObservation, Next: in.Prior})
	}
	mode, err := domain.DeriveHostMode(in.Instances.Value)
	if err != nil {
		return finish(Plan{Status: PlanInvalidObservation, Next: in.Prior})
	}

	demands := normalizedDemands(in)
	plan := Plan{Status: PlanReady, Next: in.Prior}
	if recoveries := stoppedAssignmentRecoveries(in.Instances.Value); len(recoveries) > 0 {
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
	switch ordered[0].Platform {
	case domain.PlatformMacOS:
		plan.Next.LinuxHandoff = nil
		if mode == domain.HostLinux {
			plan = planMacHandoff(in, plan, linux, macos)
		} else {
			plan.Next.MacHandoff = nil
			plan = planMacOS(in, plan, macos)
		}
	case domain.PlatformLinux:
		plan.Next.MacHandoff = retainedMacHandoff(in.Prior.MacHandoff, macos)
		if mode == domain.HostMacOS {
			plan = planLinuxHandoff(in, plan, linux, macos)
		} else {
			plan.Next.LinuxHandoff = nil
			plan = planLinux(in, plan, linux)
		}
	}
	return finish(plan)
}

func stoppedAssignmentRecoveries(instances []domain.Instance) []Operation {
	var recoveries []Operation
	for _, instance := range sortedInstances(instances) {
		if instance.Power != domain.InstancePowerStopped ||
			(instance.State != domain.InstanceAssigned && instance.State != domain.InstanceRunning) {
			continue
		}
		operation := Operation{Kind: OperationDrain, Instance: instance.ID, Profile: instance.Profile, Route: instance.Route, Recovery: true}
		operation.ID = stableID("op", operation)
		recoveries = append(recoveries, operation)
	}
	return recoveries
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

	ordered := youngPriorityOrder(young, in.Prior.DRRCursor, in.Config.RepoSchedulingClasses)
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
	ordered := append(aged, youngPriorityOrder(young, in.Prior.DRRCursor, in.Config.RepoSchedulingClasses)...)
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
	return append(aged, youngPriorityOrder(young, in.Prior.DRRCursor, in.Config.RepoSchedulingClasses)...)
}

// youngPriorityOrder implements two bounded lanes. Control-plane work can
// bypass only young standard work; aged global FIFO is assembled by
// priorityOrder before either lane and therefore remains absolute.
func youngPriorityOrder(demands []domain.Demand, cursor string, classes map[string]domain.SchedulingClass) []domain.Demand {
	controlPlane := make([]domain.Demand, 0, len(demands))
	standard := make([]domain.Demand, 0, len(demands))
	for _, demand := range demands {
		if classes[demand.Key.Repo] == domain.SchedulingControlPlane {
			controlPlane = append(controlPlane, demand)
		} else {
			standard = append(standard, demand)
		}
	}
	return append(fairOrder(controlPlane, cursor), fairOrder(standard, cursor)...)
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
	var best []int
	var search func(index int, remaining domain.Resources, counts map[string]int, chosen []int)
	search = func(index int, remaining domain.Resources, counts map[string]int, chosen []int) {
		if betterSelection(chosen, best) {
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
	for _, instance := range sortedInstances(in.Instances.Value) {
		if instance.Platform != domain.PlatformLinux || !instance.ConsumesHostResources() {
			continue
		}
		if instance.State != domain.InstanceOnlineIdle {
			allIdle = false
			continue
		}
		drains = append(drains, drainOperation(instance))
	}
	plan.Operations = append(plan.Operations, drains...)
	if allIdle {
		plan.Next.MacHandoff = nil
		plan = appendMacSpawns(in, plan, macDemands, operationIDs(drains))
		return plan
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

func linuxHandoffFor(prior *LinuxHandoff, demand domain.Demand, now time.Time) LinuxHandoff {
	if prior != nil && prior.Demand == demand.Key {
		return *prior
	}
	return LinuxHandoff{Demand: demand.Key, Since: now}
}

func activeMacProfile(instances []domain.Instance) (domain.ProfileID, bool) {
	var selected domain.ProfileID
	for _, instance := range instances {
		if !instance.Live() || instance.Platform != domain.PlatformMacOS {
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
	for _, instance := range sortedInstances(in.Instances.Value) {
		if instance.Platform != domain.PlatformMacOS || !instance.ConsumesHostResources() || instance.Profile == profile {
			continue
		}
		if instance.State != domain.InstanceOnlineIdle {
			allSwitchable = false
			continue
		}
		drains = append(drains, drainOperation(instance))
	}
	plan.Operations = append(plan.Operations, drains...)
	if allSwitchable {
		plan = appendMacSpawns(in, plan, demandsForProfile(demands, profile), operationIDs(drains))
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
	limit := profile.MaxActive
	if profile.ID == "maestro" && (limit <= 0 || limit > 2) {
		limit = 2
	}
	if limit <= 0 {
		limit = 1
	}
	active := 0
	free := in.Host.Value.Available
	for _, instance := range in.Instances.Value {
		if instance.ConsumesHostResources() && instance.Platform == domain.PlatformMacOS && instance.Profile == profile.ID {
			active++
			var ok bool
			free, ok = free.Sub(instance.Resources)
			if !ok {
				return plan
			}
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
