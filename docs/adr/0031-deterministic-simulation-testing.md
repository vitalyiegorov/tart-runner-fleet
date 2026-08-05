# ADR 0031: Cross-pass defects are found by simulation, not by coverage

## Status

Accepted. It adds a test method rather than a scheduling rule, so it changes no
invariant stated by another record. It does state which invariants are now
checked mechanically on every tick of a simulated fleet: the aged-FIFO and
scheduling-class rules of
[ADR 0004](0004-bounded-control-plane-priority.md), the shared cross-platform
envelope and repository caps of
[ADR 0012](0012-shared-cross-platform-capacity.md), the infeasible-head rule of
[ADR 0017](0017-infeasible-reservation-residual-backfill.md), the elastic host
envelope of [ADR 0018](0018-second-pilot-elastic-host-envelope.md), the
proven-absence expiry of
[ADR 0026](0026-queued-demand-expires-on-proven-absence.md), the
one-admission-per-tick rule of
[ADR 0027](0027-one-tick-admits-a-demand-once.md), the attempt-scoped identities
of [ADR 0028](0028-a-repeated-decision-is-a-new-attempt.md), and the remainder
conditions of [ADR 0029](0029-remainder-admission-behind-a-reservation.md) and
[ADR 0030](0030-a-reserved-head-holds-one-repository-slot.md).

## Context

