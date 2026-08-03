package simulation_test

import (
	"fmt"
	"sort"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/lifecycle"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

// findingKind names one property. Each value is an invariant the fleet is
// supposed to hold on every tick; a finding is the simulator's counterexample.
type findingKind string

const (
	// findingWedge is property (a), liveness: a tick with a feasible demand must
	// lead to an admission within K ticks.
	findingWedge findingKind = "liveness_wedge"
	// findingStarvation is property (b), bounded starvation: an aged feasible
	// demand may not be passed over by younger work more than N times.
	findingStarvation findingKind = "bounded_starvation"
	// findingPlanRefused is property (c): a ready plan always applies.
	findingPlanRefused findingKind = "plan_not_applied"
	// findingIdentity is property (d): identities are unique across everything in
	// flight.
	findingIdentity findingKind = "identity_collision"
	// findingDoubleAdmit is property (e): one demand is admitted once.
	findingDoubleAdmit findingKind = "double_admission"
	// findingNoQuiescence is property (f): the fleet empties when demand stops.
	findingNoQuiescence findingKind = "no_quiescence"
	// findingConservation is property (g): instances never exceed the envelope.
	findingConservation findingKind = "conservation"
	// findingStoreError is the harness's own fail-closed channel: the durable
	// store refused something the simulation had no right to be refused.
	findingStoreError findingKind = "store_error"
)

type finding struct {
	Kind findingKind
	// Signature names a specific, already-documented defect inside a property.
	// It is what lets a known hole be tolerated by the sweep without disabling
	// the whole property that found it.
	Signature string
	Tick      int
	Detail    string
}

func (f finding) String() string {
	if f.Signature != "" {
		return fmt.Sprintf("tick %d: %s (%s): %s", f.Tick, f.Kind, f.Signature, f.Detail)
	}
	return fmt.Sprintf("tick %d: %s: %s", f.Tick, f.Kind, f.Detail)
}

// sigRespawnLiveIncarnation is FINDING 1 of ADR 0031: between a spawn and its
// runner's JIT acquisition the demand is still JobAvailable to GitHub, nothing
// in the pipeline filters a demand a live instance already incarnates, and every
// tick in that window re-derives the identical content-addressed spawn.
const sigRespawnLiveIncarnation = "respawn_of_a_live_incarnation"

// sigMacOSIgnoresRepositoryCap is FINDING 2 of ADR 0031: scheduler
// .appendMacSpawns bounds macOS admission by the envelope and by the profile's
// MaxActive, and by nothing else. It never reads Config.RepoCaps, although
// activeRepoCounts charges a macOS instance to its repository exactly like a
// Linux one -- so a repository's cap bounds its Linux work and not its macOS
// work.
const sigMacOSIgnoresRepositoryCap = "macos_admission_ignores_repository_cap"

// sigControlPlaneOvertakesAgedWork is FINDING 3 of ADR 0031. ADR 0004 states the
// lane rule as "control-plane work can bypass only YOUNG standard work; aged
// global FIFO ... remains absolute", and priorityOrder honours it. exactSelect
// then re-ranks the very same candidates by scheduling class alone
// (betterAdmission's band coverage), and aging is not a band there, so a young
// control-plane demand outranks an aged standard one after all.
const sigControlPlaneOvertakesAgedWork = "control_plane_overtakes_aged_standard_work"

// sigCrossPlatformResidualArbitration is FINDING 4 of ADR 0031 and is the hole
// ADR 0030 names in its own "Not addressed here": the residual is arbitrated one
// platform at a time -- planLinux runs, then fillMacRemainder -- so a younger
// Linux demand can take a vector an older macOS demand was entitled to under the
// cross-platform aged FIFO of ADR 0012.
const sigCrossPlatformResidualArbitration = "cross_platform_residual_arbitration"

// sigCountMaximizationOvertakesAgedWork is FINDING 5 of ADR 0031. exactSelect
// searches for the admission with the largest COUNT inside a scheduling band,
// and aging is not a band. throughputOrder's own comment promises the
// shortest-resource-first optimization applies only to young work because
// "aging is applied before this function", but the aged candidates are handed to
// exactSelect in the same slice, so a pair of younger small demands still
// outranks one aged large one.
const sigCountMaximizationOvertakesAgedWork = "count_maximization_overtakes_aged_work"

// tickObservation is the simulator's complete observable state for one tick. It
// is the only thing property oracles read, so a property is a pure function of
// observations rather than of harness internals.
type tickObservation struct {
	Tick            int
	Now             time.Time
	Plan            scheduler.Plan
	Applied         bool
	Err             error
	Demands         []domain.Demand
	Instances       []domain.Instance
	InstancesUsable bool
	Host            domain.Host
	HostUsable      bool
}

func (o tickObservation) spawns() []domain.DemandKey {
	var keys []domain.DemandKey
	for _, operation := range o.Plan.Operations {
		if operation.Kind == scheduler.OperationSpawn {
			keys = append(keys, operation.Demand)
		}
	}
	return keys
}

func (o tickObservation) hasDrain() bool {
	for _, operation := range o.Plan.Operations {
		if operation.Kind == scheduler.OperationDrain {
			return true
		}
	}
	return false
}

// instanceState names the durable state of one observed instance, so a finding
// says WHICH stage of provisioning the collision happened in.
func instanceState(observation tickObservation, id string) domain.InstanceState {
	for _, instance := range observation.Instances {
		if instance.ID == id {
			return instance.State
		}
	}
	return ""
}

type checker func(*world, tickObservation) []finding

// defaultCheckers is the property set of ADR 0031, evaluated in a fixed order so
// a failing run always reports the same first violation.
//
// Order is causal, not alphabetical: the oracles that name a SPECIFIC cause run
// before the one that reports its downstream symptom. A demand admitted twice is
// why the commit was refused, and reporting the refusal instead would send an
// operator to the database for a scheduling defect.
func defaultCheckers(cfg worldConfig) []checker {
	return []checker{
		noDoubleAdmissionChecker(),
		identityUniquenessChecker(),
		planAlwaysAppliesChecker(),
		conservationChecker(cfg),
		livenessChecker(cfg),
		boundedStarvationChecker(cfg),
		quiescenceChecker(cfg),
	}
}

// ---------------------------------------------------------------------------
// (c) A ready plan always applies.
// ---------------------------------------------------------------------------

// planAlwaysAppliesChecker fails on any commit refusal of a ready plan.
//
// The simulator is single-writer and strictly sequential: the inventory the plan
// was built from cannot move before the compare-and-set that persists it. An
// optimistic-concurrency loss is therefore not the routine, self-healing event
// app.ReasonPlanCommitContended describes -- there is nobody to contend with --
// but a genuine composition defect, which is exactly how incident 2026-08-02
// presented before ADR 0027.
func planAlwaysAppliesChecker() checker {
	return func(w *world, observation tickObservation) []finding {
		if observation.Plan.Status == scheduler.PlanInvalidObservation {
			return []finding{{Kind: findingPlanRefused, Tick: observation.Tick,
				Detail: "planner refused its own observation: " + observation.Plan.Reason}}
		}
		if observation.Plan.Status != scheduler.PlanReady {
			return nil
		}
		if observation.Err != nil {
			return []finding{{Kind: findingPlanRefused, Tick: observation.Tick,
				Detail: fmt.Sprintf("commit refused a ready plan: %v\n%s", observation.Err, w.dumpPlan(observation))}}
		}
		if len(observation.Plan.Operations) > 0 && !observation.Applied {
			return []finding{{Kind: findingPlanRefused, Tick: observation.Tick,
				Detail: "ready plan with operations was not applied\n" + w.dumpPlan(observation)}}
		}
		return nil
	}
}

// dumpPlan renders the plan and the inputs it was built from. A failing seed is
// only useful if the operator can read the decision that failed.
func (w *world) dumpPlan(observation tickObservation) string {
	out := fmt.Sprintf("  plan %s status=%s reason=%q applied=%t\n", observation.Plan.ID,
		observation.Plan.Status, observation.Plan.Reason, observation.Applied)
	for _, operation := range observation.Plan.Operations {
		out += fmt.Sprintf("    op %s kind=%s demand=%s instance=%s profile=%s depends=%v\n",
			operation.ID, operation.Kind, operation.Demand, operation.Instance, operation.Profile, operation.DependsOn)
	}
	out += fmt.Sprintf("  next reservation=%v mac=%v linux=%v drr=%q\n", observation.Plan.Next.Reservation,
		observation.Plan.Next.MacHandoff, observation.Plan.Next.LinuxHandoff, observation.Plan.Next.DRRCursor)
	for _, demand := range observation.Demands {
		out += fmt.Sprintf("    demand %s profile=%s age=%s\n", demand.Key, demand.Profile,
			observation.Now.Sub(demand.CreatedAt))
	}
	for _, instance := range observation.Instances {
		out += fmt.Sprintf("    instance %s repo=%s profile=%s state=%s power=%s demand=%s\n",
			instance.ID, instance.Repo, instance.Profile, instance.State, instance.Power, instance.Demand)
	}
	out += fmt.Sprintf("  host available=%+v capacity=%+v\n", observation.Host.Available, observation.Host.Capacity)
	return out
}

// ---------------------------------------------------------------------------
// (e) One demand is admitted once.
// ---------------------------------------------------------------------------

// noDoubleAdmissionChecker is the ADR 0027 oracle. It fails both on a plan that
// admits one demand twice and on a plan that admits a demand a live, non-terminal
// instance already incarnates -- the two ways a content-addressed spawn collides
// with itself in the durable layer.
func noDoubleAdmissionChecker() checker {
	return func(w *world, observation tickObservation) []finding {
		seen := map[domain.DemandKey]bool{}
		var findings []finding
		for _, key := range observation.spawns() {
			if seen[key] {
				findings = append(findings, finding{Kind: findingDoubleAdmit, Tick: observation.Tick,
					Detail: fmt.Sprintf("one tick admitted %s twice\n%s", key, w.dumpPlan(observation))})
			}
			seen[key] = true
		}
		live := map[domain.DemandKey]string{}
		for _, instance := range observation.Instances {
			if instance.IncarnatesDemand() {
				live[instance.Demand] = instance.ID
			}
		}
		for _, key := range observation.spawns() {
			if id, exists := live[key]; exists {
				findings = append(findings, finding{Kind: findingDoubleAdmit, Signature: sigRespawnLiveIncarnation,
					Tick: observation.Tick,
					Detail: fmt.Sprintf("admitted %s while instance %s (%s) still incarnates it\n%s",
						key, id, instanceState(observation, id), w.dumpPlan(observation))})
			}
		}
		return findings
	}
}

// ---------------------------------------------------------------------------
// (d) Identities are unique across everything in flight.
// ---------------------------------------------------------------------------

// identityUniquenessChecker guards the two identities the durable layer keys on:
// the content-addressed operation identity, and the one live incarnation of a
// demand. Both were the proximate cause of a wedge (ADR 0027, ADR 0028).
func identityUniquenessChecker() checker {
	return func(_ *world, observation tickObservation) []finding {
		var findings []finding
		seen := map[string]bool{}
		for _, operation := range observation.Plan.Operations {
			if seen[operation.ID] {
				findings = append(findings, finding{Kind: findingIdentity, Tick: observation.Tick,
					Detail: "one plan carries operation identity " + operation.ID + " twice"})
			}
			seen[operation.ID] = true
		}
		owners := map[domain.DemandKey]string{}
		for _, instance := range observation.Instances {
			if !instance.IncarnatesDemand() {
				continue
			}
			if previous, exists := owners[instance.Demand]; exists {
				findings = append(findings, finding{Kind: findingIdentity, Tick: observation.Tick,
					Detail: fmt.Sprintf("instances %s and %s both incarnate %s", previous, instance.ID, instance.Demand)})
			}
			owners[instance.Demand] = instance.ID
		}
		return findings
	}
}

// ---------------------------------------------------------------------------
// (g) Conservation.
// ---------------------------------------------------------------------------

// conservationChecker is the physical-safety oracle: whatever the planner
// believes, the machine must never be oversubscribed, the slot ceiling must
// hold, a repository must never exceed its cap, and a macOS profile must never
// exceed MaxActive.
func conservationChecker(cfg worldConfig) checker {
	memoryCeiling := int(cfg.PhysicalMemoryMB - cfg.Guards.MinAvailableMemoryMB)
	return func(_ *world, observation tickObservation) []finding {
		var findings []finding
		total := domain.Resources{}
		linuxRepos := map[string]int{}
		macRepos := map[string]int{}
		macProfiles := map[domain.ProfileID]int{}
		for _, instance := range observation.Instances {
			if !instance.ConsumesHostResources() {
				continue
			}
			total = total.Add(instance.Resources)
			if instance.Platform == domain.PlatformMacOS {
				macProfiles[instance.Profile]++
			}
			// activeRepoCounts is the occupancy the scheduler's own cap test reads:
			// an idle or tearing-down instance holds no repository slot.
			if instance.State == domain.InstanceOnlineIdle || instance.State.TearingDown() {
				continue
			}
			if instance.Platform == domain.PlatformMacOS {
				macRepos[instance.Repo]++
			} else {
				linuxRepos[instance.Repo]++
			}
		}
		if total.CPU > int(cfg.PhysicalCPU) {
			findings = append(findings, finding{Kind: findingConservation, Tick: observation.Tick,
				Detail: fmt.Sprintf("live instances hold %d CPU on a %d-core host", total.CPU, cfg.PhysicalCPU)})
		}
		if total.MemoryMB > memoryCeiling {
			findings = append(findings, finding{Kind: findingConservation, Tick: observation.Tick,
				Detail: fmt.Sprintf("live instances hold %d MB above the %d MB envelope", total.MemoryMB, memoryCeiling)})
		}
		if total.Slots > cfg.Scheduler.LinuxCapacity.Slots {
			findings = append(findings, finding{Kind: findingConservation, Tick: observation.Tick,
				Detail: fmt.Sprintf("live instances hold %d slots above the %d-slot ceiling", total.Slots, cfg.Scheduler.LinuxCapacity.Slots)})
		}
		findings = append(findings, repositoryCapFindings(cfg, observation, linuxRepos, macRepos)...)
		for _, profile := range sortedProfiles(macProfiles) {
			limit := max(cfg.Scheduler.Profiles[profile].MaxActive, 1)
			if macProfiles[profile] > limit {
				findings = append(findings, finding{Kind: findingConservation, Tick: observation.Tick,
					Detail: fmt.Sprintf("macOS profile %s holds %d instances above MaxActive %d", profile, macProfiles[profile], limit)})
			}
		}
		return findings
	}
}

// repositoryCapFindings reports every repository over its cap, and attributes
// the overflow. When the repository's Linux occupancy alone is within cap and
// macOS work carried it over, the finding carries FINDING 2's signature: that is
// not an arithmetic slip in a shared allocator but a whole admission path that
// never consults the cap at all.
func repositoryCapFindings(cfg worldConfig, observation tickObservation, linuxRepos, macRepos map[string]int) []finding {
	occupied := map[string]int{}
	for repo, count := range linuxRepos {
		occupied[repo] += count
	}
	for repo, count := range macRepos {
		occupied[repo] += count
	}
	var findings []finding
	for _, repo := range sortedKeys(occupied) {
		limit := repoCap(cfg, repo)
		if occupied[repo] <= limit {
			continue
		}
		item := finding{Kind: findingConservation, Tick: observation.Tick,
			Detail: fmt.Sprintf("repository %s holds %d instances (%d linux, %d macos) above its cap of %d",
				repo, occupied[repo], linuxRepos[repo], macRepos[repo], limit)}
		if macRepos[repo] > 0 && linuxRepos[repo] <= limit {
			item.Signature = sigMacOSIgnoresRepositoryCap
		}
		findings = append(findings, item)
	}
	return findings
}

func sortedProfiles(counts map[domain.ProfileID]int) []domain.ProfileID {
	ids := make([]domain.ProfileID, 0, len(counts))
	for id := range counts {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func repoCap(cfg worldConfig, repo string) int {
	return max(cfg.Scheduler.RepoCaps[repo], 1)
}

func sortedKeys(counts map[string]int) []string {
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// ---------------------------------------------------------------------------
// The feasibility oracle shared by (a) and (b).
// ---------------------------------------------------------------------------

// feasibleDemands is an INDEPENDENT admission oracle. It is derived from
// physical facts and configured caps -- never from the scheduler's own envelope
// arithmetic -- so it cannot inherit the bug it is meant to catch.
//
// A demand is called feasible only when there is definitely room for it: its
// vector fits both the measured residual and the physical total net of live
// occupancy, a slot is free, its repository is under cap, no live instance
// already incarnates it, no idle runner already matches it (which is how
// consumeCompatibleIdle serves a demand without spawning), and, for macOS, the
// profile is under MaxActive and may coexist with whatever cohort is live.
func feasibleDemands(cfg worldConfig, observation tickObservation) []domain.Demand {
	live := domain.Resources{}
	repos := map[string]int{}
	macProfiles := map[domain.ProfileID]int{}
	incarnated := map[domain.DemandKey]bool{}
	idle := map[string]int{}
	for _, instance := range observation.Instances {
		if instance.IncarnatesDemand() {
			incarnated[instance.Demand] = true
		}
		if !instance.ConsumesHostResources() {
			continue
		}
		live = live.Add(instance.Resources)
		if instance.Platform == domain.PlatformMacOS {
			macProfiles[instance.Profile]++
		}
		if instance.State == domain.InstanceOnlineIdle {
			idle[instance.Repo+"\x00"+string(instance.Profile)]++
			continue
		}
		if !instance.State.TearingDown() {
			repos[instance.Repo]++
		}
	}
	physical := domain.Resources{CPU: int(cfg.PhysicalCPU),
		MemoryMB: int(cfg.PhysicalMemoryMB - cfg.Guards.MinAvailableMemoryMB), Slots: cfg.Scheduler.LinuxCapacity.Slots}
	headroom, ok := physical.Sub(live)
	if !ok {
		return nil
	}
	free := domain.Resources{CPU: min(headroom.CPU, observation.Host.Available.CPU),
		MemoryMB: min(headroom.MemoryMB, observation.Host.Available.MemoryMB), Slots: headroom.Slots}
	var feasible []domain.Demand
	for _, demand := range observation.Demands {
		profile := cfg.Scheduler.Profiles[demand.Profile]
		if incarnated[demand.Key] || !free.CanFit(profile.Resources) {
			continue
		}
		if repos[demand.Key.Repo] >= repoCap(cfg, demand.Key.Repo) {
			continue
		}
		key := demand.Key.Repo + "\x00" + string(demand.Profile)
		if idle[key] > 0 {
			idle[key]--
			continue
		}
		if profile.Platform == domain.PlatformMacOS && !macOSAdmissible(cfg, profile, macProfiles) {
			continue
		}
		feasible = append(feasible, demand)
	}
	return feasible
}

// macOSAdmissible applies the two macOS-only admission rules the physical
// envelope cannot express: the per-profile MaxActive bound, and the
// single-cohort rule that a foreign live profile vetoes growth unless mixed
// cohorts are enabled.
func macOSAdmissible(cfg worldConfig, profile domain.Profile, active map[domain.ProfileID]int) bool {
	if active[profile.ID] >= max(profile.MaxActive, 1) {
		return false
	}
	if cfg.Scheduler.MixedProfileCohorts {
		return true
	}
	for id, count := range active {
		if id != profile.ID && count > 0 {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// (a) Liveness: no wedge.
// ---------------------------------------------------------------------------

// livenessChecker fails when the fleet refuses to admit anything for K
// consecutive ticks while a demand it could definitely serve is queued.
//
// Ticks where the observation itself is unusable, where a drain is already in
// flight, or where a recovery is under way do not count: the fleet is making
// progress of a different kind, and counting them would report the cure as the
// disease.
func livenessChecker(cfg worldConfig) checker {
	stalled := 0
	var since domain.DemandKey
	return func(w *world, observation tickObservation) []finding {
		if observation.Plan.Status != scheduler.PlanReady || !observation.InstancesUsable || !observation.HostUsable ||
			observation.hasDrain() || w.tearingDown(observation) {
			stalled = 0
			return nil
		}
		feasible := feasibleDemands(cfg, observation)
		if len(feasible) == 0 || len(observation.spawns()) > 0 {
			stalled = 0
			return nil
		}
		if stalled == 0 {
			since = feasible[0].Key
		}
		stalled++
		if stalled <= cfg.LivenessK {
			return nil
		}
		stalled = 0
		return []finding{{Kind: findingWedge, Tick: observation.Tick,
			Detail: fmt.Sprintf("no admission for %d ticks while %s was feasible\n%s",
				cfg.LivenessK, since, w.dumpPlan(observation))}}
	}
}

// tearingDown reports whether any instance is already in the cleanup chain, or a
// drain operation is in flight. A drain that has not finished is progress, and
// the capacity it will release is the reason a feasible demand is still waiting.
//
// Provisioning deliberately does NOT count. A VM that is still booting is not an
// excuse to admit nothing, and it cannot produce a false positive either: a
// demand a live instance already incarnates is not feasible by construction.
func (w *world) tearingDown(observation tickObservation) bool {
	for _, instance := range observation.Instances {
		if instance.State.TearingDown() {
			return true
		}
	}
	for _, pending := range w.claimed {
		if pending.operation.Kind == lifecycle.OperationDrain {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// (b) Bounded starvation.
// ---------------------------------------------------------------------------

// boundedStarvationChecker is the aged-FIFO oracle. ADR 0004 promises aging is
// the absolute starvation guard and ADR 0012 promises it holds ACROSS platforms,
// so an aged feasible demand that younger work overtakes more than N times is a
// violation of a written invariant, not a preference.
func boundedStarvationChecker(cfg worldConfig) checker {
	passedOver := map[domain.DemandKey]int{}
	return func(w *world, observation tickObservation) []finding {
		admitted := admittedDemands(observation)
		if len(admitted) == 0 || w.tearingDown(observation) {
			// While a teardown is in flight the envelope is mid-transition: the
			// vector an aged demand is entitled to is held by an instance that is
			// already leaving, and admission order during that window is a statement
			// about what fits right now rather than about priority. The liveness
			// oracle exempts the same ticks for the same reason.
			return nil
		}
		var findings []finding
		for _, demand := range feasibleDemands(cfg, observation) {
			overtaker, overtaken := youngestOvertaker(admitted, demand)
			if !overtaken || !aged(cfg, observation.Now, demand) || holdsReservation(observation, demand) {
				// ADR 0017: a reserved head is protected by ORDERING, not by
				// idleness. It is re-checked first on every later tick and wins the
				// first vector large enough for it, so residual admission ahead of it
				// is the decision, not a violation of it.
				continue
			}
			passedOver[demand.Key]++
			if passedOver[demand.Key] != cfg.StarvationN+1 {
				continue
			}
			findings = append(findings, finding{Kind: findingStarvation,
				Signature: starvationSignature(cfg, demand, overtaker), Tick: observation.Tick,
				Detail: fmt.Sprintf("aged feasible %s (%s old) passed over %d times, this tick by %s (%s old)\n%s",
					demand.Key, observation.Now.Sub(demand.CreatedAt), passedOver[demand.Key],
					overtaker.Key, observation.Now.Sub(overtaker.CreatedAt), w.dumpPlan(observation))})
		}
		return findings
	}
}

// admittedDemands is this tick's spawns resolved back to the demands they came
// from, so an oracle can compare ages rather than keys.
func admittedDemands(observation tickObservation) []domain.Demand {
	byKey := make(map[domain.DemandKey]domain.Demand, len(observation.Demands))
	for _, demand := range observation.Demands {
		byKey[demand.Key] = demand
	}
	var admitted []domain.Demand
	for _, key := range observation.spawns() {
		if demand, ok := byKey[key]; ok {
			admitted = append(admitted, demand)
		}
	}
	return admitted
}

// youngestOvertaker is the newest demand this tick admitted ahead of the given
// one. It is what names the pass-over: without it a finding says a demand waited
// but not who went first, and every repair depends on that.
func youngestOvertaker(admitted []domain.Demand, demand domain.Demand) (domain.Demand, bool) {
	var youngest domain.Demand
	found := false
	for _, candidate := range admitted {
		if candidate.Key == demand.Key {
			return domain.Demand{}, false
		}
		if !demand.CreatedAt.Before(candidate.CreatedAt) {
			continue
		}
		if !found || candidate.CreatedAt.After(youngest.CreatedAt) {
			youngest, found = candidate, true
		}
	}
	return youngest, found
}

// starvationSignature attributes a pass-over to one of the two documented lane
// defects, or to nothing -- in which case the sweep treats it as a new bug.
func starvationSignature(cfg worldConfig, waiting, overtaker domain.Demand) string {
	classes := cfg.Scheduler.RepoSchedulingClasses
	if classes[overtaker.Key.Repo] == domain.SchedulingControlPlane && classes[waiting.Key.Repo] != domain.SchedulingControlPlane {
		return sigControlPlaneOvertakesAgedWork
	}
	if overtaker.Platform != waiting.Platform {
		return sigCrossPlatformResidualArbitration
	}
	if strictlySmaller(cfg.Scheduler.Profiles[overtaker.Profile].Resources, cfg.Scheduler.Profiles[waiting.Profile].Resources) {
		return sigCountMaximizationOvertakesAgedWork
	}
	return ""
}

// holdsReservation reports whether this demand is the one the plan is reserving
// a vector for.
func holdsReservation(observation tickObservation, demand domain.Demand) bool {
	reservation := observation.Plan.Next.Reservation
	return reservation != nil && reservation.Demand == demand.Key
}

// strictlySmaller reports whether one resource vector fits inside another and is
// not the same vector, which is what makes admitting it a COUNT win rather than
// a priority decision.
func strictlySmaller(candidate, incumbent domain.Resources) bool {
	return incumbent.CanFit(candidate) && candidate != incumbent
}

func aged(cfg worldConfig, now time.Time, demand domain.Demand) bool {
	return cfg.Scheduler.FairnessAge > 0 && !demand.CreatedAt.IsZero() &&
		now.Sub(demand.CreatedAt) >= cfg.Scheduler.FairnessAge
}

// ---------------------------------------------------------------------------
// (f) Eventual quiescence.
// ---------------------------------------------------------------------------

// quiescenceChecker fails when the fleet still believes it has work Q ticks
// after GitHub stopped having any. It is the ADR 0026 oracle: a demand the
// broker keeps advertising after the run was cancelled server side blocks
// quiescence, and with it every production release.
func quiescenceChecker(cfg worldConfig) checker {
	settled := 0
	return func(w *world, observation tickObservation) []finding {
		if !w.arrivalsStopped || w.brokerHasWork() {
			settled = 0
			return nil
		}
		settled++
		if settled <= cfg.QuiesceQ {
			return nil
		}
		settled = 0
		if len(observation.Demands) == 0 && len(w.claimed) == 0 {
			return nil
		}
		return []finding{{Kind: findingNoQuiescence, Tick: observation.Tick,
			Detail: fmt.Sprintf("%d demands and %d operations still in flight %d ticks after GitHub went quiet\n%s",
				len(observation.Demands), len(w.claimed), cfg.QuiesceQ, w.dumpPlan(observation))}}
	}
}

// brokerHasWork reports whether GitHub still has anything that could legitimately
// become a runner.
func (w *world) brokerHasWork() bool {
	for _, job := range w.jobs {
		switch job.status {
		case jobQueued:
			if !job.silentCancel {
				return true
			}
		case jobAcquired, jobRunning:
			return true
		case jobDone, jobCancelled:
		}
	}
	return false
}

// pendingOperations is the durable outbox depth, used by pinned incidents that
// assert the fleet really did settle rather than merely stop planning.
func (w *world) pendingOperations() int {
	retrying, dead, err := w.store.OperationCounts(w.ctx)
	if err != nil {
		return -1
	}
	return retrying + dead + len(w.claimed)
}

// liveInstanceCount is the durable live-instance depth, used by the same pinned
// incidents.
func (w *world) liveInstanceCount() int {
	instances, err := w.store.LiveInstances(w.ctx)
	if err != nil {
		return -1
	}
	live := 0
	for _, instance := range instances {
		if instance.State != operations.StateDeleted {
			live++
		}
	}
	return live
}
