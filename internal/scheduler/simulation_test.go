package scheduler

// This file adds a deterministic multi-tick simulation + property-testing
// harness for the pure scheduler. Each tick it feeds PlanTick a faithful
// in-memory world, applies the returned Plan (spawn -> new instance, drain ->
// teardown walk, lifecycle advanced one legal step per tick), and feeds
// plan.Next back as the next tick's Prior. Safety oracles run every tick;
// liveness oracles run over a bounded window.
//
// Motivation (real shipped bugs the harness must be able to catch):
//   - Head-of-line starvation: an idle host whose oldest demand was an
//     infeasible 12 GiB macOS builder produced EMPTY plans forever, starving
//     feasible jobs (fixed in PR #78 with reservation + backfill).
//   - Drain-safety: never drain a busy instance (issue #72 class).
//
// The executor is intentionally minimal but tied to the domain contract: every
// state transition it performs is validated with domain.InstanceState
// .CanTransitionTo, and host-resource occupancy is read through
// domain.Instance.ConsumesHostResources, exactly as the scheduler sees it.

import (
	"math/rand"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// simTickDuration maps one tick to wall-clock time so demand aging and
// FairnessAge behave. With FairnessAge = 5m in testConfig, a demand ages past
// fairness after 5 ticks.
const simTickDuration = time.Minute

// simMaxJobTicks bounds how long a Running job executes before it completes and
// the instance tears down. Seeded, so runs are reproducible.
const simMaxJobTicks = 4

// simWorld is the tiny in-memory world the model executor advances.
type simWorld struct {
	t      *testing.T
	now    time.Time
	config Config
	host   domain.Host
	// instances are the live durable VMs, mirroring the inventory observation.
	instances []domain.Instance
	// pending is the scheduler-visible demand queue (jobs GitHub still reports as
	// available). A demand leaves pending the moment a spawn binds it: a job with
	// a runner being provisioned is no longer offered, which also prevents a
	// double-spawn artifact. Job completion (Running -> teardown) frees capacity.
	pending []domain.Demand
	state   State
	rng     *rand.Rand

	// jobTicksLeft[id] counts remaining Running ticks for instance id.
	jobTicksLeft map[string]int
	// stuck[id] marks an instance whose assigned job never starts — a zombie
	// runner. Such an instance is held in Assigned (never advanced to Running)
	// so its AssignedSince ages past the assignment deadline, exactly the
	// 84-minute incident this models.
	stuck map[string]bool
	// lingering[id] marks an instance whose job ended (or was cancelled before
	// it began) yet the instance is stuck in Running with no active job. It is
	// held in Running with JobInactive set so its RunningSince ages past the
	// idle-runner deadline, exactly tonight's 74-minute lingerer incident.
	lingering map[string]bool
	// jobID hands out unique, deterministic demand identifiers.
	jobID int64

	// enforceHostBound turns on the mixed-admission resource oracle: the
	// go-forward fleet (consuming instances not drained this tick, plus this
	// tick's spawns) must fit Host.Available. It is opt-in because a
	// deliberately shrinking host can leave a pre-existing fleet over-subscribed
	// through no fault of the scheduler.
	enforceHostBound bool

	// bookkeeping for liveness oracles.
	spawnedAt            map[domain.DemandKey]int
	feasiblePendingTicks map[domain.DemandKey]int
	everFeasible         map[domain.DemandKey]bool
	spawnedCount         int
}

func newSimWorld(t *testing.T, cfg Config, host domain.Host, seed int64) *simWorld {
	t.Helper()
	return &simWorld{
		t:                    t,
		now:                  testNow,
		config:               cfg,
		host:                 host,
		rng:                  rand.New(rand.NewSource(seed)),
		jobTicksLeft:         map[string]int{},
		stuck:                map[string]bool{},
		lingering:            map[string]bool{},
		spawnedAt:            map[domain.DemandKey]int{},
		feasiblePendingTicks: map[domain.DemandKey]int{},
		everFeasible:         map[domain.DemandKey]bool{},
	}
}

// nextJobID returns a fresh unique job identifier.
func (w *simWorld) nextJobID() int64 {
	w.jobID++
	return w.jobID
}

// planInput assembles the scheduler Input from current world state.
func (w *simWorld) planInput() Input {
	return Input{
		Now:       w.now,
		Config:    w.config,
		Demands:   domain.Fresh(append([]domain.Demand(nil), w.pending...), w.now),
		Instances: domain.Fresh(append([]domain.Instance(nil), w.instances...), w.now),
		Host:      domain.Fresh(w.host, w.now),
		Prior:     w.state,
	}
}

// freeEnvelope mirrors scheduler.linuxFree: the shared admission headroom.
func (w *simWorld) freeEnvelope() domain.Resources {
	free := w.config.LinuxCapacity
	for _, instance := range w.instances {
		if instance.ConsumesHostResources() {
			var ok bool
			free, ok = free.Sub(instance.Resources)
			if !ok {
				return domain.Resources{}
			}
		}
	}
	return minResources(free, w.host.Available)
}

// consumingResources sums the resources of every instance that currently
// occupies the host's compute envelope.
func (w *simWorld) consumingResources() domain.Resources {
	var used domain.Resources
	for _, instance := range w.instances {
		if instance.ConsumesHostResources() {
			used = used.Add(instance.Resources)
		}
	}
	return used
}

// linuxRepoCount mirrors scheduler.activeRepoCounts, scoped to Linux, so the
// repo-cap oracle uses the same admission accounting the scheduler does.
func (w *simWorld) linuxRepoCount(repo string) int {
	count := 0
	for _, instance := range w.instances {
		if instance.Platform != domain.PlatformLinux {
			continue
		}
		if !instance.Live() || instance.State == domain.InstanceOnlineIdle || teardownState(instance.State) {
			continue
		}
		if instance.Repo == repo {
			count++
		}
	}
	return count
}

// macActiveForProfile counts live macOS instances of a profile, mirroring the
// MaxActive accounting appendMacSpawns uses.
func (w *simWorld) macActiveForProfile(profile domain.ProfileID) int {
	count := 0
	for _, instance := range w.instances {
		if instance.ConsumesHostResources() && instance.Platform == domain.PlatformMacOS && instance.Profile == profile {
			count++
		}
	}
	return count
}

// admittable reports whether a demand could be admitted right now given current
// free resources and the relevant concurrency cap. It is the liveness notion of
// "feasible": fits free resources at least once during the window.
func (w *simWorld) admittable(d domain.Demand) bool {
	profile := w.config.Profiles[d.Profile]
	if !w.freeEnvelope().CanFit(profile.Resources) {
		return false
	}
	if profile.Platform == domain.PlatformMacOS {
		return w.macActiveForProfile(profile.ID) < macProfileLimit(profile)
	}
	cap := w.config.RepoCaps[d.Key.Repo]
	if cap <= 0 {
		cap = 1
	}
	return w.linuxRepoCount(d.Key.Repo) < cap
}

// mustTransition asserts a lifecycle step is legal per the domain contract and
// returns the next state. It ties the executor to domain.CanTransitionTo.
func (w *simWorld) mustTransition(from, to domain.InstanceState) domain.InstanceState {
	if !from.CanTransitionTo(to) {
		w.t.Fatalf("illegal lifecycle transition %s -> %s", from, to)
	}
	return to
}

// spawnInstance materializes a spawn operation as a new Planned instance, the
// same initial state the reconcile controller commits (operations.StatePlanned).
func (w *simWorld) spawnInstance(op Operation) {
	profile := w.config.Profiles[op.Profile]
	id := "vm-" + op.Demand.String()
	w.instances = append(w.instances, domain.Instance{
		ID:        id,
		Repo:      op.Demand.Repo,
		Platform:  profile.Platform,
		Profile:   profile.ID,
		Route:     profile.Route,
		Resources: profile.Resources,
		State:     domain.InstancePlanned,
		Power:     domain.InstancePowerRunning,
	})
	// Binding == acquisition: the job leaves the schedulable queue.
	filtered := w.pending[:0:0]
	for _, d := range w.pending {
		if d.Key != op.Demand {
			filtered = append(filtered, d)
		}
	}
	w.pending = filtered
}

// advanceInstance walks one instance one legal step per tick. drained marks an
// instance a scheduler Drain op targeted this tick. Returns keep=false when the
// instance reaches Deleted and should be removed from the world.
func (w *simWorld) advanceInstance(inst domain.Instance, drained bool) (domain.Instance, bool) {
	// A scheduler drain, or a completed job, pushes an active instance into the
	// teardown chain. Draining is the single legal entry per the domain graph.
	if drained && inst.State != domain.InstanceDraining && !teardownState(inst.State) && inst.State.CanTransitionTo(domain.InstanceDraining) {
		inst.State = w.mustTransition(inst.State, domain.InstanceDraining)
		return inst, true
	}
	switch inst.State {
	case domain.InstancePlanned:
		inst.State = w.mustTransition(inst.State, domain.InstanceCloning)
	case domain.InstanceCloning:
		inst.State = w.mustTransition(inst.State, domain.InstanceBooting)
	case domain.InstanceBooting:
		inst.State = w.mustTransition(inst.State, domain.InstanceReachable)
	case domain.InstanceReachable:
		inst.State = w.mustTransition(inst.State, domain.InstanceRegistering)
	case domain.InstanceRegistering:
		inst.State = w.mustTransition(inst.State, domain.InstanceAssigned)
		inst.AssignedSince = w.now
	case domain.InstanceAssigned:
		if w.stuck[inst.ID] {
			// A zombie runner: assigned but the job never starts. It stays Assigned
			// (AssignedSince fixed) until the assignment deadline opens recovery.
			return inst, true
		}
		inst.State = w.mustTransition(inst.State, domain.InstanceRunning)
		inst.RunningSince = w.now
		w.jobTicksLeft[inst.ID] = w.rng.Intn(simMaxJobTicks) + 1
	case domain.InstanceRunning:
		if w.lingering[inst.ID] {
			// A lingering runner: its job ended but the instance stays Running with
			// no active job (RunningSince fixed, JobInactive set) until the
			// idle-runner deadline opens recovery.
			inst.JobInactive = true
			return inst, true
		}
		if w.jobTicksLeft[inst.ID] > 0 {
			w.jobTicksLeft[inst.ID]--
			return inst, true
		}
		inst.State = w.mustTransition(inst.State, domain.InstanceDraining)
	case domain.InstanceDraining:
		inst.State = w.mustTransition(inst.State, domain.InstanceDeregistering)
	case domain.InstanceDeregistering:
		inst.State = w.mustTransition(inst.State, domain.InstanceStopping)
	case domain.InstanceStopping:
		inst.State = w.mustTransition(inst.State, domain.InstanceDeleted)
		delete(w.jobTicksLeft, inst.ID)
		return inst, false
	}
	return inst, true
}

// step runs one tick: plan, run safety oracles, apply, advance, and feed
// plan.Next back as Prior. tick is the current tick index for bookkeeping.
func (w *simWorld) step(tick int, checkDeterminism bool) Plan {
	in := w.planInput()
	plan := PlanTick(in)

	if checkDeterminism {
		assertInputCanonicalization(w.t, in, plan)
	}
	w.assertSafety(plan)

	// Liveness bookkeeping: which pending demands were admittable this tick.
	for _, d := range w.pending {
		if w.admittable(d) {
			w.everFeasible[d.Key] = true
			w.feasiblePendingTicks[d.Key]++
		}
	}

	drained := map[string]bool{}
	for _, op := range plan.Operations {
		switch op.Kind {
		case OperationSpawn:
			w.spawnInstance(op)
			w.spawnedAt[op.Demand] = tick
			w.spawnedCount++
		case OperationDrain:
			drained[op.Instance] = true
		}
	}

	next := w.instances[:0:0]
	for _, inst := range w.instances {
		updated, keep := w.advanceInstance(inst, drained[inst.ID])
		if keep {
			next = append(next, updated)
		}
	}
	w.instances = next

	w.state = plan.Next
	w.now = w.now.Add(simTickDuration)
	return plan
}

// -- Safety oracles ---------------------------------------------------------

// assertSafety runs every per-tick safety oracle against a plan given the
// current (pre-apply) world state.
func (w *simWorld) assertSafety(plan Plan) {
	w.t.Helper()
	w.assertDrainSafety(plan)
	w.assertCapacity(plan)
	w.assertMacCohort(plan)
}

// assertMacCohort is the mixed-admission safety oracle. It evaluates the
// go-forward fleet — instances that consume the host and are NOT drained this
// tick, plus this tick's spawns — and pins the macOS platform invariants that
// mixed admission must never breach: at most one macOS profile is active (the
// single-cohort rule), each macOS profile stays within its MaxActive, and the
// total macOS guest count never exceeds Apple's hard two-guest ceiling. It uses
// the go-forward set (excluding instances drained this tick) so a legitimate
// profile switch — which drains the old cohort while spawning the new one — is
// not misread as a transient cohort violation. It holds with the flag off too,
// so it strengthens every existing simulation.
func (w *simWorld) assertMacCohort(plan Plan) {
	w.t.Helper()
	drained := map[string]bool{}
	for _, op := range plan.Operations {
		if op.Kind == OperationDrain {
			drained[op.Instance] = true
		}
	}
	perProfile := map[domain.ProfileID]int{}
	for _, inst := range w.instances {
		if inst.ConsumesHostResources() && inst.Platform == domain.PlatformMacOS && !drained[inst.ID] {
			perProfile[inst.Profile]++
		}
	}
	for _, op := range plan.Operations {
		if op.Kind == OperationSpawn && w.config.Profiles[op.Profile].Platform == domain.PlatformMacOS {
			perProfile[op.Profile]++
		}
	}
	total := 0
	for profile, count := range perProfile {
		limit := macProfileLimit(w.config.Profiles[profile])
		if count > limit {
			w.t.Fatalf("macOS profile %q go-forward count %d exceeds MaxActive %d: plan=%#v", profile, count, limit, plan.Operations)
		}
		total += count
	}
	if len(perProfile) > 1 {
		w.t.Fatalf("more than one macOS profile active go-forward (single-cohort breach): %#v", perProfile)
	}
	if total > 2 {
		w.t.Fatalf("go-forward macOS guest count %d exceeds Apple's two-guest ceiling: %#v", total, perProfile)
	}
}

// drainable defines the states in which draining an instance cannot kill a live
// CI job, mirroring the busy-drain invariant suite (issue #72):
//   - a normal drain requires OnlineIdle;
//   - a recovery drain requires Assigned/Running that is provably inactive
//     (VM powered off, or runner confirmed inactive).
func (w *simWorld) assertDrainSafety(plan Plan) {
	w.t.Helper()
	byID := map[string]domain.Instance{}
	for _, inst := range w.instances {
		byID[inst.ID] = inst
	}
	for _, op := range plan.Operations {
		if op.Kind != OperationDrain {
			continue
		}
		inst, ok := byID[op.Instance]
		if !ok {
			w.t.Fatalf("drain targets unknown instance %q", op.Instance)
		}
		if op.Recovery {
			busy := inst.State == domain.InstanceAssigned || inst.State == domain.InstanceRunning
			confirmedInactive := inst.Power == domain.InstancePowerStopped ||
				(inst.Power == domain.InstancePowerRunning && inst.RecoveryReady)
			// A stalled assignment is provably not running a job: only a JobStarted
			// event advances Assigned -> Running, so an instance still Assigned past
			// the deadline never began work and is safe to reclaim.
			stalled := inst.State == domain.InstanceAssigned && inst.Power == domain.InstancePowerRunning &&
				!inst.RecoveryReady && w.config.AssignedTimeout > 0 && !inst.AssignedSince.IsZero() &&
				w.now.Sub(inst.AssignedSince) >= w.config.AssignedTimeout
			// A lingering runner is provably not running a job: its bound demand
			// carries no active job (JobInactive), so a Running instance past the
			// idle-runner deadline holds capacity behind work that already ended and
			// is safe to reclaim. A genuinely busy runner has JobInactive false.
			lingering := inst.State == domain.InstanceRunning && inst.Power == domain.InstancePowerRunning &&
				!inst.RecoveryReady && inst.JobInactive && w.config.AssignedTimeout > 0 && !inst.RunningSince.IsZero() &&
				w.now.Sub(inst.RunningSince) >= w.config.AssignedTimeout
			if !busy || (!confirmedInactive && !stalled && !lingering) {
				w.t.Fatalf("recovery drain of unsafe instance %q state=%s power=%s recoveryReady=%v assignedSince=%s runningSince=%s jobInactive=%v",
					op.Instance, inst.State, inst.Power, inst.RecoveryReady, inst.AssignedSince, inst.RunningSince, inst.JobInactive)
			}
			continue
		}
		if inst.State != domain.InstanceOnlineIdle {
			w.t.Fatalf("drain of busy instance %q in state %s (only OnlineIdle is safe for a non-recovery drain)",
				op.Instance, inst.State)
		}
	}
}

// assertCapacity checks that consuming instances plus this tick's spawns never
// exceed LinuxCapacity nor Host.Available, that the 4-slot cap holds, and that
// per-repo Linux caps are respected.
func (w *simWorld) assertCapacity(plan Plan) {
	w.t.Helper()
	var spawns domain.Resources
	repoAdds := map[string]int{}
	for _, op := range plan.Operations {
		if op.Kind != OperationSpawn {
			continue
		}
		profile := w.config.Profiles[op.Profile]
		spawns = spawns.Add(profile.Resources)
		if profile.Platform == domain.PlatformLinux {
			repoAdds[op.Demand.Repo]++
		}
	}
	total := w.consumingResources().Add(spawns)
	if !w.config.LinuxCapacity.CanFit(total) {
		w.t.Fatalf("overcommit vs LinuxCapacity: consuming+spawns=%#v cap=%#v plan=%#v", total, w.config.LinuxCapacity, plan.Operations)
	}
	if total.Slots > 4 {
		w.t.Fatalf("slot cap exceeded: %d slots in use+spawned", total.Slots)
	}
	// The spawns this tick must fit the free envelope the scheduler observed.
	// Pre-existing over-subscription from a host that shrank after admission is
	// not a violation, so we only gate the incremental spawns against headroom.
	free := w.freeEnvelope()
	if !free.CanFit(spawns) {
		w.t.Fatalf("spawns exceed free envelope: spawns=%#v free=%#v plan=%#v", spawns, free, plan.Operations)
	}
	for repo, add := range repoAdds {
		cap := w.config.RepoCaps[repo]
		if cap <= 0 {
			cap = 1
		}
		if w.linuxRepoCount(repo)+add > cap {
			w.t.Fatalf("repo cap exceeded for %q: existing=%d add=%d cap=%d", repo, w.linuxRepoCount(repo), add, cap)
		}
	}
	if w.enforceHostBound {
		var goForward domain.Resources
		drained := map[string]bool{}
		for _, op := range plan.Operations {
			if op.Kind == OperationDrain {
				drained[op.Instance] = true
			}
		}
		for _, inst := range w.instances {
			if inst.ConsumesHostResources() && !drained[inst.ID] {
				goForward = goForward.Add(inst.Resources)
			}
		}
		goForward = goForward.Add(spawns)
		if !w.host.Available.CanFit(goForward) {
			w.t.Fatalf("go-forward consumption %#v exceeds Host.Available %#v: plan=%#v", goForward, w.host.Available, plan.Operations)
		}
	}
}

// assertInputCanonicalization is the determinism oracle: the plan must not
// depend on the observation order of demands. Because operation and plan IDs
// are content-addressed sha256 of canonicalized input, an identical plan (IDs
// included) proves canonicalization held for this tick.
func assertInputCanonicalization(t *testing.T, in Input, plan Plan) {
	t.Helper()
	reversed := append([]domain.Demand(nil), in.Demands.Value...)
	for l, r := 0, len(reversed)-1; l < r; l, r = l+1, r-1 {
		reversed[l], reversed[r] = reversed[r], reversed[l]
	}
	shuffled := in
	shuffled.Demands = domain.Fresh(reversed, in.Now)
	if got := PlanTick(shuffled); !reflect.DeepEqual(got, plan) {
		t.Fatalf("plan depends on demand observation order:\n original=%#v\n reversed=%#v", plan, got)
	}
}

// -- Scenario drivers -------------------------------------------------------

// simOptions configures a run.
type simOptions struct {
	ticks            int
	checkDeterminism bool
	// arrive injects new demands at the start of a tick. It receives the world
	// and the tick index and appends to w.pending using w.nextJobID().
	arrive func(w *simWorld, tick int)
}

// run executes the simulation for opts.ticks ticks and returns the plan-ID
// sequence (for cross-run determinism comparison).
func (w *simWorld) run(opts simOptions) []string {
	ids := make([]string, 0, opts.ticks)
	for tick := 0; tick < opts.ticks; tick++ {
		if opts.arrive != nil {
			opts.arrive(w, tick)
		}
		plan := w.step(tick, opts.checkDeterminism)
		ids = append(ids, plan.ID)
	}
	return ids
}

// addDemand appends a demand to pending with a fresh unique job id.
func (w *simWorld) addDemand(repo string, ageTicks int, profile domain.ProfileID) domain.Demand {
	d := domain.Demand{
		Key:       domain.DemandKey{Repo: repo, RunID: 100, Attempt: 1, JobID: w.nextJobID()},
		CreatedAt: w.now.Add(-time.Duration(ageTicks) * simTickDuration),
		Profile:   profile,
		Route:     w.config.Profiles[profile].Route,
		Platform:  w.config.Profiles[profile].Platform,
		Event:     domain.EventPullRequest,
	}
	w.pending = append(w.pending, d)
	return d
}

// -- Tests: randomized safety + determinism ---------------------------------

var simLinuxProfiles = []domain.ProfileID{"small", "medium", "large"}
var simAllProfiles = []domain.ProfileID{"small", "medium", "large", "builder", "maestro"}
var simRepos = []string{"a/repo", "b/repo", "c/repo", "mac-a", "mac-b"}

// TestSimulationRandomizedSafetyAndDeterminism drives mixed macOS/Linux demand
// streams across several repos and all five profiles, asserting the safety and
// determinism oracles every tick over many seeds.

// poissonArrivals returns the shared randomized demand-arrival closure used by
// the randomized simulation suites: 0-2 new demands most ticks, front-loaded,
// macOS profiles bound to mac repos and Linux profiles to linux repos.
func poissonArrivals(rng *rand.Rand, ticks int) func(*simWorld, int) {
	return func(w *simWorld, tick int) {
		n := rng.Intn(3)
		if tick > ticks/2 {
			n = rng.Intn(2)
		}
		for i := 0; i < n; i++ {
			profile := simAllProfiles[rng.Intn(len(simAllProfiles))]
			var repo string
			if w.config.Profiles[profile].Platform == domain.PlatformMacOS {
				repo = simRepos[3+rng.Intn(2)]
			} else {
				repo = simRepos[rng.Intn(3)]
			}
			w.addDemand(repo, rng.Intn(9), profile)
		}
	}
}

func TestSimulationRandomizedSafetyAndDeterminism(t *testing.T) {
	const seeds = 32
	const ticks = 200
	for seed := int64(0); seed < seeds; seed++ {
		seed := seed
		t.Run("seed", func(t *testing.T) {
			t.Parallel()
			cfg := testConfig()
			// Give mac repos their own caps so the Linux repo-cap oracle stays
			// scoped to Linux work.
			cfg.RepoCaps["mac-a"] = 2
			cfg.RepoCaps["mac-b"] = 2
			host := domain.Host{Available: domain.Resources{CPU: 8, MemoryMB: 16_384, Slots: 4}}
			w := newSimWorld(t, cfg, host, seed)
			rng := rand.New(rand.NewSource(seed ^ 0x5eed))
			arrive := poissonArrivals(rng, ticks)
			w.run(simOptions{ticks: ticks, checkDeterminism: true, arrive: arrive})
		})
	}
}

// mixedSimConfig gives two maestros CPU room to coexist with a few small/medium
// Linux VMs (the default testConfig is CPU-walled at two maestros). Host equals
// LinuxCapacity so the go-forward Host.Available oracle is exact.
func mixedSimConfig() (Config, domain.Host) {
	cfg := testConfig()
	cfg.LinuxCapacity = domain.Resources{CPU: 12, MemoryMB: 24_576, Slots: 4}
	cfg.RepoCaps["mac-a"] = 2
	cfg.RepoCaps["mac-b"] = 2
	cfg.MixedPlatformAdmission = true
	return cfg, domain.Host{Available: domain.Resources{CPU: 12, MemoryMB: 24_576, Slots: 4}}
}

// liveSimMaestro returns a running maestro instance for seeding a full macOS
// cohort at tick zero.
func liveSimMaestro(id string) domain.Instance {
	cfg, _ := mixedSimConfig()
	return domain.Instance{ID: id, Repo: "mac-a", Platform: domain.PlatformMacOS, Profile: "maestro",
		Route: cfg.Profiles["maestro"].Route, Resources: cfg.Profiles["maestro"].Resources,
		State: domain.InstanceRunning, Power: domain.InstancePowerRunning}
}

// TestSimulationMixedRandomizedSafetyAndDeterminism is the flag-ON twin of
// TestSimulationRandomizedSafetyAndDeterminism. Every per-tick safety oracle
// (drain-safety, capacity, repo caps, slots, determinism) plus the mixed
// oracles (mac single-cohort + MaxActive + Apple two-guest ceiling, and the
// go-forward Host.Available bound) must hold on every tick over all 32 seeds.
func TestSimulationMixedRandomizedSafetyAndDeterminism(t *testing.T) {
	const seeds = 32
	const ticks = 200
	for seed := int64(0); seed < seeds; seed++ {
		seed := seed
		t.Run("seed", func(t *testing.T) {
			t.Parallel()
			cfg, host := mixedSimConfig()
			w := newSimWorld(t, cfg, host, seed)
			w.enforceHostBound = true
			rng := rand.New(rand.NewSource(seed ^ 0x5eed))
			arrive := poissonArrivals(rng, ticks)
			w.run(simOptions{ticks: ticks, checkDeterminism: true, arrive: arrive})
		})
	}
}

// TestSimulationMixedImprovesCoexistence proves the throughput win directly:
// under an identical maestro-heavy workload, mixed admission runs strictly more
// Linux work than the platform-exclusive baseline over the same horizon, and
// never fewer macOS jobs — Linux fills the idle envelope the maestros leave.
func TestSimulationMixedImprovesCoexistence(t *testing.T) {
	const ticks = 200
	arrive := func(w *simWorld, tick int) {
		// An aged maestro arrives every tick, so a maestro is always the
		// global-FIFO priority head and the cohort stays full at two. The
		// exclusive baseline therefore takes the macOS branch every tick and, with
		// the cohort already at MaxActive, admits no Linux (the bounded one-shot
		// backfill only serves aged/control-plane smallest-tier work; this Linux
		// stream is YOUNG and standard). That is the idle-envelope pathology.
		// Mixed admission must fill the ~4 CPU / 10 GiB the maestros leave free.
		w.addDemand(simRepos[3+tick%2], 8, "maestro")
		w.addDemand(simRepos[tick%3], 0, "small")
	}
	run := func(mixed bool) (w *simWorld, linux, mac int) {
		cfg, host := mixedSimConfig()
		cfg.MixedPlatformAdmission = mixed
		w = newSimWorld(t, cfg, host, 21)
		w.enforceHostBound = true
		// Seed a full, live maestro cohort so the blocked macOS-head state holds
		// from tick zero rather than warming up.
		w.instances = append(w.instances, liveSimMaestro("seed-maestro-1"), liveSimMaestro("seed-maestro-2"))
		w.run(simOptions{ticks: ticks, arrive: arrive})
		for key := range w.spawnedAt {
			if key.Repo == "mac-a" || key.Repo == "mac-b" {
				mac++
			} else {
				linux++
			}
		}
		return w, linux, mac
	}
	_, baseLinux, baseMac := run(false)
	_, mixedLinux, mixedMac := run(true)

	// Headline win: mixed admission runs substantially more Linux in the envelope
	// the maestro cohort leaves idle. The exclusive baseline starves this young
	// standard stream behind the maestro head. (The Linux stream here is
	// deliberately over the four-slot throughput ceiling, so this test measures
	// the coexistence win, not queue latency — bounded latency is proved
	// separately under a below-ceiling arrival rate.)
	if mixedLinux <= baseLinux {
		t.Fatalf("mixed admission did not increase Linux throughput: baseline=%d mixed=%d", baseLinux, mixedLinux)
	}
	// macOS is not materially reduced. Raw maestro COUNT over a fixed horizon may
	// differ by at most one from the baseline: because the macOS cohort is always
	// planned before the Linux remainder, mixed admission never denies a maestro
	// in favor of Linux — but a live Linux VM can transiently hold a slot across
	// the brief macOS teardown gap, shifting a single respawn by one tick. That
	// bounded cadence coupling is dwarfed by the Linux gain.
	if mixedMac < baseMac-1 {
		t.Fatalf("mixed admission materially reduced macOS throughput: baseline=%d mixed=%d", baseMac, mixedMac)
	}
	t.Logf("throughput: linux baseline=%d mixed=%d (+%d); macOS baseline=%d mixed=%d",
		baseLinux, mixedLinux, mixedLinux-baseLinux, baseMac, mixedMac)
}

// TestSimulationMixedBoundedAdmissionLatency is the flag-ON liveness twin: no
// feasible demand waits longer than K feasible-pending ticks. Mixed admission
// must not starve any platform.
func TestSimulationMixedBoundedAdmissionLatency(t *testing.T) {
	const K = 40
	const ticks = 200
	cfg, host := mixedSimConfig()
	w := newSimWorld(t, cfg, host, 7)
	w.enforceHostBound = true
	rng := rand.New(rand.NewSource(7))
	arrive := func(w *simWorld, tick int) {
		if tick >= ticks-K-10 {
			return
		}
		if tick%3 == 0 {
			w.addDemand(simRepos[rng.Intn(3)], rng.Intn(4), simLinuxProfiles[rng.Intn(len(simLinuxProfiles))])
		}
		if tick%7 == 0 {
			w.addDemand(simRepos[3+rng.Intn(2)], rng.Intn(4), "maestro")
		}
	}
	w.run(simOptions{ticks: ticks, arrive: arrive})
	assertNoStarvation(t, w, K)
}

// TestSimulationDeterministicAcrossRuns pins that the same seed yields an
// identical plan-ID sequence: a stronger end-to-end determinism check than the
// per-tick canonicalization oracle.
func TestSimulationDeterministicAcrossRuns(t *testing.T) {
	build := func() *simWorld {
		cfg := testConfig()
		cfg.RepoCaps["mac-a"] = 2
		host := domain.Host{Available: domain.Resources{CPU: 8, MemoryMB: 16_384, Slots: 4}}
		return newSimWorld(t, cfg, host, 1234)
	}
	arrive := func(w *simWorld, tick int) {
		if tick%2 == 0 {
			w.addDemand(simRepos[tick%3], tick%9, simLinuxProfiles[tick%3])
		}
		if tick%5 == 0 {
			w.addDemand("mac-a", 1, "maestro")
		}
	}
	first := build().run(simOptions{ticks: 120, arrive: arrive})
	second := build().run(simOptions{ticks: 120, arrive: arrive})
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("plan-ID sequence differs across identical seeds")
	}
}