Six scheduler defects reached production in forty-eight hours: a queued demand
GitHub advertised but no longer had (#114), one tick admitting a demand twice
(#121), a drain identity colliding with its own earlier attempt (#122), a
reservation vetoing work it could not use (#125), a repository guard that
over-protected a reserved head (#126), and the cross-platform residual
arbitration that ADR 0030 deferred.

Every one of them shipped through a suite at ninety-nine percent statement
coverage, and that is not a paradox. None of them is a defect of a function.
Each is a defect of a COMPOSITION: two admission passes that are individually
correct and jointly admit a demand twice; a reservation rule that is correct in
isolation and wrong beside a repository cap; an expiry that is correct given
evidence and wrong given the absence of it. A per-function test asserts a
function's contract, and the contract was kept every time. The defect lived in
the state space the functions reach TOGETHER, over many ticks, under messages
that arrive late, twice, or not at all.

That state space cannot be enumerated by hand. `tests/replay` is the honest
attempt: each incident becomes a fixture, and each fixture pins one point. It is
excellent regression evidence and it is, by construction, always one incident
behind.

The industry answer to this exact problem is deterministic simulation testing:
FoundationDB's simulator, TigerBeetle's VOPR, Antithesis. The shared idea is
narrow and worth stating precisely, because it is easy to spend the effort on the
wrong half:

- **Determinism, not fidelity.** The point is not a realistic Mac mini. The
  point is that every source of nondeterminism -- clock, message delivery,
  observation freshness, host load, VM latency -- is a value drawn from one seed,
  so a failure is a command line rather than an anecdote.
- **Properties, not assertions.** The oracle is an invariant checked on every
  tick of every run, not an expected output compared once.
- **Seed fuzzing plus shrinking.** Coverage of the state space comes from
  volume; usefulness of a failure comes from reducing it to something a person
  can read.

## Decision

`tests/simulation` runs the real control plane inside a simulated single-host
fleet, driven by a seeded event trace and a virtual clock.

### What is real

The code under test is production code, not a model of it:

- `scheduler.PlanTick` -- the whole planner, unmodified.
- `reconcile.Controller.Commit` -- plan translation, generation scoping,
  content-addressed identities.
- `sqlite.Store` -- opened at `:memory:` and migrated, so `ApplyPlan`,
  `ApplyDemandBatch`, `ActiveDemands`, `PutDemandStatistics`,
  `ReconcileGitHubJobSnapshot`, `ExpireGhostDemands`, `ProjectDemandEvent`,
  `alignRunnerDemand`, `Transition`, `Advance`, `Claim`, and `Complete` are the
  real writes against the real schema and the real constraints.
- `app.Engine.Tick` and `app.DemandCoordinator` -- the whole demand pipeline,
  including the statistics bound and the degraded trickle path.
- `app.ProductionInventory` -- absence classification, the untracked-VM guard,
  and `hostObservation`'s elastic envelope.
- `domain` -- the lifecycle state machine, which validates every transition the
  simulated executor performs.

### What is simulated

- **The scale-set broker.** Messages are produced from GitHub's own truth and
  delivered with configurable delay, duplication, reordering, and loss
  (at-least-once: a lost batch is re-announced). Statistics are computed when a
  message is PRODUCED, so a delayed message carries a genuinely stale count. A
  run may be cancelled loudly (a terminal message follows) or silently (none ever
  does, which is the ADR 0026 ghost). Two acquired requests may be matched to the
  opposite registered runners, which is the crossed assignment ADR 0028 exists
  for; and a registered runner may be given a still-QUEUED sibling instead of the
  request its own VM acquired, which no instance incarnates and no swap can
  repair -- the handoff of ADR 0033.
- **The REST scope.** Complete queued-job snapshots taken at one instant and
  delivered at another, so "lagging inventory" is a real skew rather than a flag.
- **The host.** An Apple M4 -- ten cores, 24 GiB, parameterized -- shared with
  its own tenant, reported through a `macos.Snapshot` that the real guardrails
  and the real elastic envelope consume. The probe can also go stale, and `tart
  list` can fail.
- **The executor.** It mirrors `lifecycle.ProvisionExecutor` and
  `lifecycle.DrainExecutor` edge for edge, including the two details every
  timing question turns on: the job is acquired at the `reachable ->
  registering` edge, and provisioning ends in `assigned`, not `online_idle`. A
  drain re-verifies its premise against the real durable adapters and aborts on
  contrary evidence, and GitHub refuses to deregister a runner that is EXECUTING
  a job -- the edge that makes a recovery drain a loop rather than a kill.
- **Job duration.** Work runs for a configured number of ticks, and a trace may
  make a job outlive a recovery deadline. Until it could, no job ever did, and
  the whole abort class of ADR 0028 was unreachable.
- **The clock.** One tick is thirty virtual seconds. `FairnessAge` is ten ticks,
  the statistics freshness budget four, the ghost-absence window thirty.

One substitution is deliberate and is the only place the harness rewrites a
durable fact: `simInstances` replaces `Instance.UpdatedAt` with the VIRTUAL
instant the row entered its current state. The store stamps it from the process
wall clock and `ProductionInventory` derives `AssignedSince` and `RunningSince`
from it, so under a virtual clock every row would look newborn and no assignment
or idle-runner deadline could ever be reached.

### Traces, not dice

Generation and execution are separated. `generateTrace(seed, ticks, config)`
consumes the seed and produces an explicit `[]simEvent`; execution consumes only
the trace. A run is therefore a pure function of `(trace, config)`, which buys
two things: replay is exact, and a failing trace can be SHRUNK by deleting
events. `shrink` is delta debugging -- remove the largest prefix, suffix, or
single event whose removal still reproduces a finding of the same kind -- and
routinely reduces a two-hundred-event history to one or three lines.

### The property set

Every property is a checker over the simulator's observable state, evaluated
after every tick. Checkers run in causal order: the oracle that names a specific
cause runs before the one that reports its downstream symptom, so a demand
admitted twice is reported as a double admission rather than as a database
refusal.

| | Property | Oracle | Status |
|---|---|---|---|
| a | **Liveness / no wedge.** A tick with a demand that definitely fits must lead to an admission within K ticks. | Consecutive ticks with a feasible demand and no spawn, excluding ticks with an unusable observation or a teardown in flight. | Enforced (issue #208's remainder-pass wedge fixed 2026-08-05; see [ADR 0024](0024-mixed-macos-profile-cohorts.md)) |
| b | **Bounded starvation.** An aged feasible demand may not be passed over by younger work more than N times. | Pass-over count per (demand, CAUSE) against the youngest demand admitted ahead of it. | Enforced with three documented exemptions (findings 4, 5, and the ADR 0017 reserved head); finding 3 fixed 2026-08-03 and issue #208's stale reservation 2026-08-05, both in [ADR 0004](0004-bounded-control-plane-priority.md) |
| c | **Plans always apply.** A ready plan is never refused. | Commit error or `applied == false` on a ready plan with operations. Dumps the plan, the demands, the instances, and the host. | Enforced |
| d | **Identity uniqueness.** No identity is used twice in flight. | Repeated operation identity within a plan; two live instances incarnating one demand. | Enforced |
| e | **No double admission.** One demand is admitted once. | A demand spawned twice in a plan, or spawned while a live instance already incarnates it. | Enforced (finding 1 fixed 2026-08-03; see [ADR 0027](0027-one-tick-admits-a-demand-once.md)) |
| f | **Eventual quiescence.** The fleet empties when the demand stream stops. | Q ticks after GitHub has no work: scheduler-visible demand and in-flight operations must both be zero. | Enforced |
| g | **Conservation.** Instances never exceed the envelope or the caps. | Aggregate CPU, memory, and slots against the physical machine; per-repository count against its cap (excluding a rebound instance, whose repository the broker chose); per-profile count against MaxActive. | Enforced (finding 2 fixed 2026-08-03; see [ADR 0030](0030-a-reserved-head-holds-one-repository-slot.md)) |
| h | **No stranded demand.** A queued job is never held by an instance that is executing a different one. | An instance still incarnating a demand GitHub has queued and dispatched to nobody, while GitHub has dispatched another job of the same scale set to its runner, for more than G ticks. | Enforced (added with [ADR 0033](0033-a-runner-is-bound-to-the-job-github-gave-it.md)) |
| i | **No drain churn.** A recovery drain the executor aborts does not repeat. | Per-instance count of drains disproven at execution time, against N. | Enforced (added with [ADR 0033](0033-a-runner-is-bound-to-the-job-github-gave-it.md)) |

The single-writer, strictly sequential design is what makes property (c)
meaningful. The inventory a plan is built from cannot move before the
compare-and-set that persists it, so there is nobody to contend with: an
optimistic-concurrency loss is not the routine event `app
.ReasonPlanCommitContended` describes but a genuine composition defect.

### Documented exemptions

A defect the simulator finds must not be fixed in the PR that finds it -- a
harness change and a scheduling change in one diff is how a harness becomes a
place to hide behaviour. Each open finding therefore carries a SIGNATURE, and
the sweep tolerates that signature alone rather than disabling the property that
found it. Every signature is pinned by its own characterization test in
`tests/simulation/findings_test.go`, so a tolerated defect cannot silently
change shape, and its fix PR arrives with a test that already describes it.

An unsignatured violation of any property fails the build.

Two oracle refinements were needed to keep that promise honest, both made with
[ADR 0033](0033-a-runner-is-bound-to-the-job-github-gave-it.md):

- Property (b) counts pass-overs per (demand, CAUSE) rather than per demand.
  This is finding 6 of issue #130. A demand passed over once by each of several
  mechanisms has not been starved N times by any of them, and crediting the
  accumulated total to whichever cause happened to tip the counter reports the
  last mechanism for all the previous ones' work -- so a run of documented
  pass-overs surfaced as an unsignatured hard failure on its final tick, naming a
  mechanism that had acted once. Each cause now earns its own budget, so a
  genuinely new defect must repeat before it fails the build.
- Property (g) does not charge a REBOUND instance to a repository cap. A cap
  bounds admission -- how many VMs the fleet will create for a repository -- and
  cannot bound which repository's job GitHub then dispatches to a runner that
  already exists. The physical envelope still charges every instance in full, and
  the same VM ran the same foreign job before the binding followed GitHub; only
  what the fleet is willing to say about it changed.

### Where the harness lives

`tests/simulation` is a test-only package, exactly like `tests/replay`,
`tests/integration`, `tests/contract`, and `tests/chaos`. That is the repository's
established coverage arrangement rather than a new exemption:
`scripts/check-coverage.sh` instruments each package's own statements, and a
package with no non-test files contributes none, so the simulator carries the
ninety-nine percent gate for the production code it drives instead of for its own
scaffolding. `scripts/check-cpd.sh` likewise scans only `cmd` and `internal`.

### Budgets

The default sweep is bounded so `make unit`, `make race`, and the coverage gate
can run it unqualified on every pull request. The nightly workflow runs a much
larger seed range with the race detector.

#### Amendment 2026-08-05: what repetition is for, and what it costs

`Nightly resilience` runs `./tests/...` ten times under the race detector,
because repetition is how a flake surfaces in a suite with real timers,
goroutines, and I/O. On 2026-08-05 that job reported

    FAIL github.com/vitalyiegorov/tart-runner-fleet/tests/simulation 600.018s

which is the go test default timeout rather than a property violation, and a
timed-out package takes the rest of the job's report with it (issue #208).

Repeating THIS package is not what repetition is for. A run here is a pure
function of (trace, config): the clock is virtual, the world is single-writer
and sequential, and every source of nondeterminism is a value drawn from the
seed. Running it ten times cannot reach a state the first run did not. The
harness already states that contract itself, far more cheaply and far more
precisely than a repeat count can: `TestSimIsDeterministic` replays one trace
twice and compares the history, and `TestSimCorpusIsIdenticalAcrossRuns` sweeps
every arm three times and compares a digest folded over every plan identity and
application outcome in tick order. So the nightly runs this package once, with
an explicit budget of its own, and keeps `-count=10` for the four suites that
genuinely need it. Volume for this harness has its own workflow, which explores
500 seeds at 320 ticks.

That is a re-partitioning, so the budget was also genuinely cut, because a
budget re-partitioned and not reduced is a budget that grows back. Measured
before: `go test -race -count=1 ./tests/simulation` was 392s, of which ONE test
was 389s -- the federated sweep, at 24 seeds x 200 ticks in a single goroutine
while the three fuzz arms ran concurrently with each other. Its seeds are
independent by construction, so it now runs one parallel subtest per seed; and
property (g) reads the durable ledger only when a repository is over its cap
counted without the rebound set, which is sound because excluding a rebound
instance can only lower a count, and which removes one SQLite query from every
tick of every arm of every sweep.

Measured after: 154s for the same command, with both of issue #208's pinned
regressions added. The proportion is the point rather than the seconds. The four
suites the nightly job exists to repeat cost 9.6s together at `-count=10` under
the race detector, so a package that took 600s and failed was not one cost among
several -- it was the whole report.

## Consequences

The harness found four previously unknown defects while it was being written,
including one that needs no simulator at all to reproduce once you know to look
(finding 1: two consecutive ticks over a real store). The findings are recorded
in the pull request that introduced this record and pinned in
`tests/simulation/findings_test.go`.

Finding 1 was fixed on 2026-08-03 by the queue seam described in ADR 0027's
cross-tick amendment. Its signature is no longer tolerated by the sweep, and the
name survives only so a regression is reported as itself. Removing the wedge also
moved the remaining findings' frequencies, which is the expected shape: a tick
that admitted nothing could not starve anyone either. Over 150 seeds at 200
ticks, finding 1 fell from ~146 seeds to 0, seeds with no finding of any kind
rose from ~2 to ~89, and the three aged-FIFO signatures fell by roughly a
quarter to a half.

Findings 2 and 3 were fixed on 2026-08-03, by the cap amendment of ADR 0030 and
the band amendment of ADR 0004. Neither signature is tolerated by the sweep any
more. Over the same 150 seeds at 200 ticks, finding 2 fell from 11 seeds to 0 and
finding 3 from 23 to 0, and seeds with no finding of any kind rose from 107 to
123.

Fixing them also had to sharpen the oracle that NAMES them.
`starvationSignature` attributed a pass-over by the first attribute that matched,
which is harmless while several lane defects are open and misleading the moment
one is fixed: a cross-platform pass-over whose overtaker happened to belong to
the control-plane repository, and a count-maximization pass-over between two AGED
demands, were both filed under finding 3's name, so a fixed defect went on being
reported by that name on four of 150 seeds. Attribution now tests platform, then
size, then class, and records why that is the causal order.

The sharper oracle exposes MORE, not less. Run against the unfixed scheduler it
turns 14 of finding 3's 23 seeds into UNSIGNATURED violations -- aged
control-plane work overtaking OLDER aged standard work, which is the same
class-over-aging breach but out of the reach of a rule stated in terms of young
lanes -- so the old code fails the sweep on 15 seeds where the old name absorbed
everything. On the fixed scheduler both counts are zero. Nothing is tolerated
that was not tolerated before; every violation is still counted, under the open
finding whose mechanism produced it. Findings 4 and 5 absorb the renaming: over
the same corpus, 4 moves from 9 seeds to 14 and 5 from 11 to 18, of which 12 and
17 respectively are already present on the unfixed scheduler once it is measured
with the same oracle.

A cost worth naming: the simulator is a second implementation of the fleet's
environment, and a wrong model produces confident nonsense. Two safeguards
apply. The executor mirrors `lifecycle`'s state machine edge for edge and every
transition is validated by the real durable store, so a modelling error usually
surfaces as a refused transition rather than as a false property violation. And
the feasibility oracle that properties (a) and (b) rest on is derived from
physical facts and configured caps only -- never from the scheduler's own
envelope arithmetic -- so it cannot inherit the defect it exists to catch.

`TestGeneratedTraceExercisesTheWholeWorld` guards the other rot: a generator
that quietly stops delaying messages or wedging drains turns the whole suite
into a smoke test, so the required event vocabulary is asserted directly.

## Evidence

- `tests/simulation/world_test.go` -- the world, the adapters, the executor, and
  the tick order.
- `tests/simulation/broker_test.go` -- broker message production and delivery,
  acquisition, crossed sibling assignment, and REST snapshots.
- `tests/simulation/trace_test.go` -- the event vocabulary, the generator, trace
  execution, and delta-debugging.
- `tests/simulation/properties_test.go` -- the seven property oracles and the
  independent feasibility oracle.
- `tests/simulation/fuzz_test.go` -- `TestSimFuzz`, the determinism contract, and
  the generator-coverage guard.
- `tests/simulation/incidents_test.go` -- 2026-08-01 (ADR 0026), 2026-08-02 (ADR
  0027), and 2026-07-25 (ADR 0017) replayed as pinned traces, plus finding 1's
  minimal three-tick regression: the boot window plans nothing, and a terminal
  incarnation is still retried.
- `tests/simulation/findings_test.go` -- characterizations of findings 2 to 5.
- `tests/simulation/federation_test.go` -- the shared-scope conservation bound of
  issue #153, swept one parallel subtest per seed.
- `tests/simulation/regression208_test.go` -- the two nightly findings of issue
  #208, pinned as the traces delta debugging reduced them to: the remainder-pass
  wedge of property (a) in four events, and the stale-reservation pass-over of
  property (b) in twenty-six.
- `.github/workflows/nightly.yml` -- the long sweep.

## Not addressed here

- **No defect found by this harness is fixed here.** Each one gets its own
  focused, test-first PR, as AGENTS.md requires.
- **Concurrency is not simulated.** The harness is single-writer and sequential
  by design, because that is what makes a commit refusal unambiguous. Lease
  fencing and multi-writer contention remain `tests/chaos`'s subject.
- **The executor's own adapters are not simulated.** Tart command construction,
  JIT handling, guest bootstrap, and GitHub HTTP semantics are covered by their
  package tests and by `tests/contract`; this harness begins where a durable
  operation is claimed.
- **Nothing here is production evidence.** Simulation is not a cutover, and no
  authority-mode promotion may cite it.
