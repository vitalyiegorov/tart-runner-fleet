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
| a | **Liveness / no wedge.** A tick with a demand that definitely fits must lead to an admission within K ticks. | Consecutive ticks with a feasible demand and no spawn, excluding ticks with an unusable observation or a teardown in flight. On a tick whose plan holds a reservation for a head that FITS the oracle's own free envelope and is UNDER its repository cap, every other demand is judged against `free - reservation`. When the head's own repository cap is what holds it, only a demand that could take the head's whole vector is judged against `free - reservation`; everything else is judged against `free`. | Enforced (issue #208's remainder-pass wedge fixed 2026-08-05; the reserved-vector refinement 2026-08-09, issue #216; the repository-cap axis 2026-08-10, issue #226 and [ADR 0038](0038-a-cap-held-reserved-head-lends-its-vector.md)) |
| b | **Bounded starvation.** An aged feasible demand may not be passed over by younger work more than N times. | Pass-over count per (demand, CAUSE) against the youngest demand admitted ahead of it. | Enforced with three documented exemptions (findings 4, 5, and the ADR 0017 reserved head); finding 3 fixed 2026-08-03 and issue #208's stale reservation 2026-08-05, both in [ADR 0004](0004-bounded-control-plane-priority.md) |
| c | **Plans always apply.** A ready plan is never refused. | Commit error or `applied == false` on a ready plan with operations. Dumps the plan, the demands, the instances, and the host. | Enforced |
| d | **Identity uniqueness.** No identity is used twice in flight. | Repeated operation identity within a plan; two live instances incarnating one demand. | Enforced |
| e | **No double admission.** One demand is admitted once. | A demand spawned twice in a plan, or spawned while a live instance already incarnates it. | Enforced (finding 1 fixed 2026-08-03; see [ADR 0027](0027-one-tick-admits-a-demand-once.md)) |
| f | **Eventual quiescence.** The fleet empties when the demand stream stops. | Q ticks after GitHub has no work: scheduler-visible demand and in-flight operations must both be zero. | Enforced |
| g | **Conservation.** Instances never exceed the envelope or the caps. | Aggregate CPU, memory, and slots against the physical machine; per-repository count against its cap (excluding a rebound instance, whose repository the broker chose); per-profile count against MaxActive. | Enforced (finding 2 fixed 2026-08-03; see [ADR 0030](0030-a-reserved-head-holds-one-repository-slot.md)) |
| h | **No stranded demand.** A queued job is never held by an instance that is executing a different one. | An instance still incarnating a demand GitHub has queued and dispatched to nobody, while GitHub has dispatched another job of the same scale set to its runner, for more than G ticks. | Enforced (added with [ADR 0033](0033-a-runner-is-bound-to-the-job-github-gave-it.md)) |
| i | **No drain churn.** A recovery drain the executor aborts does not repeat. | Per-instance count of drains disproven at execution time, against N. | Enforced (added with [ADR 0033](0033-a-runner-is-bound-to-the-job-github-gave-it.md)) |
| j | **Nothing goes unheard.** A job whose `JobAvailable` the broker actually delivered reaches the durable demand ledger. | Per-job ticks between delivery and the ledger, against H. It is a delivery budget, not a scheduling one. | Enforced (added with [ADR 0035](0035-a-broker-message-id-is-unique-only-within-its-sequence.md)) |
| k | **Bounded occupancy.** No instance holds its profile's resource vector beyond that profile's occupancy budget. | Per-instance age from the harness's own virtual creation instant, against the budget plus a bounded reclaim grace. Scoped to instances that consume host resources and are not already tearing down. | Enforced (added with [ADR 0036](0036-an-instance-may-not-hold-its-vector-forever.md)) |
| l | **No tier inversion.** Two feasible demands of one platform, one resource vector, and one ADR 0004 lane are admitted in priority-tier order. | Effective tier of each not-admitted feasible demand against each admitted one that shares its vector and lane, excluding the reserved head and any repository this plan has already filled to its cap. | Enforced (added with [ADR 0037](0037-a-declared-tier-orders-a-band-escalation-bounds-it.md); inert on every world that declares no tier) |
| m | **Monotonic escalation.** A waiting demand's effective priority tier never falls. | Per-demand high-water mark of the effective tier, recomputed by the harness from the demand and the clock. | Enforced (added with [ADR 0037](0037-a-declared-tier-orders-a-band-escalation-bounds-it.md)) |
| n | **No tier starvation.** Escalation ends every tier-based pass-over within T ticks. | Per-demand count of ticks passed over by an aged overtaker of strictly higher effective tier, against T = declared tiers x escalation ticks + K. | Enforced (added with [ADR 0037](0037-a-declared-tier-orders-a-band-escalation-bounds-it.md)) |
| o | **Bounded teardown release.** Once a drain has passed deregistration, the instance releases its resource vector within a bounded number of ticks, whatever the guest does. | Per-instance ticks since the oracle first saw the instance in `deregistering` or `stopping` while still consuming host resources, against the release bound. Scoped past deregistration deliberately: a deregistration GitHub legitimately refuses is bounded by evidence rather than by a clock, and property (i) already watches it. | Enforced (added with [ADR 0039](0039-a-drain-that-cannot-stop-its-guest-escalates.md)) |
| p | **Bounded dead-guest release.** No instance whose GUEST has stopped executing holds its resource vector beyond the probe window plus the escalation bound. | Per-instance ticks since the harness's own virtual instant of guest death — not since the fleet first suspected it — against the release bound. Scoped to instances still consuming host resources. Counting from the death rather than from the verdict is what keeps the oracle independent of the mechanism it judges: an oracle counting from the verdict would go green however long the noticing took, and the noticing is half the defect. | Enforced (added with [ADR 0040](0040-a-guest-that-stopped-answering-is-not-running.md)) |

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