// -- Tests: liveness on designed-live scenarios -----------------------------

// TestSimulationBoundedAdmissionLatency asserts that every demand that was
// feasible at some point is admitted within K ticks (measured as feasible ticks
// spent pending). K is generous relative to the design's FairnessAge.
func TestSimulationBoundedAdmissionLatency(t *testing.T) {
	// K: aging kicks in at 5 ticks; allow ample slack for fair round-robin and
	// slot contention among a handful of concurrently feasible demands.
	const K = 40
	const ticks = 200
	cfg := testConfig()
	host := domain.Host{Available: domain.Resources{CPU: 8, MemoryMB: 16_384, Slots: 4}}
	w := newSimWorld(t, cfg, host, 7)
	rng := rand.New(rand.NewSource(7))
	// Arrival rate kept below the 4-slot throughput ceiling (a runner occupies a
	// slot for its whole lifecycle plus job), so the queue stays bounded and the
	// latency oracle measures scheduling latency, not overload backlog.
	arrive := func(w *simWorld, tick int) {
		if tick >= ticks-K-10 {
			return // stop injecting so late arrivals still get their full K-tick budget
		}
		if tick%3 == 0 {
			w.addDemand(simRepos[rng.Intn(3)], rng.Intn(4), simLinuxProfiles[rng.Intn(len(simLinuxProfiles))])
		}
	}
	w.run(simOptions{ticks: ticks, arrive: arrive})
	assertNoStarvation(t, w, K)
}

