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
	// findingStrandedDemand is property (h): no queued demand is held hostage by
	// an instance that is executing a DIFFERENT job. Such a demand can never be
	// respawned (its own instance still incarnates it) and never executed (its
	// runner is busy with something else), so its queue age grows without bound.
	findingStrandedDemand findingKind = "stranded_demand"
	// findingDrainChurn is property (i): a recovery drain the executor aborts must
	// not repeat. Aborting returns the instance to Running, which restarts the very
	// deadline that planned the drain, so a second abort is a loop rather than an
	// unlucky race.
	findingDrainChurn findingKind = "drain_churn"
	// findingUnheardDemand is property (j): a job the broker has actually
	// delivered a JobAvailable for must be in the fleet's durable ledger. Every
	// other oracle here reads the fleet's own view of demand, so all of them are
	// silent exactly when the fleet cannot hear GitHub at all -- which is how a
	// binding stayed deaf for three days in issue #165 while `fleet queues` read
	// zero and `fleet doctor` read PASS.
	findingUnheardDemand findingKind = "unheard_demand"
	// findingOverOccupied is property (k): no instance occupies its profile's
	// resource vector beyond that profile's occupancy budget. It is the issue #223
	// oracle. Every other reclaim in this fleet is premised on the runner being
	// idle and re-verifies that at execution time; a job that has stopped making
	// progress satisfies none of them, so before the budget existed one hung
	// `if: always()` cleanup step held a builder for seventy-three minutes and no
	// deadline in the system could take it back.
	findingOverOccupied findingKind = "occupancy_budget"
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

// sigRespawnLiveIncarnation was FINDING 1 of ADR 0031: between a spawn and its
// runner's JIT acquisition the demand is still JobAvailable to GitHub, nothing
// in the pipeline filtered a demand a live instance already incarnates, and
// every tick in that window re-derived the identical content-addressed spawn.
//
// It is FIXED -- app.plannableDemands is now the single seam that assembles the
// scheduler's queue -- so the sweep no longer tolerates it. The name survives
// because a regression deserves to be reported as itself.
const sigRespawnLiveIncarnation = "respawn_of_a_live_incarnation"

// sigMacOSIgnoresRepositoryCap was FINDING 2 of ADR 0031: scheduler
// .appendMacSpawns bounded macOS admission by the envelope and by the profile's
// MaxActive, and by nothing else. It never read Config.RepoCaps, although
// activeRepoCounts charges a macOS instance to its repository exactly like a
// Linux one -- so a repository's cap bounded its Linux work and not its macOS
// work.
//
// It is FIXED -- appendMacSpawns is the single seam every macOS spawn passes
// through, and it now applies the same cap the Linux allocator does -- so the
// sweep no longer tolerates it. The name survives because a regression deserves
// to be reported as itself.
const sigMacOSIgnoresRepositoryCap = "macos_admission_ignores_repository_cap"

// sigControlPlaneOvertakesAgedWork was FINDING 3 of ADR 0031. ADR 0004 orders
// scheduling as aged global FIFO, then young control-plane, then young standard,
// and priorityOrder honoured it. exactSelect then re-ranked the very same
// candidates by scheduling class alone (betterAdmission's band coverage), and
// aging was not a band there, so a young control-plane demand outranked an aged
// standard one after all.
//
// It is FIXED -- schedulingBand's first band is aging, so the band vector is
// ADR 0004's list -- so the sweep no longer tolerates it. The name survives
// because a regression deserves to be reported as itself.
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
	Tick    int
	Now     time.Time
	Plan    scheduler.Plan
	Applied bool
	Err     error
	// Demands is the PLANNABLE queue: what the scheduler was allowed to admit
	// this tick. app.plannableDemands has already dropped every demand a live
	// instance incarnates, so it answers "what work is still unserved".
	Demands []domain.Demand
	// Queued is the DURABLE queue depth across every binding -- the number the
	// SLO monitor reads. It answers a different question ("what does the fleet
	// still believe it has to do"), and a demand whose runner is already booting
	// counts here while it is absent from Demands.
	Queued          int
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
		// (k) sits here for the same causal reason (h) and (i) do, one layer up: an
		// instance holding a vector past its ceiling is a CAUSE of the wedge, the
		// pass-over, and the queue that will not drain, and reporting one of those
		// instead would send an operator to the scheduler for a hung job. It follows
		// conservation and identity because those answer a stronger question -- the
		// machine is oversubscribed, two rows claim one identity -- which no reading
		// of an occupancy clock can explain away.
		occupancyBudgetChecker(cfg),
		// (h) and (i) precede the liveness and starvation oracles deliberately: a
		// demand nobody can serve and a drain that will not stay dead are the CAUSE
		// of the queue that never drains, and reporting the symptom would send an
		// operator to the scheduler for a binding defect (issue #123).
		strandedDemandChecker(cfg),
		drainChurnChecker(cfg),
		// (j) precedes liveness for the same reason: a binding that cannot hear
		// the broker produces an empty queue, and an empty queue is indistinguishable
		// from an idle fleet to every oracle that reads only the fleet's own view.
		unheardDemandChecker(cfg),
		livenessChecker(cfg),
		boundedStarvationChecker(cfg),
		quiescenceChecker(cfg),
	}
}