Finding 7 -- a wedge behind a reserved head a REPOSITORY CAP, not the host, was
holding -- was found by the sweep that added property (k) and issue #223's
`overrun_job`. It is FIXED, by [ADR 0038](0038-a-cap-held-reserved-head-lends-its-vector.md),
and its history is the most instructive thing in this record.

It was first reported as a scheduling defect rather than an occupancy one -- it
reproduced unchanged with every simulated profile's budget set to zero -- and
tolerated under `sigReservedHeadHeldByARepositoryCap` while issue #226 tracked
it. Then the third oracle refinement below landed, and no seed of the
container-node arm reproduced it any more. The pin was RETIRED PENDING #226,
recorded as retired and never as fixed, because a pin no seed reaches cannot be
maintained.

The suspicion in that retirement was right, and the record should be blunt about
it: **the wedge never went away, the oracle stopped being able to see it.** The
refinement judged a withheld reservation on the vector axis and not on the
repository-cap axis, so `feasibleDemands` withheld a whole vector on behalf of a
head it had itself already ruled inadmissible on the cap. Worse, the tick the
refinement was measured on -- issue #216's seed 92 -- is itself a cap-held head,
so property (a)'s original report there was a TRUE POSITIVE that the refinement
taught the harness to disbelieve. The oracle defect that refinement fixed was
real; the state it was anchored to was not an instance of it.

That is the failure mode worth keeping: teaching an oracle to agree with the
implementation on one axis stops it asking about the others, and a blind oracle
does not merely permit a defect -- it certifies the fleet as correct while the
defect runs in production. The signature, `wedgeSignature`, and the
characterization test are restored, with three corrections. The signature no
longer says the mechanism is unreduced: it is `safeBackfill`'s remainder
subtraction, and the wedge reduces to a single tick's inputs. The
characterization is a one-tick `PlanTick` test rather than a trace pin, because a
trace pin only holds while `overrun_job` is armed. And finding 7 is NOT in
`knownFinding`: like findings 1, 2 and 3 it is fixed, so a wedge carrying its
signature now fails the sweep like any other violation, reported by name rather
than as an anonymous refusal.

Three oracle refinements were needed to keep that promise honest. The first two
were made with [ADR 0033](0033-a-runner-is-bound-to-the-job-github-gave-it.md);
the third is the amendment of 2026-08-09 below.

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

#### Amendment 2026-08-09: a held reservation is part of the feasibility question

Issue #216. Property (a) reported a wedge on a tick that was correctly holding a
vector for the oldest aged demand, and the fix belongs to this record because the
plan was right and the oracle was wrong.

`feasibleDemands` measured a free envelope and called every demand that fit it
"definitely admissible". A tick that holds a reservation does not have a free
envelope. ADR 0029 condition 1 withholds the reserved head's whole vector and
`safeBackfill` plans inside `free - reservation`, so a demand that fits only
INSIDE that vector is refused by a WRITTEN rule -- the aged-FIFO guarantee of
ADR 0004 -- rather than by an arithmetic slip. On seed 92 of the container-node
arm six cores were free, two `xl` demands each needed all six, and the reserved
head was six minutes older. Calling the younger one feasible and then reporting
twelve ticks of "no admission" reports the guarantee as the defect. The nightly
sweep failed on that signature three nights running.