// TestSimulationNoPermanentStarvationBehindRepoCappedHead is the PR #78
// regression as a liveness property: a stream of small feasible demands keeps
// being admitted even while an aged head is blocked (here by a saturated repo
// cap, the designed reservation+backfill case where free capacity remains).
func TestSimulationNoPermanentStarvationBehindRepoCappedHead(t *testing.T) {
	const ticks = 200
	cfg := testConfig()
	cfg.RepoCaps["blocked"] = 1 // the head's repo saturates immediately
	host := domain.Host{Available: domain.Resources{CPU: 8, MemoryMB: 16_384, Slots: 4}}
	w := newSimWorld(t, cfg, host, 3)
	// An aged large demand whose repo caps at 1: once one is live it is blocked
	// by cap (not resources), so its vector still fits free and backfill runs.
	w.addDemand("blocked", 30, "large")
	w.addDemand("blocked", 29, "large")
	arrive := func(w *simWorld, tick int) {
		// A steady stream of small feasible demands in other repos.
		w.addDemand(simRepos[tick%3], 6, "small")
	}
	w.run(simOptions{ticks: ticks, arrive: arrive})
	// The 4-slot throughput ceiling caps how many can run at once, so the test
	// proves liveness two ways: a healthy total, and continued admission in the
	// final quarter (the stream never latches shut behind the blocked head).
	admitted := w.spawnedCount
	lateAdmitted := spawnsSince(w, ticks*3/4)
	if admitted < ticks/5 {
		t.Fatalf("small feasible stream starved behind blocked head: only %d admitted over %d ticks", admitted, ticks)
	}
	if lateAdmitted == 0 {
		t.Fatalf("small feasible stream latched shut: no admissions in the final quarter")
	}
}