// ---------------------------------------------------------------------------
// (h) No demand is stranded on a runner that is executing something else.
// ---------------------------------------------------------------------------

// strandedDemandChecker is the issue #123 oracle.
//
// The fleet binds an instance to ONE demand, and every question it later asks
// about that instance -- is a job active, is it safe to deregister, may this
// demand be planned again -- is answered from that binding. GitHub, meanwhile,
// decides what the runner actually executes. When the broker gives a runner a
// sibling from the same scale set, the two answers come apart and STAY apart:
//
//   - the demand is not plannable, because a live instance incarnates it, so no
//     other VM is ever spawned for it (app.plannableDemands);
//   - it is not executable either, because the instance that holds it is busy
//     with the sibling.
//
// Its queue age then grows without bound, and because aging is the absolute
// scheduling priority (ADR 0004) that unbounded age outranks every genuinely
// newer job on the host. On 2026-08-02 one such demand reported 1h28m and pinned
// an xl VM for the rest of the day.
//
// The oracle is deliberately conditioned on the runner being BUSY. Without that
// clause it would also flag the ADR 0026 ghost -- a runner GitHub never gave any
// work to -- which is a different defect with a different repair.
//
// StrandedG ticks of grace is the broker's delivery budget: the message that
// names the real assignment can be delayed, and evidence that has not arrived
// cannot be acted on.
func strandedDemandChecker(cfg worldConfig) checker {
	stranded := map[string]int{}
	return func(w *world, observation tickObservation) []finding {
		var findings []finding
		seen := map[string]bool{}
		for _, instance := range observation.Instances {
			job := w.strandedDemandOf(instance)
			if job == nil {
				continue
			}
			seen[instance.ID] = true
			stranded[instance.ID]++
			if stranded[instance.ID] != cfg.StrandedG+1 {
				continue
			}
			findings = append(findings, finding{Kind: findingStrandedDemand, Tick: observation.Tick,
				Detail: fmt.Sprintf("%s holds demand %s (queued %s ago, still JobAvailable to GitHub) while GitHub gave its runner %s: the demand can neither be respawned nor served\n%s",
					instance.ID, instance.Demand, observation.Now.Sub(job.queuedAt), w.runnerJobName(instance.ID),
					w.dumpPlan(observation))})
		}
		for id := range stranded {
			if !seen[id] {
				delete(stranded, id)
			}
		}
		return findings
	}
}