The oracle now applies ADR 0017's own distinction, with its own arithmetic:

- **The head does not fit the free envelope.** Nothing is charged and admission
  proceeds in the full residual, because such a head is blocked by live
  instances rather than by this tick's admission. Work that fits and is refused
  here **is** a wedge, and property (a) is untouched. That is the 2026-07-25
  incident, issue #125, and the whole reason the property exists.
- **The head does fit.** Its whole vector is withheld by design, and every other
  demand is judged against `free - reservation`. The head itself is always
  judged against the whole envelope, so a reservation held for a head the oracle
  can admit, admitting nothing, is still reported.

The refinement is deliberately narrow in both directions. Exempting "a
reservation is held" wholesale would blind the property to the exact class it was
built for; both incidents behind it are ticks with a reservation held. And
ADR 0030's repository slot -- a reserved head also holds one slot of its own
repository's cap -- is deliberately NOT modelled. Charging it would narrow the
oracle further, and narrowing is the direction that blinds a property; leaving it
out can only produce a report the scheduler must then justify.

**Independence.** The rule below the property set says the feasibility oracle
reads physical facts and configured caps and never the scheduler's envelope
arithmetic. It still holds, and the two inputs here are why. The reservation is
read from the plan's own published next state -- a DECISION the plan announces,
which property (b) has always read through `holdsReservation` -- and not from any
envelope computation. The vector subtracted is the head's CONFIGURED profile
vector from `worldConfig`, never the `Resources` the scheduler stamped on the
reservation and never its free, aged, or remainder envelopes. An envelope defect
therefore cannot reach the oracle; only a reservation the scheduler admits to
holding can.

**Evidence that nothing was blinded.** `tests/simulation/oracle_reservation_test
.go` pins all four directions of the question directly against the oracle rather
than against the scheduler, because a scheduler test cannot tell an oracle that
is right from one that agrees with the code it checks. Beyond that, the
refinement was measured against broken schedulers. With ADR 0017's
infeasible-head branch mutated back to issue #125's behaviour, the corpus over
40 seeds x 320 ticks reports the same 10 and 3 violated seeds on the two Linux
arms under the new oracle as under the old one, and the pull-request sweep still
fails on its first seed. With `internal/scheduler` reverted to the commit before
issue #208's repair, both of that issue's pinned regressions still fire.

#### Amendment 2026-08-17: the world model may not build what the fleet cannot reach

Issue #239. The first nightly sweep after ADR 0040's guest-liveness reclaim landed
reported conservation violations on three arms at once — `TestSimFuzz` seed 96
(tick 190), `TestSimFuzzBudgetedHost` seed 33 (tick 260), and
`TestSimFuzzTieredPriority` seed 499 (tick 136) — each of them exactly four ticks
after a `silent_guest` event.

**This is a world-model defect, and it is the opposite of issue #216 above.**
The distinction is the whole reason the record is here. In #216 the oracle
over-reported: `feasibleDemands` called a demand admissible that a written rule
refused, so the property was wrong about a world the fleet had built correctly.
Here property (g) was **right about every tick it judged**. It faithfully
reported a genuine double-charge in the world it was given — two instances really
were holding the vector at once. What was wrong is that `simGuestProbe` had
built a world the fleet cannot reach. `conservationChecker` is byte-identical
across #237; the property never changed and never needed to.

Getting this the right way round matters for this record specifically. After
#220 and #226 the reflex when a property fires on a harness change is to suspect
the property, and that reflex would have "fixed" a correct oracle here and left
the fiction in place.

`simGuestProbe` answered the probe from the harness's `silentGuest` flag alone.
The reclaim CLEARS that flag when it powers the guest off, so one tick later the
harness reported the guest it had just killed as **alive**. That is not a
conservative approximation; it is the one answer the real probe can never give. A
VM that is not running executes nothing, so `exec <instance> true` fails against
the control socket rather than against the probe's own deadline, and
`daemon.execGuestProbe` classifies precisely that as **refused**.

The cost of the fiction was a fabricated abort. A drain held in `Draining` by a
wedge re-enters its phase branch on the next tick, read "alive", and returned the
instance to `Running` — with its VM powered off. `Draining` plus a proven-idle
power is exactly what `domain.ConsumesHostResources` reads as the vector having
come back, so the scheduler had already committed that capacity to a replacement
spawn. The corpse then re-took it. Both instances charged the host, and property
(g) reported the sum, correctly: the harness really had built a world holding
11 CPU on a 10-CPU machine. What it had not done was build one the fleet could
reach.