// spawnsSince counts demands spawned at or after the given tick.
func spawnsSince(w *simWorld, tick int) int {
	count := 0
	for _, at := range w.spawnedAt {
		if at >= tick {
			count++
		}
	}
	return count
}

// assertNoStarvation fails if any demand was feasible-and-pending for more than
// K ticks without being spawned.
func assertNoStarvation(t *testing.T, w *simWorld, K int) {
	t.Helper()
	var starved []domain.DemandKey
	for key, ticks := range w.feasiblePendingTicks {
		if _, spawned := w.spawnedAt[key]; spawned {
			continue
		}
		if ticks > K {
			starved = append(starved, key)
		}
	}
	sort.Slice(starved, func(i, j int) bool { return starved[i].String() < starved[j].String() })
	if len(starved) > 0 {
		t.Fatalf("starvation: %d demand(s) feasible+pending > %d ticks and never spawned, e.g. %s",
			len(starved), K, starved[0].String())
	}
}

// -- Tests: adversarial fixtures (incident topologies + suspected gaps) ------

// incident3Config builds the exact incident topology: an idle host without
// headroom for a 12 GiB macOS builder, and a feasible small/medium/maestro
// backlog behind the infeasible builder head.
func incident3Config() Config {
	cfg := testConfig()
	cfg.RepoCaps["mac-a"] = 2
	return cfg
}