// strandedDemandOf reports the broker's job for an instance's bound demand when
// that binding is PROVEN wrong, and nil otherwise. Three facts must hold
// together, and each one excludes a different, legitimate shape:
//
//   - The instance still incarnates the demand, so nothing else may be spawned
//     for it (app.plannableDemands).
//   - GitHub still has that job queued and dispatched to nobody. This excludes
//     ordinary execution and completion, and it excludes the crossed assignment
//     of ADR 0016, where BOTH requests were acquired and neither is queued.
//   - GitHub has dispatched a different job of the same scale set to this
//     instance's runner. This is what makes the binding provably wrong rather
//     than merely unlucky, and it excludes the ADR 0026 ghost -- a runner GitHub
//     never gave any work to -- which is a different defect with a different
//     repair.
//
// The dispatch is remembered after the sibling finishes, deliberately. The
// binding does not heal when the sibling ends: the instance still holds a demand
// it will never execute, and the demand still cannot be respawned. On 2026-08-02
// that state outlived several sibling jobs.
func (w *world) strandedDemandOf(instance domain.Instance) *simJob {
	if !instance.IncarnatesDemand() || !w.runnerRanAnotherJob(instance.ID, instance.Demand.JobID) {
		return nil
	}
	job := w.jobByRequest(instance.Demand.JobID)
	if job == nil || job.status != jobQueued || job.runner != "" || job.silentCancel {
		return nil
	}
	return job
}

// runnerRanAnotherJob reports whether GitHub has dispatched any job other than
// the given request to this runner. A finished job keeps its runner, so this
// stays true once it has happened -- which is the point: the evidence that a
// binding is wrong does not expire when the job that proved it ends.
func (w *world) runnerRanAnotherJob(runner string, bound int64) bool {
	for _, job := range w.jobs {
		if job.runner != runner || job.requestID == bound {
			continue
		}
		switch job.status {
		case jobAcquired, jobRunning, jobDone:
			return true
		case jobQueued, jobCancelled:
		}
	}
	return false
}

// runnerJobName names what a runner is really executing, so a stranding finding
// says which sibling took the runner rather than merely that one did.
func (w *world) runnerJobName(runner string) string {
	for _, job := range w.jobs {
		if job.runner == runner {
			return fmt.Sprintf("request %d (%s)", job.requestID, job.status)
		}
	}
	return "nothing"
}

// ---------------------------------------------------------------------------
// (i) An aborted recovery drain does not repeat.
// ---------------------------------------------------------------------------

// drainChurnChecker fails when one instance has more than DrainChurnN recovery
// drains disproven at execution time.
//
// The abort itself is correct and must stay: it is what upholds the busy-drain
// invariant when planning-time evidence was wrong. What it cannot do is make the
// evidence right. `abort` returns the instance to Running, RunningSince is the
// row's updated_at, so the idle-runner deadline restarts and the identical
// recovery is re-derived one deadline later -- forever, at the cost of a durable
// transition, a GitHub round trip, and a deregistration attempt against a runner
// doing real work (ADR 0028's "not addressed here").
//
// One abort is therefore a race the design anticipates; two is a loop.
func drainChurnChecker(cfg worldConfig) checker {
	reported := map[string]bool{}
	return func(w *world, observation tickObservation) []finding {
		var findings []finding
		for _, id := range sortedKeys(w.drainAborts) {
			if w.drainAborts[id] <= cfg.DrainChurnN || reported[id] {
				continue
			}
			reported[id] = true
			findings = append(findings, finding{Kind: findingDrainChurn, Tick: observation.Tick,
				Detail: fmt.Sprintf("%s had %d recovery drains aborted; its runner is executing %s while its bound demand %s says otherwise\n%s",
					id, w.drainAborts[id], w.runnerJobName(id), boundDemandOf(observation, id), w.dumpPlan(observation))})
		}
		return findings
	}
}

