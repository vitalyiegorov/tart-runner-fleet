// Package simulation_test is the deterministic simulation testing (DST) harness
// described by ADR 0031.
//
// It runs the REAL control plane -- scheduler.PlanTick, reconcile.Controller
// .Commit, and a real SQLite store's ApplyPlan/inbox/lifecycle writes -- inside a
// simulated single-host fleet whose every source of nondeterminism (GitHub
// scale-set broker delivery, REST inventory freshness, host tenant load, VM boot
// latency, job duration) is driven by a seeded event trace and a virtual clock.
//
// Every run is reproducible from (seed, config); the same trace replayed twice
// produces the same observations, so a failure shrinks to a minimal event trace
// instead of a screenshot of a bad night.
//
// The harness is deliberately a test-only package under tests/, exactly like
// tests/replay and tests/chaos: scripts/check-coverage.sh instruments each
// package's own statements, and a package with no non-test files contributes
// none, so the simulator carries the 99% gate for the production code it drives
// rather than for its own scaffolding. scripts/check-cpd.sh likewise scans only
// cmd and internal.
package simulation_test

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/sqlite"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/executor"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/lifecycle"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/reconcile"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

// simEpoch is the virtual wall clock the simulation starts from. It is a fixed
// constant so a (seed, config) pair names one exact history.
var simEpoch = time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)

// simTick is one control-plane reconciliation. Thirty virtual seconds keeps the
// production ratios legible: FairnessAge 5m is ten ticks, the statistics
// freshness budget is four, and the ghost-absence window is thirty.
const simTick = 30 * time.Second

// simControlPlaneRepo is the fleet's own repository. ADR 0004 gives it the
// control-plane scheduling class, so the simulator carries one.
const simControlPlaneRepo = "ops/fleet"

// simOwner runs the executor and the controller. One writer, one goroutine: the
// harness is sequential by construction so that any lost compare-and-set is a
// genuine composition defect rather than a modelled race.
const simOwner = "sim-controller"

// worldConfig is the parameterized host envelope and policy under simulation.
// The defaults are the production Mac mini (Apple M4, 10 cores, 24 GiB).
type worldConfig struct {
	Name             string
	PhysicalCPU      int64
	PhysicalMemoryMB int64
	Guards           executor.Guardrails
	Scheduler        scheduler.Config
	Bindings         []app.Binding
	Repos            []string
	Profiles         []domain.ProfileID
	// LivenessK bounds property (a): a feasible demand must reach admission
	// within this many ticks.
	LivenessK int
	// StarvationN bounds property (b): an aged feasible demand may be passed over
	// by younger work at most this many times.
	StarvationN int
	// QuiesceQ bounds property (f): after the demand stream stops the fleet must
	// be empty within this many ticks.
	QuiesceQ int
	// StrandedG bounds property (h): a demand whose own instance is executing a
	// DIFFERENT job may stay in that state at most this many ticks. It is a
	// delivery budget, not a tolerance — the broker message that names the real
	// assignment may be delayed by up to six ticks, and the fleet cannot act on
	// evidence it has not received.
	StrandedG int
	// DrainChurnN bounds property (i): an instance may have at most this many
	// recovery drains aborted by execution-time evidence. One is a genuine race
	// between planning and execution; a second is a loop, because aborting
	// restarts the very deadline that planned it.
	DrainChurnN int
	// HearingH bounds property (j): a job the broker has actually delivered a
	// JobAvailable for must be in the fleet's durable ledger within this many
	// ticks. It is a delivery budget, not a scheduling one -- ingestion is
	// synchronous with delivery, so anything beyond one tick of slack means the
	// binding refused evidence it was handed (issue #165).
	HearingH int
	// OccupancyGraceTicks bounds property (k): an instance that has reached its
	// profile's occupancy budget may go on holding its vector for at most this
	// many further ticks. It is a RECLAIM budget, not a tolerance — the breach has
	// to be observed, the plan has to commit, and the drain has to reach the guest
	// stop that actually releases the vector, and each of those is a tick the
	// fleet cannot skip.
	OccupancyGraceTicks int
	// Priority is the demand priority policy this world declares (issue #224).
	// The zero policy is every world that predates it: one default tier and aged
	// FIFO, with every tier oracle inert.
	Priority domain.PriorityPolicy
	// TierStarvationT bounds property (n): a feasible demand may be passed over
	// on priority-tier grounds for at most this many ticks, because escalation
	// must end the exemption a declared tier earns from property (b).
	TierStarvationT int
	// TeardownReleaseTicks bounds property (o): once a drain has passed
	// deregistration — the runner is gone and the job is provably over — the
	// instance may go on holding its vector for at most this many further ticks,
	// WHATEVER the guest does.
	//
	// Twelve is the ladder plus slack. Production climbs three graceful stops,
	// three forced ones and three destroys; the harness spends one tick per
	// attempt, and the teardown needs two more edges after the guest is off. The
	// worst honest case is therefore eight ticks, and twelve leaves room for the
	// observation delays a stale host probe or an unavailable Tart can stack.
	// The leak it exists to catch is unbounded: the 2026-08-10 incident ran 165
	// ticks' worth of virtual time and was still climbing.
	TeardownReleaseTicks int
	// GuestDeathReleaseTicks bounds property (p): once an instance's guest has
	// stopped executing, the instance may go on holding its vector for at most
	// this many further ticks.
	//
	// Its clock starts at the DEATH, not at the fleet's first suspicion, which is
	// what makes it an honest bound rather than a restatement of the mechanism.
	// The budget it has to cover: three probes to reach the verdict, one tick to
	// observe and plan it, one to claim the operation, one drain edge to stop the
	// guest, up to nine more if that guest also refuses to power down, and two to
	// finish the teardown -- plus whatever a stale host probe, an unavailable Tart
	// enumeration or a wedged drain stacks on top. Twenty-four ticks is twelve
	// virtual minutes. The leak it exists to catch is not merely larger: it is
	// unbounded by the fleet, and ended only because GitHub gave up sixteen to
	// eighteen minutes in.
	GuestDeathReleaseTicks int
	// SequenceResetAt is the tick at which GitHub restarts the broker's
	// message-id sequence, as it did for scale set 8077185082566234948 on
	// 2026-08-01T18:32Z. Zero means the sequence is never restarted.
	SequenceResetAt int
}

// simOccupancyBudget is the per-profile ceiling on how long ONE instance may
// hold its resource vector (issue #223). Every simulated profile declares the
// same one, and it is deliberately far above anything a HEALTHY job can reach:
// a job runs simJobTicks (6) plus at most eventLongJob's 6*6, so the longest
// legitimate job in this world is 42 ticks -- 21 virtual minutes -- and a boot
// adds five more. Sixty virtual minutes is 120 ticks, nearly three times that,
// so no long suite, slow boot, or delayed message can ever reach the ceiling.
//
// Only eventOverrunJob can, by construction: it adds a whole budget per Count.
// That is the harness equivalent of the guard test -- the budget must be
// unreachable by anything except the condition it exists to reclaim, or a green
// sweep would prove only that the ceiling is loose.
const simOccupancyBudget = 60 * time.Minute

// simGuestLiveness is the bound every simulated node judges a silent guest
// against: three consecutive REFUSED probes spanning at least sixty virtual
// seconds. At one probe per tick that is a verdict on the third refusal, ninety
// virtual seconds after the guest died.
//
// It is deliberately the tightest bound configuration permits rather than the
// shipped default, for the same reason simOccupancyBudget is deliberately loose:
// the sweep must be able to REACH the mechanism within a trace, while nothing a
// healthy guest does may satisfy it. Nothing can, by construction -- only a
// refused transport counts, and only eventSilentGuest produces one.
var simGuestLiveness = domain.GuestLivenessPolicy{ConsecutiveRefusals: 3, Window: time.Minute}