// TestSimulationIncident3Topology reproduces the head-of-line incident: an aged
// infeasible macOS builder is the oldest demand on an otherwise-idle host, with
// a feasible Linux + maestro backlog behind it. After PR #78 the feasible
// backlog should drain. This runs the liveness oracle; if it fails on current
// main it is marked as a KNOWN GAP rather than changing production code.
func TestSimulationIncident3Topology(t *testing.T) {
	const ticks = 200
	const K = 40
	cfg := incident3Config()
	// Idle host with only 9 GiB free: the 12 GiB builder can never be admitted.
	host := domain.Host{Available: domain.Resources{CPU: 8, MemoryMB: 9_216, Slots: 4}}
	w := newSimWorld(t, cfg, host, 42)
	// Aged infeasible builder head (oldest).
	w.addDemand("mac-a", 60, "builder")
	// Feasible backlog behind it, all aged so they are eligible for fairness.
	for i := 0; i < 7; i++ {
		w.addDemand(simRepos[i%3], 30, simLinuxProfiles[i%len(simLinuxProfiles)])
	}
	w.addDemand("mac-b", 30, "maestro")
	w.run(simOptions{ticks: ticks})

	// Count how many of the feasible backlog demands were ever admitted.
	feasibleAdmitted := 0
	feasibleTotal := 0
	for key := range w.everFeasible {
		if key.Repo == "mac-a" { // the infeasible builder is expected to stay pending
			continue
		}
		feasibleTotal++
		if _, ok := w.spawnedAt[key]; ok {
			feasibleAdmitted++
		}
	}
	if feasibleAdmitted < feasibleTotal {
		t.Fatalf("idle-host macOS-builder head starved feasible backlog: only %d/%d feasible backlog demands admitted over %d ticks "+
			"(a resource-infeasible macOS head must not latch backfill shut behind it)",
			feasibleAdmitted, feasibleTotal, ticks)
	}
	assertNoStarvation(t, w, K)
}