// boundDemandOf names the demand an observed instance is bound to, which is the
// key every recovery decision about it was derived from.
func boundDemandOf(observation tickObservation, id string) domain.DemandKey {
	for _, instance := range observation.Instances {
		if instance.ID == id {
			return instance.Demand
		}
	}
	return domain.DemandKey{}
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
//
// On a node with a configured hostBudget the resource ceiling is the budget
// rather than the machine (issue #136). That is the stronger claim, and it is
// the one the operator actually bought: a budget exists because somebody else's
// work is on this host, so committing vectors that fit the machine but exceed
// the declared share is the failure the setting was added to prevent. The
// oracle derives its own ceiling from the configured facts and never from the
// scheduler's envelope arithmetic, so it cannot inherit the bug it is checking.
func conservationChecker(cfg worldConfig) checker {
	ceiling := cfg.hostCeiling()
	return func(w *world, observation tickObservation) []finding {
		var findings []finding
		total := domain.Resources{}
		macProfiles := map[domain.ProfileID]int{}
		var charged []domain.Instance
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
			charged = append(charged, instance)
		}
		if total.CPU > ceiling.CPU {
			findings = append(findings, finding{Kind: findingConservation, Tick: observation.Tick,
				Detail: fmt.Sprintf("live instances hold %d CPU above the %d-CPU ceiling of %s", total.CPU, ceiling.CPU, cfg.ceilingSource())})
		}
		if total.MemoryMB > ceiling.MemoryMB {
			findings = append(findings, finding{Kind: findingConservation, Tick: observation.Tick,
				Detail: fmt.Sprintf("live instances hold %d MB above the %d MB envelope of %s", total.MemoryMB, ceiling.MemoryMB, cfg.ceilingSource())})
		}
		if total.Slots > ceiling.Slots {
			findings = append(findings, finding{Kind: findingConservation, Tick: observation.Tick,
				Detail: fmt.Sprintf("live instances hold %d slots above the %d-slot ceiling", total.Slots, ceiling.Slots)})
		}
		findings = append(findings, repositoryCapFindings(w, cfg, observation, charged)...)
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
//
// The rebound set costs a durable read, and it is taken only when some
// repository is over its cap counted WITHOUT it. That is sound because excluding
// a rebound instance can only lower a count, so a tick within cap on the full
// count is within cap on either; and it matters because this oracle runs on
// every tick of every arm of every sweep. Property (j) adopted the same
// discipline in issue #165 -- read the ledger once, and only for something that
// actually owes an answer -- after the package outgrew its budget and started
// masking the rest of the nightly job.
func repositoryCapFindings(w *world, cfg worldConfig, observation tickObservation, charged []domain.Instance) []finding {
	if _, _, occupied := repositoryOccupancy(charged, nil); !anyRepositoryOverCap(cfg, occupied) {
		return nil
	}
	// A repository cap bounds ADMISSION -- how many VMs the fleet will CREATE for
	// a repository -- and it cannot bound which repository's job GitHub then
	// dispatches to a runner that already exists. A rebound instance is one the
	// broker moved (ADR 0033); it consumes no new capacity, and the physical
	// envelope above still charges it in full. Charging it to a cap it was never
	// admitted under would report GitHub's decision as a scheduler defect. Before
	// the binding followed GitHub the same VM ran the same foreign job while the
	// fleet mis-attributed it to the repository it was spawned for, so nothing
	// about the machine changed here -- only what the fleet is willing to say
	// about it.
	linuxRepos, macRepos, occupied := repositoryOccupancy(charged, w.reboundInstances())
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

// reboundInstances names every live instance the broker moved off the demand it
// was spawned for. Ownership is immutable, so the durable row carries both facts:
// `ownership.resource_id` is the demand it was born for, and its scheduling
// metadata is the demand GitHub gave it.
func (w *world) reboundInstances() map[string]bool {
	instances, err := w.store.LiveInstances(w.ctx)
	if err != nil {
		return nil
	}
	rebound := make(map[string]bool, len(instances))
	for _, instance := range instances {
		if instance.Demand != (domain.DemandKey{}) && instance.Ownership.ResourceID != instance.Demand.String() {
			rebound[instance.ID] = true
		}
	}
	return rebound
}

// repositoryOccupancy is the per-repository slot occupancy of the instances a
// cap charges, split by platform so a finding can attribute the overflow.
func repositoryOccupancy(charged []domain.Instance, rebound map[string]bool) (linuxRepos, macRepos, occupied map[string]int) {
	linuxRepos, macRepos, occupied = map[string]int{}, map[string]int{}, map[string]int{}
	for _, instance := range charged {
		if rebound[instance.ID] {
			continue
		}
		if instance.Platform == domain.PlatformMacOS {
			macRepos[instance.Repo]++
		} else {
			linuxRepos[instance.Repo]++
		}
		occupied[instance.Repo]++
	}
	return linuxRepos, macRepos, occupied
}

func anyRepositoryOverCap(cfg worldConfig, occupied map[string]int) bool {
	for repo, count := range occupied {
		if count > repoCap(cfg, repo) {
			return true
		}
	}
	return false
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
// (k) No instance occupies its vector beyond its profile budget.
// ---------------------------------------------------------------------------

// occupancyBudgetChecker is the issue #223 oracle: every live instance whose
// profile declares an occupancy budget must stop holding its resource vector
// within OccupancyGraceTicks of reaching that budget.
//
// The age is measured from the harness's OWN virtual ground truth -- w.createdAt,
// the tick the world first held the row -- and never from domain.Instance
// .OccupiedSince or scheduler.Occupancies. That is the same discipline
// feasibleDemands follows for the envelope: an oracle derived from the arithmetic
// it is checking cannot fail when that arithmetic is wrong, and the defect this
// property exists for is precisely a hold nothing in the fleet was measuring.
//
// The grace is what separates a reclaim from a leak, and every tick in it is a
// tick the fleet cannot skip: the breach must be observed (the inventory read at
// the head of the tick), the plan must commit, the drain operation must be
// claimed from the outbox, and the executor must reach the guest stop that
// releases the vector. A healthy reclaim costs ONE tick of over-budget occupancy
// -- the tick the breach is first visible -- because the stop happens inside
// that same tick's execution and the instance leaves this oracle's scope on the
// next one.
//
// Twelve is measured, not guessed. Over the three sweep arms at 80 seeds and 200
// ticks the whole observed distribution of over-budget ticks is
// {1: 10, 2: 3, 3: 3, 4: 2, 8: 1}: one tick is the mode, and the single 8-tick
// tail is a budgeted-host maestro whose observation was blinded while the breach
// was accruing. Twelve is half again the worst case the generator has produced,
// which is enough slack for the delays it can stack -- a stale host probe or an
// unavailable Tart blinds the observation for up to six ticks, a wedged drain
// holds the deregistration for up to six more -- and still an order of magnitude
// below the leak it exists to catch, which is unbounded.
//
// Scope is two clauses, and both are the property's meaning rather than
// convenience. An instance is judged only while it CONSUMES host resources,
// which is the same fact the budget is about and the same set the conservation
// oracle charges: once the guest is off, the vector is back on the host whatever
// the durable row still says. And an instance already TEARING DOWN is excluded,
// because the fleet is by then doing the very thing this property demands -- a
// durable drain is in flight and the vector is on its way back. Whether that
// drain finishes is the drain's own contract (ADR 0007's owned-cleanup retry and
// ADR 0020's diagnosable failures), not the budget's, and the budget could not
// act on it in any case: scheduler.assignmentRecoveries only ever considers an
// Assigned or Running instance. Judging teardown here would report a stuck drain
// as an unreclaimed hold and send an operator to the occupancy ceiling for a
// deregistration that GitHub refused.
func occupancyBudgetChecker(cfg worldConfig) checker {
	reported := map[string]bool{}
	return func(w *world, observation tickObservation) []finding {
		grace := time.Duration(cfg.OccupancyGraceTicks) * simTick
		// Keyed by instance so the report is stable, valued by how many ticks the
		// hold has lasted so the finding says how far past the ceiling it went.
		overBudget := map[string]int{}
		budgets := map[string]time.Duration{}
		for _, instance := range observation.Instances {
			budget := cfg.Scheduler.Profiles[instance.Profile].OccupancyBudget
			since, known := w.createdAt[instance.ID]
			if budget <= 0 || !known || reported[instance.ID] ||
				!instance.ConsumesHostResources() || instance.State.TearingDown() {
				continue
			}
			age := observation.Now.Sub(since)
			if age <= budget+grace {
				continue
			}
			overBudget[instance.ID] = int(age / simTick)
			budgets[instance.ID] = budget
		}
		var findings []finding
		for _, id := range sortedKeys(overBudget) {
			reported[id] = true
			findings = append(findings, finding{Kind: findingOverOccupied, Tick: observation.Tick,
				Detail: fmt.Sprintf("%s has held %+v for %d ticks, past its %s budget plus %d ticks of reclaim grace; its runner is executing %s\n%s",
					id, occupiedVector(observation, id), overBudget[id], budgets[id], cfg.OccupancyGraceTicks,
					w.runnerJobName(id), w.dumpPlan(observation))})
		}
		return findings
	}
}

// occupiedVector is the resource vector an over-budget instance is holding,
// which is what the queue behind it is waiting for.
func occupiedVector(observation tickObservation, id string) domain.Resources {
	for _, instance := range observation.Instances {
		if instance.ID == id {
			return instance.Resources
		}
	}
	return domain.Resources{}
}

// ---------------------------------------------------------------------------
// The feasibility oracle shared by (a) and (b).
// ---------------------------------------------------------------------------

// feasibleDemands is an INDEPENDENT admission oracle. It is derived from
// physical facts and configured caps -- never from the scheduler's own envelope
// arithmetic -- so it cannot inherit the bug it is meant to catch.
//
// A demand is called feasible only when there is definitely room for it: its
// vector fits both the measured residual and the host ceiling -- the physical
// total net of live occupancy, narrowed by any configured hostBudget -- a slot is free, its repository is under cap, no live instance
// already incarnates it, no idle runner already matches it (which is how
// consumeCompatibleIdle serves a demand without spawning), and, for macOS, the
// profile is under MaxActive and may coexist with whatever cohort is live.
//
// "Definitely" is the whole word. A held reservation is a WRITTEN rule about
// which vectors this tick may hand out, so an envelope that ignores it does not
// measure admissibility -- see reservedResidual.
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
	headroom, ok := cfg.hostCeiling().Sub(live)
	if !ok {
		return nil
	}
	free := domain.Resources{CPU: min(headroom.CPU, observation.Host.Available.CPU),
		MemoryMB: min(headroom.MemoryMB, observation.Host.Available.MemoryMB), Slots: headroom.Slots}
	head, residual, withheld := reservedResidual(cfg, observation, free)
	var feasible []domain.Demand
	for _, demand := range observation.Demands {
		profile := cfg.Scheduler.Profiles[demand.Profile]
		// The reserved head is entitled to the vector, so it is judged against the
		// envelope that vector comes out of. Everything else is judged against what
		// the fleet is allowed to hand out beside it.
		envelope := free
		if withheld && demand.Key != head {
			envelope = residual
		}
		if incarnated[demand.Key] || !envelope.CanFit(profile.Resources) {
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

// reservedResidual is the envelope every demand OTHER than the reserved head
// must fit, and whether a reservation withholds anything from it at all.
//
// This is the issue #216 refinement, and it is the same shape as issue #129's
// (feasibleDemands did not model the tick's own repository-cap consumption):
// the oracle's arithmetic was right and its question was incomplete. A tick that
// holds a reservation is not a tick with a free envelope, because the fleet has
// a WRITTEN rule about part of it.
//
// The distinction is ADR 0017's own, and both halves of it are load-bearing:
//
//   - The head does NOT fit the free envelope. Nothing is charged, and admission
//     proceeds in the full residual (ADR 0017; ADR 0029 condition 1's second
//     sentence). Such a head is blocked by live instances holding what it needs,
//     so it cannot start until they release whatever backfill does -- it is not
//     waiting on backfill to stop. Work that fits and is refused HERE is a wedge,
//     and it is precisely the 2026-07-25 incident and issue #125 that property
//     (a) was built for. This function returns false and changes nothing.
//   - The head DOES fit. ADR 0029 condition 1 withholds its whole vector by
//     design, and `safeBackfill` plans inside `free - reservation`. A demand that
//     fits only INSIDE that vector is not "definitely" admissible: admitting it
//     would take exactly what the oldest aged demand is entitled to, so calling
//     it feasible reports the aged-FIFO guarantee of ADR 0004 as a defect.
//
// Two properties of the inputs keep ADR 0031's independence rule intact. The
// reservation is read from the plan's own next state, which is a DECISION the
// plan publishes -- the same fact property (b) already reads through
// holdsReservation -- and not an envelope computation. And the vector subtracted
// is the head's CONFIGURED profile vector, taken from worldConfig, never the
// `Resources` the scheduler stamped on the reservation and never any of its free,
// aged, or remainder envelopes. So an envelope defect cannot reach this oracle;
// only a reservation the scheduler admits to holding can.
//
// What is deliberately NOT modelled is ADR 0030's repository slot: a head also
// holds one slot of its own repository's cap, and the remainder passes honour it.
// Charging it here would narrow the oracle further, and narrowing is the
// direction that BLINDS a property. Leaving it out can only produce a report the
// scheduler must then justify, which is the direction a harness is allowed to
// err in.
func reservedResidual(cfg worldConfig, observation tickObservation, free domain.Resources) (domain.DemandKey, domain.Resources, bool) {
	reservation := observation.Plan.Next.Reservation
	if reservation == nil {
		return domain.DemandKey{}, free, false
	}
	residual, fits := free.Sub(cfg.Scheduler.Profiles[reservation.Profile].Resources)
	if !fits {
		return domain.DemandKey{}, free, false
	}
	return reservation.Demand, residual, true
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
// (j) Every advertised job is heard.
// ---------------------------------------------------------------------------

// unheardDemandChecker is the issue #165 oracle, and it is the only one that
// reads GitHub's truth rather than the fleet's.
//
// The broker delivered a JobAvailable. Ingestion is synchronous with delivery --
// the coordinator commits before the message is acknowledged (rule 6) -- so
// within a tick or two of delivery that job must exist in the durable demand
// ledger, whatever the scheduler then decides to do with it. When it does not,
// the fleet is not slow, over capacity, or fair-sharing: it is deaf, and the job
// is invisible to every queue count, health reason, and metric the fleet
// publishes. That is precisely the state scale set 8077185082566234948 held from
// 2026-08-01T18:37Z to 2026-08-04T19:40Z.
//
// The oracle is measured from delivery, not creation, so message delay is not a
// violation; and a silently cancelled run is exempt because GitHub advertising a
// job it has already retired is ADR 0026's ghost, not a lost message.
func unheardDemandChecker(cfg worldConfig) checker {
	return func(w *world, observation tickObservation) []finding {
		budget := cfg.HearingH
		if budget <= 0 {
			return nil
		}
		// The ledger is read at most once per binding per tick, and only for a
		// binding that actually owes an answer. A job proven heard is latched, so
		// the steady state of a healthy fleet costs one query per job for its whole
		// life -- the oracle must be able to run inside every arm of every sweep,
		// under the race detector, without becoming the reason the suite is slow.
		var heard map[int]map[int64]struct{}
		for _, job := range w.jobs {
			if job.heard || job.status != jobQueued || job.silentCancel || job.advertisedAt == 0 ||
				observation.Tick-job.advertisedAt <= budget {
				continue
			}
			if heard == nil {
				heard = map[int]map[int64]struct{}{}
			}
			known, read := heard[job.binding]
			if !read {
				known = w.durableRequests(job.binding)
				heard[job.binding] = known
			}
			if known == nil {
				// Rule 4: an unreadable ledger is not an empty one. The store's own
				// fail-closed channel reports it; this oracle must not double-report it
				// as a lost message.
				continue
			}
			if _, ok := known[job.requestID]; ok {
				job.heard = true
				continue
			}
			return []finding{{Kind: findingUnheardDemand, Tick: observation.Tick,
				Detail: fmt.Sprintf("scale set %d was handed request %d at tick %d and still holds no durable demand for it %d ticks later",
					cfg.Bindings[job.binding].StoreKey, job.requestID, job.advertisedAt, observation.Tick-job.advertisedAt)}}
		}
		return nil
	}
}

// durableRequests is every runner request this binding holds a demand row for.
// Any status counts: the question is whether the broker's evidence was heard at
// all, not what the scheduler concluded from it. A nil result means the ledger
// could not be read, which is not the same as an empty one.
func (w *world) durableRequests(binding int) map[int64]struct{} {
	records, err := w.store.ActiveDemands(w.ctx, w.cfg.Bindings[binding].StoreKey)
	if err != nil {
		return nil
	}
	requests := make(map[int64]struct{}, len(records))
	for _, record := range records {
		requests[record.RunnerRequestID] = struct{}{}
	}
	return requests
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
	// Keyed on (demand, CAUSE), not on the demand alone. A demand passed over once
	// by each of several mechanisms has not been starved N times by any of them,
	// and crediting the accumulated total to whichever cause happened to tip the
	// counter reports the last mechanism for all the previous ones' work -- so a
	// run of documented pass-overs surfaces as an unsignatured hard failure on its
	// final tick, naming a mechanism that acted once. That is the oracle
	// imprecision issue #130 records as finding 6. Each cause now earns its own
	// N+1, so a genuinely new defect must repeat before it fails the build and a
	// documented one can no longer masquerade as one.
	passedOver := map[string]int{}
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
			signature := starvationSignature(cfg, observation.Now, demand, overtaker)
			cause := demand.Key.String() + "\x00" + signature
			passedOver[cause]++
			if passedOver[cause] != cfg.StarvationN+1 {
				continue
			}
			findings = append(findings, finding{Kind: findingStarvation, Signature: signature, Tick: observation.Tick,
				Detail: fmt.Sprintf("aged feasible %s (%s old) passed over %d times by one cause, this tick by %s (%s old)\n%s",
					demand.Key, observation.Now.Sub(demand.CreatedAt), passedOver[cause],
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

// starvationSignature attributes a pass-over to one of the documented lane
// defects, or to nothing -- in which case the sweep treats it as a new bug.
//
// Attribution names the MECHANISM that decided the tick, not a coincidence of
// the two demands' attributes. The order of the tests below is that reasoning,
// and it is not cosmetic: a signature that outlives its own defect keeps
// reporting a FIXED finding, which is how a repair looks incomplete and an open
// finding hides. Both tightenings below were made on 2026-08-03, when finding 3
// was fixed and its signature stopped being tolerated.
//
//   - Different platforms: the two demands were never in one candidate list, so
//     no lane inside a pass ever ordered them. The residual is arbitrated one
//     platform at a time -- planLinux and safeBackfill, then fillMacRemainder --
//     and that pass ordering is what decided the tick whatever class or size the
//     overtaker happens to carry. Aging cannot be the explanation either, since
//     priorityOrder ranks aged work above every young lane on both platforms.
//   - A strictly smaller overtaker: its SIZE is a sufficient explanation. It fits
//     vectors and residuals the waiting demand cannot -- more of them per
//     envelope, and whatever another pass leaves behind -- which is finding 5,
//     the count-and-packing question. Since 2026-08-03 no young lane can take a
//     vector an aged demand could have used (exactSelect settles the aged band's
//     members before it reads any younger band), so a smaller overtaker won on
//     size or on a residual, never on its lane.
//   - Scheduling class: a lane only between YOUNG demands of the same size or
//     larger. ADR 0004's aged band is class-blind, so an overtaker that is itself
//     aged did not win because of its class either.
func starvationSignature(cfg worldConfig, now time.Time, waiting, overtaker domain.Demand) string {
	if overtaker.Platform != waiting.Platform {
		return sigCrossPlatformResidualArbitration
	}
	if strictlySmaller(cfg.Scheduler.Profiles[overtaker.Profile].Resources, cfg.Scheduler.Profiles[waiting.Profile].Resources) {
		return sigCountMaximizationOvertakesAgedWork
	}
	classes := cfg.Scheduler.RepoSchedulingClasses
	if !aged(cfg, now, overtaker) && classes[overtaker.Key.Repo] == domain.SchedulingControlPlane &&
		classes[waiting.Key.Repo] != domain.SchedulingControlPlane {
		return sigControlPlaneOvertakesAgedWork
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
		// The DURABLE queue depth, not the plannable one: a demand whose runner is
		// already live is absent from the plannable queue by design, and the ghost
		// of ADR 0026 is exactly such a demand. Measuring the plannable queue here
		// would call that fleet quiescent while it still holds a runner for work
		// GitHub no longer has.
		if observation.Queued == 0 && len(w.claimed) == 0 {
			return nil
		}
		return []finding{{Kind: findingNoQuiescence, Tick: observation.Tick,
			Detail: fmt.Sprintf("%d demands and %d operations still in flight %d ticks after GitHub went quiet\n%s",
				observation.Queued, len(w.claimed), cfg.QuiesceQ, w.dumpPlan(observation))}}
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