func simProfiles() map[domain.ProfileID]domain.Profile {
	return map[domain.ProfileID]domain.Profile{
		"small":   {ID: "small", Platform: domain.PlatformLinux, Route: "linux-small", Resources: domain.Resources{CPU: 1, MemoryMB: 2_048, Slots: 1}, OccupancyBudget: simOccupancyBudget},
		"medium":  {ID: "medium", Platform: domain.PlatformLinux, Route: "linux-medium", Resources: domain.Resources{CPU: 2, MemoryMB: 4_096, Slots: 1}, OccupancyBudget: simOccupancyBudget},
		"large":   {ID: "large", Platform: domain.PlatformLinux, Route: "linux-large", Resources: domain.Resources{CPU: 4, MemoryMB: 8_192, Slots: 1}, OccupancyBudget: simOccupancyBudget},
		"xl":      {ID: "xl", Platform: domain.PlatformLinux, Route: "linux-xl", Resources: domain.Resources{CPU: 6, MemoryMB: 12_288, Slots: 1}, OccupancyBudget: simOccupancyBudget},
		"builder": {ID: "builder", Platform: domain.PlatformMacOS, Route: "macos-builder", Resources: domain.Resources{CPU: 6, MemoryMB: 12_288, Slots: 1}, MaxActive: 1, OccupancyBudget: simOccupancyBudget},
		"maestro": {ID: "maestro", Platform: domain.PlatformMacOS, Route: "macos-maestro", Resources: domain.Resources{CPU: 4, MemoryMB: 7_168, Slots: 1}, MaxActive: 2, OccupancyBudget: simOccupancyBudget},
	}
}

// simBindings maps one scale set to each profile, exactly as the production
// configuration does. Scale-set identifiers are stable so a pinned trace names
// the same queues on every replay.
func simBindings(profiles map[domain.ProfileID]domain.Profile) []app.Binding {
	ids := sortedProfileIDs(profiles)
	bindings := make([]app.Binding, 0, len(ids))
	for index, id := range ids {
		profile := profiles[id]
		bindings = append(bindings, app.Binding{
			StoreKey: int64(index + 1), ScaleSetID: int64(index + 1), Scope: "sim/" + string(id),
			ScaleSetLabels: []string{"self-hosted", string(profile.Route)}, Profile: profile,
		})
	}
	return bindings
}