// TestSimulationTwoAgedInfeasibleDemands probes the suspected residual gap that
// State.Reservation is singular: a SECOND simultaneously-aged infeasible demand
// has no reservation of its own. It asserts the feasible backlog still drains.
func TestSimulationTwoAgedInfeasibleDemands(t *testing.T) {
	const ticks = 200
	const K = 40
	cfg := testConfig()
	// Host fits neither large (8 GiB) reserved head this tick but leaves room
	// for smalls; two aged large demands in two repos capped at 1 each are the
	// two infeasible-by-cap heads competing for the single reservation slot.
	cfg.RepoCaps["big1"] = 1
	cfg.RepoCaps["big2"] = 1
	host := domain.Host{Available: domain.Resources{CPU: 8, MemoryMB: 16_384, Slots: 4}}
	w := newSimWorld(t, cfg, host, 99)
	// Two aged large demands, then two more of the same repos so the repos stay
	// saturated (one live large caps the repo, the second is infeasible-by-cap
	// and wants a reservation, but only one reservation slot exists).
	w.addDemand("big1", 40, "large")
	w.addDemand("big2", 40, "large")
	w.addDemand("big1", 39, "large")
	w.addDemand("big2", 39, "large")
	arrive := func(w *simWorld, tick int) {
		w.addDemand(simRepos[tick%3], 6, "small")
	}
	w.run(simOptions{ticks: ticks, arrive: arrive})
	// FINDING: the suspected "second aged infeasible demand has no reservation"
	// gap did NOT reproduce as starvation. planLinux reserves only the FIFO head
	// and backfills feasible work in the reserved vector's remainder; the second
	// infeasible demand simply waits its turn without a reservation and does not
	// block that backfill. The small stream keeps flowing at the throughput
	// ceiling, and the two heads are served FIFO as their repo caps free up.
	admitted := w.spawnedCount
	lateAdmitted := spawnsSince(w, ticks*3/4)
	if admitted < ticks/5 || lateAdmitted == 0 {
		t.Fatalf("unexpected starvation with two aged infeasible heads: total=%d late=%d", admitted, lateAdmitted)
	}
	assertNoStarvation(t, w, K)
}