**The rule.** The simulator's physics may be crueller than reality and may be
simpler than reality, but it may never return an observation the production
adapter it stands in for could not return. A harness that can answer a question
in a way no adapter can is not testing the fleet against the world; it is testing
it against a world that does not exist, and every property downstream inherits
the fiction. `world.guestLiveness` is now the single place the harness decides
what a guest is doing, and both the probe port and the drain mirror read it, so
they cannot drift apart again.

**Which side was wrong, and how that was established.** The claim to establish is
narrow and it is not "the property over-reported": it is that no sequence of
production code can reach the state the world model built. From the records, not
by making the harness agree with the code:

- `daemon.execGuestProbe` classifies a fast failure as refused and only a
  successful command as alive, pinned by
  `TestTheGuestProbeClassifiesRefusalSeparatelyFromSlowness` — including the
  literal control-socket error a stopped VM produces.
- The only abort in `DrainPhaseGuestUnresponsive` sits AHEAD of the stop
  (`internal/lifecycle/executor.go`), so no vector has been released when it can
  fire.
- The one abort that could follow a stop — the `runner_busy` deregister refusal —
  is explicitly withheld from the phases that stop their guest first
  (`lifecycle.stopsItsGuestFirst`), whose comment names this exact hazard.

So no sequence of production code returns a stopped-guest instance to `Running`.
`TestAReclaimThatAlreadyStoppedItsGuestCompletesOnRetry` now pins the composition
rather than either half, because the retry-after-stop path is where the harness
went wrong and nothing exercised it end to end.

**One dependency worth naming.** The phase re-probes at the top of every attempt
and keeps no memory of having stopped the guest already. Its safety on a retry is
therefore carried entirely by the probe's answer for a powered-off VM, not by an
ordering guard. That is true today and pinned on both sides; a future change that
made a stopped VM read as anything other than refused would reintroduce this
defect in the fleet rather than in the harness.

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
physical facts, configured caps, and the reservation the plan publishes -- never
from the scheduler's own envelope arithmetic -- so it cannot inherit the defect
it exists to catch.

The cost of that independence has its own shape, and issues #129 and #216 are
both instances of it: an oracle written from the rules rather than from the code
can be INCOMPLETE about a rule, and an incomplete oracle reports a correct plan
as a defect. Each such imprecision is repaired here with the rigour of a
scheduling fix -- red first, both directions pinned, and measured against a
deliberately broken scheduler to prove nothing was blinded -- and never by
tolerating a signature. Signatures name defects this repository documents as open
in the SCHEDULER; filing a harness imprecision under one would move a harness bug
into the scheduler's ledger.

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
- `tests/simulation/properties_test.go` -- the property oracles and the
  independent feasibility oracle.
- `tests/simulation/priority_test.go` -- the tiered arm of
  [ADR 0037](0037-a-declared-tier-orders-a-band-escalation-bounds-it.md),
  properties (l), (m), and (n), and the escalation bound stated deterministically
  against a rolling backlog of higher-tier work.
- `tests/simulation/fuzz_test.go` -- `TestSimFuzz`, the determinism contract, and
  the generator-coverage guard.
- `tests/simulation/incidents_test.go` -- 2026-08-01 (ADR 0026), 2026-08-02 (ADR
  0027), and 2026-07-25 (ADR 0017) replayed as pinned traces, plus finding 1's
  minimal three-tick regression: the boot window plans nothing, and a terminal
  incarnation is still retried. 2026-08-10 (ADR 0039) is pinned twice, because a
  property is only worth having if it is red on the defect: once with the
  escalation ladder, where the vector comes back and the queued job runs, and
  once with the pre-#233 executor that asked the same way every time, where
  property (o) fires and names the instance, the vector, and the hold. 2026-08-16
  (ADR 0040) is pinned three times: once with the guest-liveness reclaim, where
  the vector comes back on the fleet's own bound instead of GitHub's; once with
  the bound unconfigured — the fleet of every day up to that date — where property
  (p) fires; and once against a guest that is merely SATURATED, which must finish
  its job untouched however many probes go unanswered.
- `tests/simulation/findings_test.go` -- characterizations of findings 2 to 5.
- `tests/simulation/oracle_reservation_test.go` -- the feasibility oracle asked
  directly about a held reservation: the tick of issue #216 it must stop
  reporting, and the three it must go on reporting.
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