func sortedProfileIDs(profiles map[domain.ProfileID]domain.Profile) []domain.ProfileID {
	ids := make([]domain.ProfileID, 0, len(profiles))
	for id := range profiles {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// defaultWorld is the production-shaped envelope: an elastic 10-core / 24 GiB
// Apple M4 shared with its own tenant, mixed platform and profile admission on,
// four repositories with distinct caps, and the fleet repository in the
// control-plane scheduling class.
func defaultWorld() worldConfig {
	profiles := simProfiles()
	return worldConfig{
		Name:             "m4-mac-mini",
		PhysicalCPU:      10,
		PhysicalMemoryMB: 24_576,
		Guards: executor.Guardrails{MinFreeDiskGB: 20, MinAvailableMemoryMB: 2_048,
			MaxSwapUsedMB: 8_192, MaxLoadAverage: 24, MinCPUidlePercent: 5},
		Scheduler: scheduler.Config{
			LinuxCapacity:   domain.Resources{CPU: 10, MemoryMB: 24_576, Slots: 4},
			FairnessAge:     5 * time.Minute,
			AssignedTimeout: 15 * time.Minute,
			RepoCaps: map[string]int{
				"a/repo": 4, "b/repo": 4, "c/repo": 2, simControlPlaneRepo: 2,
			},
			RepoSchedulingClasses:  map[string]domain.SchedulingClass{simControlPlaneRepo: domain.SchedulingControlPlane},
			Profiles:               profiles,
			MixedPlatformAdmission: true,
			MixedProfileCohorts:    true,
			ElasticHostEnvelope:    true,
			GuestLiveness:          simGuestLiveness,
		},
		Bindings:               simBindings(profiles),
		Repos:                  []string{"a/repo", "b/repo", "c/repo", simControlPlaneRepo},
		Profiles:               sortedProfileIDs(profiles),
		LivenessK:              12,
		StarvationN:            3,
		QuiesceQ:               40,
		StrandedG:              10,
		DrainChurnN:            1,
		HearingH:               2,
		OccupancyGraceTicks:    12,
		TeardownReleaseTicks:   12,
		GuestDeathReleaseTicks: 24,
	}
}

// budgetedWorld is issue #136's node: the shared Mac Studio, a machine much
// larger than the share the fleet is allowed on it, serving maestro overflow
// inside an explicit 4-vCPU / 10 GiB ceiling. It exists because the pressure
// guardrails cannot express that promise -- they narrow admission as the host's
// other tenant gets busy, and on a quiet host they let the fleet take everything.
//
// The shape is deliberately tight. One maestro (4 vCPU, 7 GiB) is exactly the
// budget's CPU dimension, so a second can never coexist however idle the machine
// is and however much work is queued; that makes every admission in this world a
// statement about the budget rather than about supply.
func budgetedWorld() worldConfig {
	cfg := defaultWorld()
	cfg.Name = "mac-studio-4x10-budget"
	cfg.Scheduler.HostBudget = domain.Resources{CPU: 4, MemoryMB: 10_240}
	profiles := map[domain.ProfileID]domain.Profile{"maestro": simProfiles()["maestro"]}
	cfg.Scheduler.Profiles = profiles
	cfg.Bindings = simBindings(profiles)
	cfg.Profiles = sortedProfileIDs(profiles)
	return cfg
}

// containerNodeWorld is issue #139's node: the x86 Linux machine of ADR 0034 §3,
// serving every Linux profile and no macOS profile at all, on twelve cores and
// 32 GiB.
//
// It is the "single-platform configuration" docs/MULTI_NODE_PLAN.md reserves for
// chunk 2d, and it adds no dimension to the harness -- the world config already
// expresses it. What it exercises is the arrangement the two-platform admission
// logic of ADR 0012, 0014, 0024 and 0029 is simply not present for: with
// `macosBurst.enabled: false` there is no cohort to drain and switch, no
// reservation held for a platform that cannot run here, and every property the
// oracles check must hold on the `planLinux` path alone.
//
// The executor is irrelevant to it, which is the point. ADR 0031 says the
// simulated world is one node, and under ADR 0034 one node is one whole fleet;
// whether that node's guests are Tart VMs or Podman containers is below the seam
// the simulation models.
func containerNodeWorld() worldConfig {
	cfg := defaultWorld()
	cfg.Name = "geekom-linux-amd64"
	cfg.PhysicalCPU = 12
	cfg.PhysicalMemoryMB = 32_768
	cfg.Guards.MaxLoadAverage = 28
	cfg.Scheduler.LinuxCapacity = domain.Resources{CPU: 12, MemoryMB: 32_768, Slots: 4}
	// A node that cannot run a macOS guest declares none, so the mixed-platform
	// and mixed-profile settings describe a case that can no longer occur.
	cfg.Scheduler.MixedPlatformAdmission = false
	cfg.Scheduler.MixedProfileCohorts = false
	profiles := map[domain.ProfileID]domain.Profile{}
	for id, profile := range simProfiles() {
		if profile.Platform == domain.PlatformLinux {
			profiles[id] = profile
		}
	}
	cfg.Scheduler.Profiles = profiles
	cfg.Bindings = simBindings(profiles)
	cfg.Profiles = sortedProfileIDs(profiles)
	return cfg
}

// simFederatedScope is the one GitHub scope two nodes serve together. It is a
// single scope name on purpose: the shared label lives inside one registration
// boundary, and that is what makes one queued job visible to both scale sets.
const simFederatedScope = "sudoku-repo"

// federatedWorld is the topology ADR 0034 gained when the #144 spike proved
// GitHub distributes work across two identically-labelled scale sets in one
// scope: node A's `maestro` set and node C's `maestro` set, same scope, same
// profile, byte-identical labels, different durable identities.
//
// ADR 0031 says the simulated world is ONE node, and that is still true here.
// What this world models is not two hosts but one SCOPE, observed the way both
// nodes observe it: the REST inventory poll returns the scope's whole queue, and
// every job in it matches both sets by label. The second binding stands in for
// the peer's scale set as it appears in that shared view. Its work executes on
// the same simulated host, which overstates the load and understates nothing --
// and it is exactly the arrangement in which an unbounded REST lane counts one
// queued job twice and lets two sets derive demand for it (issue #153).
//
// Placement is GitHub's, not the fleet's: simGitHubPlacement fills the set with
// the most room, which is the rule the spike measured.
func federatedWorld() worldConfig {
	cfg := defaultWorld()
	cfg.Name = "federated-maestro-scope"
	profile := simProfiles()["maestro"]
	profiles := map[domain.ProfileID]domain.Profile{"maestro": profile}
	cfg.Scheduler.Profiles = profiles
	cfg.Profiles = sortedProfileIDs(profiles)
	labels := []string{"self-hosted", string(profile.Route)}
	cfg.Bindings = []app.Binding{
		{StoreKey: 1, ScaleSetID: 1, Scope: simFederatedScope, ScaleSetLabels: labels,
			Profile: profile, SharedLabels: true},
		{StoreKey: 2, ScaleSetID: 2, Scope: simFederatedScope, ScaleSetLabels: labels,
			Profile: profile, SharedLabels: true},
	}
	return cfg
}

// hostCeiling is the largest total the fleet may ever hold on this host, derived
// from configured facts alone so the property oracles never borrow the
// scheduler's own envelope arithmetic. It is the physical machine net of the
// memory reserve, narrowed by an explicit host budget where one is configured.
// An unset budget dimension imposes no bound, matching the production rule that
// an unobserved or undeclared total is not a measurement of zero.
func (c worldConfig) hostCeiling() domain.Resources {
	ceiling := domain.Resources{CPU: int(c.PhysicalCPU),
		MemoryMB: int(c.PhysicalMemoryMB - c.Guards.MinAvailableMemoryMB), Slots: c.Scheduler.LinuxCapacity.Slots}
	if budget := c.Scheduler.HostBudget; budget.CPU > 0 {
		ceiling.CPU = min(ceiling.CPU, budget.CPU)
	}
	if budget := c.Scheduler.HostBudget; budget.MemoryMB > 0 {
		ceiling.MemoryMB = min(ceiling.MemoryMB, budget.MemoryMB)
	}
	return ceiling
}

// ceilingSource names what a conservation violation was measured against, so a
// finding on a budgeted node reads as the broken promise it is rather than as an
// impossible claim about the hardware.
func (c worldConfig) ceilingSource() string {
	if c.Scheduler.HostBudget == (domain.Resources{}) {
		return "the physical host"
	}
	return "the configured host budget"
}

// ---------------------------------------------------------------------------
// Simulated host
// ---------------------------------------------------------------------------

// simHost is the host probe. It reports the physical machine, what the fleet's
// own VMs hold, and what the host's other tenant holds, so the elastic envelope
// of ADR 0018 is exercised against a machine that really does get busy.
type simHost struct{ world *world }

func (h simHost) Snapshot(context.Context) executor.HostSnapshot {
	w := h.world
	usedCPU, usedMemory := w.fleetOccupancy()
	idle := float64(w.cfg.PhysicalCPU-int64(usedCPU)-int64(w.tenantCPU)) / float64(w.cfg.PhysicalCPU) * 100
	if idle < 0 {
		idle = 0
	}
	available := w.cfg.PhysicalMemoryMB - int64(usedMemory) - int64(w.tenantMemoryMB)
	if available < 0 {
		available = 0
	}
	if w.hostProbeStale > 0 {
		return executor.HostSnapshot{Freshness: executor.Stale, ObservedAt: w.now}
	}
	return executor.HostSnapshot{
		Freshness: executor.Fresh, ObservedAt: w.now, AvailableMemoryMB: available, FreeDiskGB: 400,
		SwapUsedMB: 0, SwapOuts: 0, CPUidlePercent: idle, LoadAverage: float64(usedCPU + w.tenantCPU),
		SwapOutRatePerSecond: 0, SwapOutRateObserved: true,
		PhysicalCPU: w.cfg.PhysicalCPU, PhysicalMemoryMB: w.cfg.PhysicalMemoryMB,
	}
}

// simTart is the VM enumeration. It answers from the simulated hypervisor, so
// an absent or powered-off VM is a fact the real inventory adapter classifies.
type simTart struct{ world *world }

func (a simTart) List(context.Context) ([]executor.Instance, error) {
	w := a.world
	if w.tartUnavailable > 0 {
		return nil, fmt.Errorf("simulated tart failure")
	}
	names := make([]string, 0, len(w.vms))
	for name := range w.vms {
		names = append(names, name)
	}
	sort.Strings(names)
	vms := make([]executor.Instance, 0, len(names))
	for _, name := range names {
		// The misreport is applied HERE and nowhere else, because it is a fault of
		// the reading rather than of the machine. w.vms stays true, so the guest goes
		// on executing and its job goes on progressing, and the drain executor's own
		// re-read below contradicts this one — which is exactly the disagreement
		// issue #246 is about, and the reason it cannot be expressed by powering a
		// VM off.
		running := w.vms[name]
		if running && w.powerMisreport[name] > 0 {
			running = false
		}
		vms = append(vms, executor.Instance{Name: name, Running: running})
	}
	return vms, nil
}

// simRecovery answers the two destructive-recovery evidence questions from the
// broker's own truth, which is what makes a stalled assignment or a lingering
// runner reachable in simulation.
type simRecovery struct{ world *world }

func (r simRecovery) ConfirmDeletion(_ context.Context, name string) (operations.DeletionConfirmation, error) {
	inactive := !r.world.runnerBusy(name)
	return operations.DeletionConfirmation{Fresh: true, RunnerInactive: inactive, JobsInactive: inactive,
		ObservedAt: r.world.now}, nil
}

func (r simRecovery) JobActive(_ context.Context, instance operations.Instance) (bool, error) {
	job := r.world.jobByRequest(instance.Demand.JobID)
	return job != nil && job.status == jobRunning, nil
}

// simGuestProbe is the node's guest-liveness probe. It answers from the
// simulator's own physics rather than from anything the fleet has learned,
// because the discrimination under test IS the classification: a dead guest
// refuses the transport, a saturated one cannot answer inside the deadline, and
// a healthy one runs the command.
type simGuestProbe struct{ world *world }

func (p simGuestProbe) Probe(_ context.Context, name string) domain.GuestLiveness {
	return p.world.guestLiveness(name)
}

// guestLiveness is the simulated hypervisor's answer to `exec <instance> true`,
// and it is the ONE place the harness decides what a guest is doing. Both the
// probe port and the drain mirror read it, so they cannot disagree about a guest
// the way they did before 2026-08-17.
//
// The powered-off case is the one that matters and the one that was missing. A VM
// that is not running executes nothing, so the command cannot succeed; it fails
// at once, against the control socket rather than against the probe's deadline,
// and daemon.execGuestProbe classifies exactly that as Refused. Reporting such a
// guest ALIVE is not a conservative approximation, it is the one answer the real
// probe can never give, and it is load-bearing here: the guest-liveness reclaim
// powers the guest off before it deregisters and CLEARS silentGuest when it does,
// so a probe that read only that flag declared every reclaimed guest healthy one
// tick after killing it. The drain then aborted back to Running holding a vector
// domain.ConsumesHostResources had already released to a replacement spawn, which
// is the conservation violation the nightly sweep reported on three arms.
func (w *world) guestLiveness(name string) domain.GuestLiveness {
	if w.silentGuest[name] {
		return domain.GuestLivenessRefused
	}
	if running, exists := w.vms[name]; !exists || !running {
		return domain.GuestLivenessRefused
	}
	if w.saturatedGuest[name] {
		return domain.GuestLivenessUnknown
	}
	return domain.GuestLivenessAlive
}

// simInstances is the durable instance reader with two substitutions, and they
// are the ONLY places this harness rewrites a durable fact (ADR 0031).
//
// UpdatedAt is rewritten to the VIRTUAL instant the row entered its current
// state. The store stamps updated_at from the process wall clock, and
// app.ProductionInventory derives AssignedSince and RunningSince from it. Under
// a virtual clock every row would therefore look newborn and no assignment or
// idle-runner deadline could ever be reached, hiding exactly the recovery paths
// ADR 0028 was written about.
//
// CreatedAt is rewritten to the VIRTUAL instant the world first held the row.
// app.ProductionInventory derives OccupiedSince from created_at — the one
// per-instance age that must survive a state change (ADR 0036) — so the
// occupancy clock is the third durable timestamp this harness's virtual clock
// has to be able to move.
//
// It is worth being exact about why, because the reason is NOT the same as
// UpdatedAt's. sqlite.Advance stamps updated_at from time.Now, so without the
// first substitution no deadline could ever be reached. created_at is stamped by
// sqlite.ApplyPlan from plan.CreatedAt, which reconcile.Controller takes from the
// injected clock, so today it is already virtual and the budget would fire
// without this line. That is an implementation detail of one INSERT, not a
// contract: the sibling write in the same file reaches for the wall clock, and
// if created_at ever followed it the occupancy budget would become unreachable
// in simulation, property (k) would go permanently green, and nothing in this
// package would say so. Stating the harness's own clock here makes the property
// independent of that choice.
//
// It also makes the harness's ground truth and the production derivation the
// SAME number, which matters because property (k) deliberately measures
// occupancy from w.createdAt rather than from OccupiedSince: the oracle and the
// scheduler must be able to disagree about the CONCLUSION without disagreeing
// about the clock. Nothing else about either row is touched.
type simInstances struct{ world *world }

func (s simInstances) LiveInstances(ctx context.Context) ([]operations.Instance, error) {
	instances, err := s.world.store.LiveInstances(ctx)
	if err != nil {
		return nil, err
	}
	for index := range instances {
		s.world.noteCreated(instances[index].ID)
		if since, ok := s.world.enteredAt[stateKey(instances[index])]; ok {
			instances[index].UpdatedAt = since
		}
		instances[index].CreatedAt = s.world.createdAt[instances[index].ID]
	}
	return instances, nil
}

func stateKey(instance operations.Instance) string {
	return instance.ID + "\x00" + string(instance.State)
}

// noteCreated records the virtual instant an instance row came into existence,
// the first time the world sees it. Two callers race to be first and both are
// legitimate: executeOperations claims the provisioning operation on the very
// tick the plan created the row, and the inventory read observes it on the tick
// after. The earliest sighting wins, so the recorded instant is the tick the
// vector actually started being held.
func (w *world) noteCreated(id string) {
	if _, known := w.createdAt[id]; !known {
		w.createdAt[id] = w.now
	}
}

// ---------------------------------------------------------------------------
// The world
// ---------------------------------------------------------------------------

type world struct {
	ctx   context.Context
	cfg   worldConfig
	trace simTrace

	store  *sqlite.Store
	engine app.Engine
	demand app.DemandCoordinator

	now  time.Time
	tick int

	// jobs is the broker's own truth about every workflow job it has ever
	// advertised, independent of what the fleet has durably learned.
	jobs      []*simJob
	nextRunID int64
	nextReq   int64
	nextMsgID int64

	// outbound holds broker messages that have been produced but not yet
	// delivered, which is where delay, duplication, reorder, and loss live.
	outbound []*simMessage
	// delivered remembers the last message committed per scale set so a
	// duplication event can redeliver a batch the fleet has already applied.
	delivered map[int]*simMessage
	// restQueue holds REST scope snapshots taken at one instant and delivered at
	// another, which is what "lagging inventory" means here.
	restQueue []*simSnapshot
	// restCommitted is the most recently committed scope observation, kept so a
	// federated scope can be judged against the work that observation could
	// possibly have been about.
	restCommitted *simSnapshot
	// owedHistory is how much work the scope owed at each tick so far, which is
	// what a conservation bound over lagging evidence has to be measured against.
	owedHistory []int

	// vms is the simulated hypervisor: name -> powered on.
	vms map[string]bool
	// enteredAt records the virtual instant each instance entered each state.
	enteredAt map[string]time.Time
	// createdAt records the virtual instant each instance row was created, which
	// is when it began holding its profile's vector. It is the harness's own
	// ground truth for occupancy: property (k) measures from it, and simInstances
	// substitutes it for the store's wall-clock created_at.
	createdAt map[string]time.Time
	// claimed maps an in-flight durable operation to the executor's progress.
	claimed map[string]*simOperation

	// Fault state, all set by trace events and decayed each tick.
	tenantCPU       int
	tenantMemoryMB  int
	statisticsGap   int
	restLag         int
	hostProbeStale  int
	tartUnavailable int
	// powerMisreport is how many more ticks the enumeration lies about each named
	// VM being powered off (issue #246). It is keyed per VM because both production
	// storms hit exactly one instance while its siblings in the same `tart list`
	// output read correctly.
	powerMisreport map[string]int
	messageDelay   int
	delayWindow    int
	slowBootNext   int
	longJobNext    int
	// overrunJobNext arms the issue #223 shape: the next job to start holds its
	// runner for whole profile budgets rather than for a long suite's worth of
	// work, so nothing but the occupancy reclaim can end it.
	overrunJobNext   int
	wedgeNextDrain   int
	reassignSiblings bool
	// substituteSiblings arms the issue #123 handoff: the broker gives a
	// registered runner a QUEUED sibling instead of the request its own VM
	// acquired. Latched like reassignSiblings, so it fires at the first
	// opportunity rather than needing the trace to name the exact tick.
	substituteSiblings bool
	// substitutions counts how many times it actually happened, so a pinned
	// incident can prove the harness reached the state it claims to model.
	substitutions int
	// drainAborts counts, per instance, the recovery drains whose premise
	// execution-time evidence disproved. It is property (i)'s whole measurement.
	drainAborts   map[string]int
	stalledRunner map[string]bool
	wedgedDrain   map[string]int
	// unstoppableGuest is issue #233: a guest that will not power itself down, and
	// the minimum force it finally yields to. Unlike every other fault here it is
	// NOT decayed — that is the whole point. Nothing about a wedged macOS guest
	// gets better because time passed, and a harness whose every fault expires is
	// a harness that cannot express "this step will fail forever".
	unstoppableGuest map[string]lifecycle.StopForce
	wedgeNextGuest   lifecycle.StopForce
	// silentGuest is issue #236: a guest whose kernel has stopped executing. Its
	// probes are REFUSED, its job stops making progress, and its VM goes on being
	// enumerated as running. Like unstoppableGuest it never decays.
	silentGuest map[string]bool
	// guestDiedAt is the harness's own ground truth for property (p): the tick at
	// which each guest stopped executing. It is kept separately from silentGuest
	// because the reclaim CLEARS silentGuest when it powers the guest off, and an
	// oracle that measured from a fact the fix erases would be measuring the fix.
	guestDiedAt map[string]int
	// saturatedGuest is the false positive the mechanism must never produce: a
	// guest so busy with legitimate work that its probe cannot complete. Its
	// probes are INCONCLUSIVE, and its job progresses normally.
	saturatedGuest    map[string]bool
	silenceNextGuest  bool
	saturateNextGuest bool
	// repeatWithoutEscalating reproduces the executor as it behaved BEFORE issue
	// #233: every stop attempt is the same polite request. It exists so a pinned
	// test can prove property (o) is red on the defect and green on the fix; no
	// generated trace ever sets it.
	repeatWithoutEscalating bool
	// stopAttempts counts, per instance, the stop attempts a drain has already
	// spent failing. Production reads the same count off the durable operation
	// row; the mirror keeps it here because the harness completes and re-steps a
	// claimed operation rather than persisting a retry.
	stopAttempts map[string]int
	// arrivalsStopped marks the tick after which no new job may be created, which
	// is what makes property (f) meaningful.
	arrivalsStopped bool

	observations []tickObservation
	findings     []finding
	// known collects the first occurrence of each documented defect the run met.
	known map[string]finding
	// checkers are the property oracles evaluated after every tick.
	checkers []checker
}

type jobStatus string

const (
	jobQueued    jobStatus = "queued"
	jobAcquired  jobStatus = "acquired"
	jobRunning   jobStatus = "running"
	jobDone      jobStatus = "done"
	jobCancelled jobStatus = "cancelled"
)

// simJob is one workflow job in GitHub's world. requestID doubles as the
// scale-set runner request identifier and as DemandKey.JobID, exactly as
// app.convertDemandRecords maps them.
type simJob struct {
	requestID int64
	runID     int64
	repo      string
	owner     string
	name      string
	binding   int
	event     string
	queuedAt  time.Time
	// startedAt is when work actually began on a runner. queuedAt to startedAt is
	// the wait a GitHub user sees, and the only honest measure of whether a demand
	// the fleet released back to the queue was ever served.
	startedAt time.Time
	// finishedAt is when the runner handed the result back, which is what frees
	// the capacity the next job waits for.
	finishedAt time.Time
	// advertisedAt is the tick at which the broker actually delivered this job's
	// JobAvailable to the fleet. Property (j) is measured from it.
	advertisedAt int
	// heard latches property (j)'s answer. A demand row the fleet has committed
	// is not un-committed, so the oracle asks once and never again.
	heard     bool
	status    jobStatus
	runner    string
	remaining int
	// announced records which broker events have already been produced, so a
	// redelivery is a duplicate rather than a new fact.
	announced map[operations.DemandEventKind]bool
	// silentCancel models the ADR 0026 ghost: GitHub cancelled the run server
	// side and never sent a terminal message, so the broker keeps advertising it.
	silentCancel bool
}

// simMessage is one scale-set message in flight.
type simMessage struct {
	binding   int
	messageID int64
	events    []operations.DemandEvent
	available int
	assigned  int
	running   int
	deliverAt int
	withStats bool
}

// simSnapshot is one complete REST scope observation of the queued jobs.
type simSnapshot struct {
	at        time.Time
	deliverAt int
	jobs      []githubscaleset.WorkflowJob
	// outstanding is every job the scope still owed at the instant of capture --
	// queued, acquired, or executing. It is the scope's whole work, and no set of
	// attributions taken from this observation may add up to more than it.
	outstanding int
}

// simOperation is the executor's progress through one durable outbox operation.
type simOperation struct {
	operation operations.Operation
	instance  string
	remaining int
}

// runTrace executes one candidate history end to end and reports every property
// violation it produced. Shrinking calls it hundreds of times, so the store is
// closed on return rather than deferred to the end of the test.
func runTrace(t testingT, cfg worldConfig, trace simTrace) []finding {
	t.Helper()
	w := newWorld(t, cfg, trace)
	defer w.close()
	return w.run()
}

func (w *world) close() { _ = w.store.Close() }

func newWorld(t testingT, cfg worldConfig, trace simTrace) *world {
	t.Helper()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open simulated store: %v", err)
	}
	w := &world{
		ctx: ctx, cfg: cfg, trace: trace, store: store, now: simEpoch,
		nextRunID: 1_000, nextReq: 500_000, nextMsgID: 1,
		vms: map[string]bool{}, enteredAt: map[string]time.Time{}, createdAt: map[string]time.Time{},
		claimed: map[string]*simOperation{}, delivered: map[int]*simMessage{},
		stalledRunner: map[string]bool{}, wedgedDrain: map[string]int{},
		unstoppableGuest: map[string]lifecycle.StopForce{}, stopAttempts: map[string]int{},
		silentGuest: map[string]bool{}, guestDiedAt: map[string]int{}, saturatedGuest: map[string]bool{},
		powerMisreport: map[string]int{},
		drainAborts:    map[string]int{}, known: map[string]finding{},
	}
	w.demand = app.DemandCoordinator{Store: store, Now: func() time.Time { return w.now },
		StatisticsMaxAge: 2 * time.Minute, GhostAbsence: 15 * time.Minute, Priority: cfg.Priority}
	w.engine = app.Engine{
		Store: store, Demand: w.demand, Config: cfg.Scheduler, Bindings: cfg.Bindings,
		ControllerID: simOwner, Mode: reconcile.Authority, Now: func() time.Time { return w.now },
		Inventory: app.ProductionInventory{
			Store: simInstances{world: w}, Executor: simTart{world: w}, Host: simHost{world: w},
			Recovery: simRecovery{world: w}, Capacity: cfg.Scheduler.LinuxCapacity, Guards: cfg.Guards,
			ElasticHostEnvelope: cfg.Scheduler.ElasticHostEnvelope, HostBudget: cfg.Scheduler.HostBudget,
			// The accumulator's clock must be the harness's virtual clock, because the
			// scheduler judges its instants against the tick's instant. On the wall
			// clock a run of refusals recorded thirteen real days after the simulated
			// epoch is not measurable at all, the verdict fails closed, and the
			// property would go green against a mechanism that never fires.
			Guest: &app.GuestLivenessTracker{Probe: simGuestProbe{world: w},
				Policy: cfg.Scheduler.GuestLiveness, Now: func() time.Time { return w.now }},
			// Wired exactly as production wires it (internal/daemon/runtime.go), and
			// unconditionally for the same reason: the bound it enforces is not a
			// configured mechanism. Leaving it nil is the pre-#246 world, and that is
			// how misreported_power was proved red — property (i) fires on seed 1 with
			// two recovery drains aborted on a three-tick misreport.
			Power: &app.PowerCorroborator{},
		},
	}
	w.checkers = defaultCheckers(cfg)
	return w
}

// fleetOccupancy is what the fleet's own VMs hold on the host right now. A VM
// counts from the moment it is cloned until it is deleted, which is what a
// hypervisor would report.
func (w *world) fleetOccupancy() (cpu, memoryMB int) {
	instances, err := w.store.LiveInstances(w.ctx)
	if err != nil {
		return 0, 0
	}
	for _, instance := range instances {
		if instance.State == operations.StatePlanned || instance.State == operations.StateDeleted {
			continue
		}
		cpu += instance.Resources.CPU
		memoryMB += instance.Resources.MemoryMB
	}
	return cpu, memoryMB
}

func (w *world) jobByRequest(requestID int64) *simJob {
	for _, job := range w.jobs {
		if job.requestID == requestID {
			return job
		}
	}
	return nil
}

// runnerExecuting is GitHub's own answer to "may this runner be removed": only a
// job that has actually started blocks removal.
func (w *world) runnerExecuting(name string) bool {
	for _, job := range w.jobs {
		if job.runner == name && job.status == jobRunning {
			return true
		}
	}
	return false
}

func (w *world) runnerBusy(name string) bool {
	for _, job := range w.jobs {
		if job.runner == name && (job.status == jobAcquired || job.status == jobRunning) {
			return true
		}
	}
	return false
}

// run executes the whole trace and returns every property violation observed.
func (w *world) run() []finding {
	for w.tick = 1; w.tick <= w.trace.Ticks; w.tick++ {
		w.now = simEpoch.Add(time.Duration(w.tick) * simTick)
		w.applyTraceEvents()
		w.advancePhysics()
		w.restartSequence()
		w.produceMessages()
		w.deliverMessages()
		w.deliverSnapshots()
		observation := w.reconcile()
		w.executeOperations()
		w.observations = append(w.observations, observation)
		if w.check(observation) {
			return w.findings
		}
	}
	return w.findings
}

// check evaluates the property set in order and reports whether the run must
// stop. A DOCUMENTED defect (ADR 0031's findings table) is recorded and ends the
// tick's evaluation without ending the run, so a known hole neither fails the
// sweep nor masks the properties it would otherwise cascade into. Anything else
// stops the run immediately with its counterexample.
func (w *world) check(observation tickObservation) bool {
	for _, evaluate := range w.checkers {
		results := evaluate(w, observation)
		if len(results) == 0 {
			continue
		}
		item := results[0]
		if !knownFinding(item) {
			w.findings = append(w.findings, item)
			return true
		}
		if _, seen := w.known[item.Signature+string(item.Kind)]; !seen {
			w.known[item.Signature+string(item.Kind)] = item
		}
		break
	}
	return len(w.findings) > 0
}

// reconcile is the control plane under test: the real engine tick, which
// observes inventory, plans, and commits through the real durable store.
func (w *world) reconcile() tickObservation {
	// The inventory is read BEFORE the engine reads it, not after: nothing in
	// this sequential world mutates in between, so both reads are the same
	// snapshot, and a property oracle must judge a plan against the state it was
	// built from rather than against the state it just created.
	instances, host := w.engine.Inventory.Observe(w.ctx)
	result, err := w.engine.Tick(w.ctx)
	queued := 0
	for _, summary := range result.Queues {
		queued += summary.Count
	}
	return tickObservation{
		Tick: w.tick, Now: w.now, Plan: result.Plan, Applied: result.Applied, Err: err,
		Demands: result.Demands, Queued: queued, Instances: instances.Value, InstancesUsable: instances.Usable(),
		Host: host.Value, HostUsable: host.Usable(),
	}
}

// executeOperations claims durable outbox work and walks the real instance
// lifecycle one legal edge per tick, validated by the durable store's own
// transition guard.
func (w *world) executeOperations() {
	claimed, err := w.store.Claim(w.ctx, simOwner, 8, w.now, time.Hour)
	if err != nil {
		w.record(findingStoreError, fmt.Sprintf("claim operations: %v", err))
		return
	}
	for _, operation := range claimed {
		delay := 0
		switch operation.Kind {
		case lifecycle.OperationProvision:
			delay, w.slowBootNext = w.slowBootNext, 0
		case lifecycle.OperationDrain:
			if w.wedgeNextDrain > 0 {
				w.wedgedDrain[operation.ResourceID], w.wedgeNextDrain = w.wedgeNextDrain, 0
			}
		}
		// The provisioning operation is claimed on the very tick the plan created
		// its instance row, which is the earliest the world can see it exist.
		w.noteCreated(operation.ResourceID)
		w.claimed[operation.ID] = &simOperation{operation: operation, instance: operation.ResourceID, remaining: delay}
	}
	for _, id := range sortedOperationIDs(w.claimed) {
		w.stepOperation(w.claimed[id])
	}
}

func sortedOperationIDs(claimed map[string]*simOperation) []string {
	ids := make([]string, 0, len(claimed))
	for id := range claimed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// stepOperation advances one claimed operation by a single lifecycle edge.
func (w *world) stepOperation(pending *simOperation) {
	if pending.remaining > 0 {
		pending.remaining--
		return
	}
	instance, err := w.store.Instance(w.ctx, pending.instance)
	if err != nil {
		w.record(findingStoreError, fmt.Sprintf("read instance %s: %v", pending.instance, err))
		return
	}
	next, done := w.nextLifecycleState(instance)
	if next != "" {
		if !w.advance(instance, next) {
			return
		}
	}
	if !done {
		return
	}
	if _, err := w.store.Complete(w.ctx, pending.operation.ID, simOwner, pending.operation.EffectKey, w.now); err != nil {
		w.record(findingStoreError, fmt.Sprintf("complete %s: %v", pending.operation.ID, err))
		return
	}
	delete(w.claimed, pending.operation.ID)
}

// nextLifecycleState mirrors lifecycle.ProvisionExecutor and
// lifecycle.DrainExecutor edge for edge.
//
// Two details matter and are deliberately not simplified. First, the job is
// acquired at the reachable -> registering edge, exactly where production calls
// AcquireAndGenerateJIT: until then GitHub still counts the job Available, which
// is the whole re-planning window. Second, provisioning ends in ASSIGNED, not
// online_idle -- a JIT runner is created bound to its request -- so online_idle
// is only ever reached by the durable demand projection when a broker event
// overtakes registration.
//
// A wedged drain stays in draining: the 2026-07-25 shape, where GitHub refused to
// deregister a runner it had brokered another job to.
func (w *world) nextLifecycleState(instance operations.Instance) (operations.State, bool) {
	switch instance.State {
	case operations.StatePlanned:
		w.vms[instance.ID] = true
		return operations.StateCloning, false
	case operations.StateCloning:
		return operations.StateBooting, false
	case operations.StateBooting:
		return operations.StateReachable, false
	case operations.StateReachable:
		// An acquisition that returns nothing still generates a JIT runner: the VM
		// registers and then waits for work GitHub will never send, which is
		// precisely the ghost-demand runner that "registered online but never went
		// busy" in the 2026-08-01 incident.
		w.acquireJob(instance)
		return operations.StateRegistering, false
	case operations.StateRegistering:
		return operations.StateAssigned, true
	case operations.StateOnlineIdle:
		// Reached only when a projection overtook registration. The provisioning
		// operation is finished either way.
		return "", true
	case operations.StateDraining:
		if instance.DrainPhase == operations.DrainPhaseGuestUnresponsive {
			// executor.go's guest-liveness branch re-probes at the moment of acting,
			// and this is the one recovery premise a fresh observation CAN disprove: a
			// guest that answers again is alive, whatever the accumulator said a tick
			// ago, and the drain returns the instance to Running untouched.
			//
			// It asks the simulated hypervisor rather than the silent-guest flag,
			// because the stop below clears that flag and a drain held here by a wedge
			// re-enters this branch on the next tick. Reading the flag answered "alive"
			// for a guest this very drain had already powered off, and aborted a
			// reclaim whose vector was gone (2026-08-17 nightly). All three outcomes
			// are honoured exactly as executor.go honours them: alive aborts,
			// inconclusive fails the guard and retries, and only a refused transport
			// may cut.
			switch w.guestLiveness(instance.ID) {
			case domain.GuestLivenessAlive:
				w.drainAborts[instance.ID]++
				return operations.StateRunning, true
			case domain.GuestLivenessRefused:
				// The premise holds, so the guest is powered off BEFORE deregistration,
				// for the same reason the occupancy branch below does it: GitHub will go
				// on calling this runner busy until its own grace timer expires.
				delete(w.silentGuest, instance.ID)
				w.stopOccupyingGuest(instance.ID)
			default:
				// A probe that established nothing is not permission to end a job.
				return "", false
			}
		}
		if instance.DrainPhase == operations.DrainPhaseOccupancyBudget {
			// executor.go's occupancy branch stops the guest BEFORE it deregisters,
			// because GitHub will go on calling this runner busy for as long as the
			// hung job runs and deregistering first could only retry until the
			// operation dead-lettered with the vector still held. The mirror must be
			// honest about the order: the stop happens here, ahead of the refusal
			// gate below, and it is what ends the job.
			w.stopOccupyingGuest(instance.ID)
		}
		if w.wedgedDrain[instance.ID] > 0 {
			return "", false
		}
		if w.drainAbortsNow(instance) {
			// DrainExecutor.abort: the drain's planning-time premise is disproven,
			// so the instance returns to Running -- the conservative busy state --
			// and the operation completes as an acknowledged no-op (ADR 0028).
			w.drainAborts[instance.ID]++
			return operations.StateRunning, true
		}
		if w.runnerExecuting(instance.ID) {
			// GitHub refuses to deregister a runner that is EXECUTING a job, and the
			// fleet's own durable evidence did not corroborate that refusal, so the
			// stage fails and is retried rather than escalated. This is the
			// 2026-07-25 kill loop, and it is why the busy-drain invariant survives
			// even when planning-time evidence was wrong.
			//
			// An assigned-but-not-started runner is deliberately NOT refused: GitHub
			// re-queues its job when the runner goes away, which is exactly what
			// makes the stalled-assignment reclaim safe.
			return "", false
		}
		return operations.StateDeregistering, false
	case operations.StateDeregistering:
		// executor.go's deregistering arm: the runner is gone and its deletion is
		// confirmed, so all that remains is to make the guest stop holding the
		// host. A guest that will not do so is issue #233, and it is the one
		// lifecycle step the harness models as able to fail indefinitely.
		if !w.stopGuest(instance.ID) {
			return "", false
		}
		return operations.StateStopping, false
	case operations.StateStopping:
		delete(w.vms, instance.ID)
		w.releaseRunner(instance.ID)
		return operations.StateDeleted, true
	case operations.StateDeleted, operations.StateFailed:
		// A terminal instance can carry a second operation for the same effect --
		// a scheduler recovery drain beside the demand-event drain, for instance.
		// operation_effects already made the physical effect idempotent, so the
		// straggler completes as the acknowledged no-op production performs.
		return "", true
	default:
		// Assigned or running: the drain operation waits for the job to end.
		return "", false
	}
}

// stopGuest performs one rung of the REAL escalation ladder against the
// harness's own simulated guest.
//
// The rung is chosen by lifecycle.StopEscalation itself rather than by a copy of
// it, so the policy under simulation is production's policy; what the mirror
// supplies is the physics — a guest that ignores a polite request forever, and
// yields only to the force its level names. A guest the trace never wedged
// stops on the first ask, which is every trace that predates issue #233 and is
// why their histories are unchanged.
func (w *world) stopGuest(id string) bool {
	// A trace event that arrived before any drain existed wedges the next guest
	// the fleet actually asks to stop, which is the earliest instant the fault is
	// meaningful. Arming it at claim time would miss every drain, because the
	// harness drives a whole instance lifecycle from whichever operation it holds.
	if w.wedgeNextGuest > lifecycle.StopGraceful {
		if _, already := w.unstoppableGuest[id]; !already {
			w.unstoppableGuest[id], w.wedgeNextGuest = w.wedgeNextGuest, lifecycle.StopGraceful
		}
	}
	level, wedged := w.unstoppableGuest[id]
	if !wedged {
		return true
	}
	attempts := w.stopAttempts[id]
	w.stopAttempts[id] = attempts + 1
	force := lifecycle.StopEscalation(attempts)
	if w.repeatWithoutEscalating {
		force = lifecycle.StopGraceful
	}
	if force < level {
		// The guest ignores the request. Production returns a stop-stage failure
		// here and the operation is retried; the harness leaves the instance where
		// it is, which is the same observable: the vector stays held.
		return false
	}
	delete(w.unstoppableGuest, id)
	if force >= lifecycle.StopDestructive {
		// Destroy removes the guest outright rather than powering it off.
		delete(w.vms, id)
		return true
	}
	w.vms[id] = false
	return true
}

// silenceGuest kills the kernel of one running guest, and latches when there is
// none yet. Arming is deferred rather than dropped for the same reason
// wedgeGuest defers: a trace event that lands before any instance is running
// would otherwise be a no-op, and the fault would be unreachable in most traces.
func (w *world) silenceGuest() {
	if id, found := w.runningInstance(); found {
		w.killGuest(id)
		return
	}
	w.silenceNextGuest = true
}

// saturateGuest puts one running guest under so much legitimate load that its
// probe cannot complete. Its job keeps making progress; only the probe suffers.
func (w *world) saturateGuest() {
	if id, found := w.runningInstance(); found {
		w.saturatedGuest[id] = true
		return
	}
	w.saturateNextGuest = true
}

// misreportPower makes the enumeration lie about one running instance's VM for
// the next count ticks. The instance is chosen the way the guest faults choose
// theirs, and for the same reason: only a Running instance has a power reading a
// destructive recovery is derived from.
//
// Unlike the guest faults it does NOT latch when nothing is running. A misreport
// with no instance to misreport about is not a deferred fault, it is no fault at
// all — the reading it corrupts does not exist yet.
func (w *world) misreportPower(count int) {
	id, found := w.runningInstance()
	if !found {
		return
	}
	w.powerMisreport[id] = count
}

func (w *world) killGuest(id string) {
	if w.silentGuest[id] {
		return
	}
	w.silentGuest[id] = true
	w.guestDiedAt[id] = w.tick
}

// runningInstance is the first live instance actually executing work.
//
// It is deliberately RUNNING only, not Assigned. The guest-liveness mechanism is
// scoped the same way and for the same reason: an instance that never reached
// Running has never had a guest worth this question, and the assignment deadline
// already owns that ground. Arming either guest fault against an Assigned
// instance would be asking property (p) to bound a condition a different
// deadline is responsible for.
func (w *world) runningInstance() (string, bool) {
	instances, err := w.store.LiveInstances(w.ctx)
	if err != nil {
		return "", false
	}
	for _, instance := range instances {
		if instance.State == operations.StateRunning {
			return instance.ID, true
		}
	}
	return "", false
}

// armLatchedGuestFaults applies a guest fault the trace asked for before any
// guest existed, at the first tick one does.
func (w *world) armLatchedGuestFaults() {
	if !w.silenceNextGuest && !w.saturateNextGuest {
		return
	}
	id, found := w.runningInstance()
	if !found {
		return
	}
	if w.silenceNextGuest {
		w.killGuest(id)
		w.silenceNextGuest = false
	}
	if w.saturateNextGuest {
		w.saturatedGuest[id] = true
		w.saturateNextGuest = false
	}
}

// drainAbortsNow mirrors lifecycle.DrainExecutor's execution-time re-checks
// against the REAL durable evidence the production adapters read. It is
// deliberately not answered from the simulator's own ground truth: the
// disagreement issue #123 is about lives precisely between these queries, and an
// oracle that resolved it by cheating could not reproduce it.
//
// There are two gates, in the executor's own order.
//
// First the drain phase re-verifies its own planning premise, on the key that
// premise was derived from:
//
//   - A stalled assignment aborts when the BOUND demand shows a job that started
//     (ControlRouter.JobStarted).
//   - A lingering runner aborts when the BOUND demand shows a job executing right
//     now (ControlRouter.JobActive).
//   - Every other drain -- the event drain the demand projection issues -- aborts
//     on RUNNER-keyed evidence (ControlRouter.RunnerBusy), because GitHub may have
//     handed this runner a job the fleet is not waiting on.
//
// Then, and only then, the drain reaches GitHub, and githubRefusesDeregister is
// the second gate. This is the edge that made the 2026-08-02 churn a LOOP rather
// than a kill: the bound demand showed nothing started, so the stalled-assignment
// guard passed and the executor really did try to deregister a runner that was
// executing a sibling. GitHub refused (`runner_busy`), the runner-keyed re-check
// confirmed it, and the drain aborted (executor.go's deregister-refusal branch).
//
// The stopped recovery re-reads the power. Until issue #246 that was unreachable
// here, and the comment this replaces said so — "no fault powers a VM off" — which
// was true and beside the point: the production incident did not power a VM off,
// it made the ENUMERATION say one was off while it ran. misreported_power builds
// that, and this branch is the disagreement it produces.
//
// The inactive recovery keys on registration and stays unreachable.
func (w *world) drainAbortsNow(instance operations.Instance) bool {
	scaleSet := w.storeKeyFor(instance)
	if scaleSet <= 0 {
		return false
	}
	switch instance.DrainPhase {
	case operations.DrainPhaseStalledAssignment, operations.DrainPhaseLingeringRunner:
		record, err := w.store.DemandRecord(w.ctx, scaleSet, instance.Demand.JobID)
		if err == nil && record.Status == operations.DemandJobStarted {
			return true
		}
		if err == nil && instance.DrainPhase == operations.DrainPhaseStalledAssignment &&
			record.Status == operations.DemandJobCompleted {
			return true
		}
	case operations.DrainPhaseStoppedRecovery:
		// executor.go re-reads the power through VMControl.Running at the moment of
		// acting and aborts when the VM answers that it is running. That re-read goes
		// to the hypervisor's truth, NOT through the misreport above: a second read of
		// a lying enumeration would agree with the first, and the production incident
		// is precisely that the two disagreed, eighty-six times, two seconds apart.
		return w.vms[instance.ID]
	case operations.DrainPhaseInactiveRecovery:
		return false
	case operations.DrainPhaseGuestUnresponsive:
		// Already re-verified against a fresh probe in nextLifecycleState, ahead of
		// the stop, exactly where executor.go performs it. Falling through to the
		// runner-keyed question below would abort every one of these drains: GitHub
		// calls a dead guest's runner busy for as long as its own grace timer runs,
		// and that refusal is the premise rather than a refutation of it.
		return false
	case operations.DrainPhaseOccupancyBudget:
		// The one phase with nothing to re-verify. Every other cause claims that no
		// work is happening and aborts when work turns out to be happening; here the
		// work IS the premise, so an abort on busy evidence would make the budget
		// unenforceable by construction (executor.go's occupancy branch, and its
		// exemption from the deregister-refusal abort below it).
		return false
	}
	// verifyRunnerIdle for an event drain, and the deregister refusal for a
	// recovery drain, ask the same runner-keyed question of the same durable
	// evidence. Runner-active evidence implies GitHub would refuse anyway, so one
	// query answers both.
	busy, err := w.store.RunnerActiveJob(w.ctx, scaleSet, instance.ID)
	return err == nil && busy
}

// storeKeyFor resolves the durable scale-set key an instance's demand lives
// under, which is the same routing lifecycle.ControlRouter performs.
//
// The demand's own scale set is asked first, because in a federated scope the
// profile no longer names one set: two identically-labelled sets serve one
// profile, and an instance's evidence lives under the set that brokered its job.
// The profile fallback is the answer for every other world, and is identical to
// the demand lookup wherever a profile has exactly one set.
func (w *world) storeKeyFor(instance operations.Instance) int64 {
	if job := w.jobByRequest(instance.Demand.JobID); job != nil {
		return w.cfg.Bindings[job.binding].StoreKey
	}
	for _, binding := range w.cfg.Bindings {
		if binding.Profile.ID == instance.Profile {
			return binding.StoreKey
		}
	}
	return 0
}

// releaseRunner ends whatever the broker had brokered to a VM that no longer
// exists. A job that never started returns to the queue, which is precisely how
// GitHub re-advertises work whose runner disappeared.
func (w *world) releaseRunner(name string) {
	for _, job := range w.jobs {
		if job.runner != name {
			continue
		}
		job.runner = ""
		switch job.status {
		case jobAcquired, jobRunning:
			job.status = jobQueued
			job.announced = map[operations.DemandEventKind]bool{operations.DemandJobAvailable: true}
		case jobQueued, jobDone, jobCancelled:
		}
	}
}

// stopOccupyingGuest is the guest stop an occupancy-budget reclaim performs
// before it deregisters: the ephemeral VM is asked to power down, and whatever
// it was executing ends with it.
//
// GitHub's own answer to that is a lost-communication failure, so the job is
// retired rather than re-queued -- which is the difference between this and
// releaseRunner, where a runner that disappears hands its unstarted work back to
// the queue. A hung job that came back queued would be spawned for again and
// hang again, and the budget would have bought nothing.
func (w *world) stopOccupyingGuest(name string) {
	for _, job := range w.jobs {
		if job.runner != name {
			continue
		}
		switch job.status {
		case jobAcquired, jobRunning:
			job.status, job.finishedAt = jobCancelled, w.now
		case jobQueued, jobDone, jobCancelled:
		}
	}
	if _, exists := w.vms[name]; exists {
		w.vms[name] = false
	}
}

func (w *world) advance(instance operations.Instance, next operations.State) bool {
	updated, err := w.store.Advance(w.ctx, lifecycle.StateChange{InstanceID: instance.ID,
		ExpectedState: instance.State, ExpectedVersion: instance.Version, NextState: next})
	if err != nil {
		w.record(findingStoreError, fmt.Sprintf("advance %s %s->%s: %v", instance.ID, instance.State, next, err))
		return false
	}
	w.enteredAt[stateKey(updated)] = w.now
	return true
}

func (w *world) record(kind findingKind, detail string) {
	w.findings = append(w.findings, finding{Kind: kind, Tick: w.tick, Detail: detail})
}