// TestSimulationPriorityInversionExactSelect probes the suspected latent
// priority inversion: exactSelect maximizes selection COUNT, so several small
// young demands can be admitted ahead of a higher-priority (control-plane) large
// young demand that fits. The oracle asserts the high-priority demand is not
// deferred behind lower-priority work in the first admitting tick.
func TestSimulationPriorityInversionExactSelect(t *testing.T) {
	cfg := testConfig()
	cfg.RepoCaps["hp/control"] = 1
	cfg.RepoCaps["lp/small"] = 4
	setRepoSchedulingClass(t, &cfg, "hp/control", controlPlaneClass)
	// CPU-bound capacity: the large control-plane job (4 CPU) consumes the whole
	// CPU budget alone, whereas four smalls (1 CPU each) also fit. exactSelect
	// maximizes selection COUNT, so it can prefer the four smalls (count 4) over
	// the single higher-priority large (count 1) — the latent inversion.
	cfg.LinuxCapacity = domain.Resources{CPU: 4, MemoryMB: 16_384, Slots: 4}
	host := domain.Host{Available: cfg.LinuxCapacity}
	w := newSimWorld(t, cfg, host, 5)
	// One high-priority large control-plane job, young.
	hp := w.addDemand("hp/control", 1, "large")
	// Four low-priority small jobs, young.
	for i := 0; i < 4; i++ {
		w.addDemand("lp/small", 1, "small")
	}
	in := w.planInput()
	plan := PlanTick(in)
	w.assertSafety(plan)

	hpSpawned := false
	for _, op := range plan.Operations {
		if op.Kind == OperationSpawn && op.Demand == hp.Key {
			hpSpawned = true
		}
	}
	// The control-plane head must be admitted in the first admitting tick rather
	// than deferred behind a larger count of lower-priority standard work.
	// exactSelect must not invert priority to maximize selection COUNT.
	if containsSpawn(plan.Operations) && !hpSpawned {
		t.Fatalf("priority inversion: exactSelect admitted lower-priority small jobs (%d spawns) while deferring the "+
			"higher-priority control-plane large job %s that also fits; exact admission must honor priority over raw count",
			len(spawnedKeys(plan)), hp.Key.String())
	}
	if !hpSpawned {
		t.Fatalf("expected the control-plane head admitted; got plan %#v", plan.Operations)
	}
}

// TestSimulationShrinkingAndGrowingHost asserts every safety oracle holds while
// host memory shrinks below current occupancy and later recovers. The scheduler
// must never spawn beyond the (possibly shrunken) headroom, and must never drain
// a busy instance to make room.
func TestSimulationShrinkingAndGrowingHost(t *testing.T) {
	const ticks = 150
	cfg := testConfig()
	w := newSimWorld(t, cfg, domain.Host{Available: domain.Resources{CPU: 8, MemoryMB: 16_384, Slots: 4}}, 11)
	arrive := func(w *simWorld, tick int) {
		// Oscillate host memory: shrink toward a single small VM, then grow back.
		phase := tick % 40
		mem := 16_384
		switch {
		case phase < 10:
			mem = 4_096
		case phase < 20:
			mem = 2_048
		case phase < 30:
			mem = 8_192
		}
		w.host = domain.Host{Available: domain.Resources{CPU: 8, MemoryMB: mem, Slots: 4}}
		if tick%2 == 0 {
			w.addDemand(simRepos[tick%3], tick%9, simLinuxProfiles[tick%len(simLinuxProfiles)])
		}
	}
	w.run(simOptions{ticks: ticks, checkDeterminism: true, arrive: arrive})
}

// TestSimulationStalledAssignmentZombieIsReclaimed is the liveness oracle for
// the 84-minute incident: an assigned runner whose job never starts must not
// live forever. It occupies a slot and 4 GiB the whole time, so left unbounded
// it starves the queue behind it. The oracle asserts (1) the zombie is
// reclaimed within a bounded window after the assignment deadline, and (2) work
// queued behind it is still admitted — while assertDrainSafety runs every tick,
// proving no genuinely busy instance is ever caught by the deadline.
func TestSimulationStalledAssignmentZombieIsReclaimed(t *testing.T) {
	cfg := testConfig() // AssignedTimeout = 15m; simTickDuration = 1m => 15 ticks.
	deadlineTicks := int(cfg.AssignedTimeout / simTickDuration)
	// One free slot only, so the zombie genuinely blocks the queue behind it and
	// reclaim is the sole way the pending demand can ever run.
	host := domain.Host{Available: domain.Resources{CPU: 8, MemoryMB: 16_384, Slots: 1}}
	w := newSimWorld(t, cfg, host, 3)
	zombie := domain.Instance{ID: "vm-zombie", Repo: "a/repo", Platform: domain.PlatformLinux, Profile: "medium",
		Route: cfg.Profiles["medium"].Route, Resources: cfg.Profiles["medium"].Resources,
		State: domain.InstanceAssigned, Power: domain.InstancePowerRunning, AssignedSince: w.now}
	w.instances = append(w.instances, zombie)
	w.stuck[zombie.ID] = true
	behind := w.addDemand("b/repo", 0, "small")

	reclaimedAt, admittedAt := -1, -1
	for tick := 0; tick < deadlineTicks+30; tick++ {
		w.step(tick, false) // assertSafety (incl. drain-safety) runs inside every step
		present := false
		for _, inst := range w.instances {
			if inst.ID == zombie.ID {
				present = true
			}
			if inst.Repo == behind.Key.Repo && admittedAt < 0 {
				admittedAt = tick
			}
		}
		if !present && reclaimedAt < 0 {
			reclaimedAt = tick
		}
	}
	if reclaimedAt < 0 {
		t.Fatal("stalled zombie was never reclaimed; it would have starved the fleet indefinitely")
	}
	if reclaimedAt < deadlineTicks {
		t.Fatalf("zombie reclaimed at tick %d, before the deadline of %d ticks", reclaimedAt, deadlineTicks)
	}
	if admittedAt < 0 {
		t.Fatalf("the demand queued behind the zombie was never admitted after reclaim at tick %d", reclaimedAt)
	}
}

// TestSimulationLingeringRunnerIsReclaimed is the liveness oracle for tonight's
// 74-minute incident: a cancelled run left a runner stuck in Running with no
// active job, occupying the only free slot so a full-width profile queued
// behind it could never be admitted (the CPU=8 builder starvation chain). Left
// unbounded the fleet starves indefinitely. The oracle asserts (1) the lingerer
// is reclaimed within a bounded window after the idle-runner deadline, and (2)
// the work queued behind it is then admitted — while assertDrainSafety runs
// every tick, proving no runner with an active job is ever caught by the deadline.
func TestSimulationLingeringRunnerIsReclaimed(t *testing.T) {
	cfg := testConfig() // AssignedTimeout = 15m; simTickDuration = 1m => 15 ticks.
	deadlineTicks := int(cfg.AssignedTimeout / simTickDuration)
	// One free slot only, so the lingerer genuinely blocks the queue behind it
	// and reclaim is the sole way the pending demand can ever run.
	host := domain.Host{Available: domain.Resources{CPU: 8, MemoryMB: 16_384, Slots: 1}}
	w := newSimWorld(t, cfg, host, 5)
	lingerer := domain.Instance{ID: "vm-lingerer", Repo: "a/repo", Platform: domain.PlatformLinux, Profile: "small",
		Route: cfg.Profiles["small"].Route, Resources: cfg.Profiles["small"].Resources,
		State: domain.InstanceRunning, Power: domain.InstancePowerRunning, JobInactive: true, RunningSince: w.now}
	w.instances = append(w.instances, lingerer)
	w.lingering[lingerer.ID] = true
	behind := w.addDemand("b/repo", 0, "small")

	reclaimedAt, admittedAt := -1, -1
	for tick := 0; tick < deadlineTicks+30; tick++ {
		w.step(tick, false) // assertSafety (incl. drain-safety) runs inside every step
		present := false
		for _, inst := range w.instances {
			if inst.ID == lingerer.ID {
				present = true
			}
			if inst.Repo == behind.Key.Repo && admittedAt < 0 {
				admittedAt = tick
			}
		}
		if !present && reclaimedAt < 0 {
			reclaimedAt = tick
		}
	}
	if reclaimedAt < 0 {
		t.Fatal("lingering runner was never reclaimed; it would have starved the fleet indefinitely")
	}
	if reclaimedAt < deadlineTicks {
		t.Fatalf("lingerer reclaimed at tick %d, before the deadline of %d ticks", reclaimedAt, deadlineTicks)
	}
	if admittedAt < 0 {
		t.Fatalf("the demand queued behind the lingerer was never admitted after reclaim at tick %d", reclaimedAt)
	}
}
