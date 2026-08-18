# ADR 0043: A released vector cannot be taken back

## Status

Accepted. Closes
[issue #247](https://github.com/vitalyiegorov/tart-runner-fleet/issues/247).

It closes the defect
[ADR 0042](0042-a-destructive-premise-must-be-corroborated.md) narrowed and
recorded as open, and it amends that record's tearing-down exemption, which is the
half of it that was wrong.

## Context

`domain.ConsumesHostResources` is where the fleet decides that an instance has
given its compute vector back, and the scheduler admits work into whatever it
says is free. Before this record it answered from two facts: the instance is in a
cleanup state, and a host observation says its VM is not executing.

The second of those is **a reading**, and the fleet has two ways to find out a
reading was wrong.

### The reading corrects itself

The nightly sweep found it the first time `misreported_power` — ADR 0042's fault,
a backend that enumerates a running VM as powered off — was put into the
generator's draw. `geekom-linux-amd64`, seed 8, tick 36: *five slots against a
four-slot ceiling*.

```
t033  small-b60 running                 3 of 4 slots held, one admitted
t034  small-b60 draining  power=stopped  <- released; a 5th instance admitted
t035  small-b60 deregistering            4 held, all agreed
t036  small-b60 stopping   power=running <- the lie expired. 5 held.
```

Nothing in that trace is a recovery drain. The instance's job finished, an
ordinary event drain began, and while it was draining one enumeration said its VM
was already off. The fleet believed it, stopped charging the host, admitted a
replacement into the capacity — and three ticks later the enumeration told the
truth again about a guest that had been executing the whole time.

### The drain disproves its own premise

The path [issue #247](https://github.com/vitalyiegorov/tart-runner-fleet/issues/247)
states, reproduced here as a pinned trace on `mac-studio-4x10-budget`, where one
maestro is exactly the 4-vCPU budget so the vector is indivisible:

```
t014  power corroborated stopped -> recovery drain planned   (ADR 0042's bound met)
t015  draining, power=stopped    -> vector released, replacement admitted
t018  the drain re-reads the power at the moment of acting,
      finds the VM running, and aborts back to Running       (ADR 0033)
      -> 8 vCPU held against a 4-vCPU budget
```

The abort is the fleet catching itself about to destroy a live runner. It is
correct, it is ADR 0033 working exactly as designed, and it arrives **after** the
capacity has already been lent to somebody else. There is no mechanism in this
control plane for un-admitting a spawn, and there should not be one: the
replacement's VM is cloning.

### And the same defect again, on the other capacity a teardown gives back

With both of those closed, the sweep found it a third time on
`geekom-linux-amd64` seed 87, tick 146: *repository c/repo holds 3 instances
above its cap of 2*.

```
t141  power corroborated stopped -> recovery drain planned
t142  draining -> activeRepoCounts stops charging c/repo
t143  a third c/repo instance is admitted into the slot
t144  the drain re-reads the power, finds the VM running, aborts to Running
t146  three instances of a two-instance repository, all executing
```

A teardown gives back **two** capacities — the host's compute vector and the
repository's concurrency slot (ADR 0030) — and they were released by two
different predicates on the same retractable premise. Fixing one left the other.

### They are all the same defect

Capacity was released on a premise the fleet had not established, and a release
cannot be undone. Whether the retraction comes from the next enumeration or from
the drain's own re-verification, and whether the capacity is a vector or a
repository slot, is incidental; what matters is that a premise which can be
retracted was allowed to free capacity that cannot be recalled.

This is a **fleet defect**. Property (g) judged every tick correctly, and the
world it judged is one a real backend really produces — ADR 0042 demonstrated the
misreport on this fleet's own host by making one `config.json` unreadable. It is
the mirror of [#239](0031-deterministic-simulation-testing.md)'s world-model
defect and it is not one, which is the distinction ADR 0042's own record and
[#242](0031-deterministic-simulation-testing.md) exist to keep straight.

### ADR 0042's exemption is where it entered

That record exempted tearing-down instances from the corroboration bound, on this
reasoning: *"There the fleet's own decision corroborates the reading."*

It does not. **Deciding to drain an instance is not powering anything off.** For
an ordinary event drain the guest keeps executing until the drain reaches its stop
step; for a stopped recovery the decision to drain *is* the reading, so the
exemption made a reading corroborate itself. The exemption was adopted because it
kept every corpus digest byte-identical, and a digest that does not move is
evidence about the traces, not about the rule.

### And the bound it exempted was unreachable in simulation anyway

Measured while reproducing this issue: `app.PowerCorroborator` stamped and judged
its run with the instant `ProductionInventory.Observe` reads from the **wall
clock**, while the simulated fleet plans on a virtual clock thirteen real days
away. `CorroborationPolicy.Confirmed` therefore never met its forty-five second
window in any simulated world, no misreport could ever reach the recovery gate,
and ADR 0042's own damping — the retraction factor, the whole second half of that
record — was never exercised by anything except the two pinned traces.

This is the failure shape `AGENTS.md` names outright: *"one 'green no-op' shipped
only because a clock bug made a verdict unfirable"*. ADR 0040 wrote the rule down
— *a run stamped on one clock and judged on another is not measurable at all* —
and gave `GuestLivenessTracker` an injectable clock for exactly this reason; the
corroborator built on the same accumulator did not get one.

### And a second clock, on the other side of the same seam

Enabling the fault in the generator's draw then found a third thing, and it is
neither this issue's defect nor a defect at all in production.
`sqlite.Store.enqueueDemandDrain` — the ordinary teardown of a job that finished —
stamps `available_at` from `time.Now()`, while everything `reconcile.Controller`
plans is stamped from the injected clock. The simulated world claimed on its
virtual instant, so **a demand-event drain could never be claimed there at all**:
its availability lay thirteen real days in the future, the operation stayed
pending forever, and the instance sat in `draining` holding its whole vector with
nothing left to move it.

The sweep reported it as property (p) — *a dead guest held for twenty-five ticks
past its release bound*, `geekom-linux-amd64` seed 93 — and the four-event trace
it shrinks to contains no misreport at all and reproduces on the merge base. The
generator had simply never drawn that combination before the draw list changed.

Production has one clock on both sides: the worker claims with `time.Now()` and
the store stamps with `time.Now()`, so the operation is available immediately.
This is therefore a **world-model defect** in #242's taxonomy — property (p) was
right, the fleet was right, and the simulated world had built a state the fleet
cannot reach. The harness claims on the later of the two instants now, and
ordinary teardowns complete in simulation for the first time.

### And one more, the same shape as #239

At five hundred seeds the sweep then reported property (i) — *two recovery drains
aborted* — on `m4-mac-mini` seed 442. `simRecovery.JobActive`, the evidence a
lingering-runner reclaim is derived from, answered out of the simulator's own job
truth, while `drainAbortsNow` re-read the durable demand record. Production wires
**one** `lifecycle.ControlRouter` as the inventory's `RecoveryObserver` *and* as
the executor's `Control`, so both questions come out of `demandStatus` — the same
row, the same status test.

A delayed broker message made the harness's two sources disagree *persistently*:
the world knew a job had finished while the durable record still said
`JobStarted`, so the fleet planned a reclaim its own drain disproved, and planned
it again. In production the two reads can only disagree if the status genuinely
changed between planning and acting, which is the race the abort exists for and
which cannot repeat.

This is #239 exactly — the harness handing the planner and the executor different
answers to one question — and it is the reason ADR 0031's rule about mirrors is
worth restating: **a simulated port answers from the source production answers
from, or it is testing a different fleet.**

## Decision

### The vector comes back on evidence nothing can retract

`domain.ConsumesHostResources` releases a tearing-down instance's vector on two
facts, and on nothing else:

- **The VM is gone.** A successful enumeration that does not list an owned VM is
  proven absence (ADR 0022), and absence is monotone: nothing brings a deleted VM
  back, so no later reading and no aborted drain can make the fleet owe that
  vector again. It occupies strictly less than a stopped VM, so it must free the
  vector at least wherever a stop does — otherwise a deleted VM would pin the host
  to its platform harder than a live one.
- **The fleet's own stop has landed.** `stopping` is entered by exactly one edge:
  the drain powered the guest off and its deregistration was already confirmed.
  From there the instance can never return to `running` — there is no abort past
  that line — and nothing can put work back on the machine.

Everything else is a claim about a machine the fleet has not acted on, and it goes
on charging the host. `draining` and `deregistering` with a stopped reading are
now exactly that: states an instance can still leave.

### The repository slot comes back one edge earlier, for a reason the states carry

`scheduler.activeRepoCounts` stops charging a repository at **deregistration**
rather than at the start of the drain. Past that line GitHub cannot hand the
runner a job at all, so the slot is genuinely free — even while the machine is
still executing, which is why this line is not the same as the vector's.

Before it, `draining` is a state the instance can leave: ADR 0033's re-verification
sends it back to `running` with its job still running, and the replacement admitted
into the slot meanwhile is already cloning.

Two capacities, two commit points, one rule: **capacity is released when the fleet
has done the thing, not when it has decided to.** The vector needs the guest
stopped; the slot needs the runner gone. Each is named by the state that records
it.

Both rules are stated once, in the one predicate each consumer already reads. No
new state, no new field on `domain.Instance`, no new configuration, no shadow
capacity, no second notion of what is free.

### The corroborator judges on its own clock

`app.PowerCorroborator` gains the `Now` field `app.GuestLivenessTracker` has had
since ADR 0040, and `Observe` reads the instant from it rather than from the
caller. Production states `time.Now` at the wiring, exactly as it states it for the
guest tracker; the simulation states its virtual clock. Nil remains the wall clock,
so no production node changes behaviour.

### Alternatives considered

**Release only when the durable row is `deleted`.** The simplest possible rule,
and it is the one this record started from. Rejected because it re-creates the
defect ADR 0022 fixed: a VM the fleet has already deleted from the backend would
go on pinning the host to its platform while its row finished cleaning up. The
release rule has to name absence regardless, and naming `stopping` beside it costs
one comparison and returns the vector one lifecycle edge sooner.

**Release on belief, into a shadow state the scheduler cannot admit into.**
Rejected under the second question. It adds a state, a second notion of free
capacity, and a rule about when the shadow resolves — to express a fact the
existing states already carry.

**Keep releasing at `draining`, and let the drain phase decide.** The tempting
one, because it would preserve exactly the two reclaims that pay for this change
(below). Rejected because it is *narrower* rather than *safe*: a drain phase says
what the fleet intends, not what it has done, and the unsafe window is precisely
between entering the state and the stop landing. A state can answer that question
and a phase cannot.

**Latch the release: once free, never charged again.** Rejected because it would
make the oracle green while the machine is genuinely over-committed. When a
tearing-down instance's power flips from stopped back to running, the correct
conclusion is that the earlier release was wrong — the guest was executing all
along. Suppressing the correction hides the over-admission instead of preventing
it.

## Consequences

**A tearing-down instance holds its vector one lifecycle edge longer**, from the
tick its guest is observed idle to the tick the fleet's own stop lands. Measured
across the whole corpus — six arms, 64 seeds, 200 ticks, **6393 jobs offered per
arm**, both sides carrying the two world-model repairs above so the traces are
identical and only the release rules differ:

| arm | jobs completed | instance-ticks charged while tearing down | mean queue wait (ticks) |
|---|---|---|---|
| `m4-mac-mini` | 1786 → 1772 | 4127 → 4210 (+2.0%) | 50.16 → 49.83 |
| `mac-studio-4x10-budget` | 363 → 351 | 1090 → 1277 (+17.2%) | 75.34 → 73.97 |
| `geekom-linux-amd64` | 1910 → 1907 | 4476 → 4568 (+2.1%) | 54.15 → 54.36 |
| `federated-maestro-scope` | 916 → 908 | 2402 → 2541 (+5.8%) | 68.33 → 67.60 |
| `sequence-reset-linux-large` | 1581 → 1580 | 3842 → 3918 (+2.0%) | 66.09 → 66.30 |
| `tiered-release-priority` | 1734 → 1718 | 4008 → 4098 (+2.2%) | 51.29 → 51.00 |
| **total** | **8290 → 8236 (−0.65%)** | **19945 → 20612 (+3.3%)** | — |

Per teardown that is **2.15 → 2.23 ticks**: the average instance holds its vector
about **two and a half virtual seconds** longer, throughput falls by two thirds of
one percent, and the queue behind it waits *less* on four of six arms. The
budgeted arm pays the most, which is what a four-vCPU ceiling shared by one
six-vCPU profile should do — there the vector is indivisible and every extra tick
of hold is a tick nothing else can run.

That is the price of never over-admitting the host, and it is the whole of it.

**Every corpus digest moves, and each change is attributed separately.** The
default corpus flags (four seeds, sixty ticks — the settings ADR 0042's table
used):

| arm | merge base | + the two clocks | + the release rules | + `misreported_power` in the draw |
|---|---|---|---|---|
| `m4-mac-mini` | `dbd2cf83f5b4018c` | `83c5ab77cfeda4cf` | `fed74beed9a61d0e` | `740552d06c7fed8c` |
| `mac-studio-4x10-budget` | `53bfdf2506be08e4` | `c0b8b59eff3da9cc` | `4eb3c36ef04fc17d` | `70a1bc1a0b6c8b32` |
| `geekom-linux-amd64` | `e7faee12ece8b9cc` | `4bc3755040cdaef6` | `276a8d6339bed7b5` | `338662c83e821380` |
| `federated-maestro-scope` | `8599708eaa5481a4` | `75a17777c617a78d` | `55d102be1cc203e6` | `17cb38e275e1b042` |
| `sequence-reset-linux-large` | `e3921bba61fbe8a2` | `c8b8bf13d6c6ab67` | `0bcd4154003a464f` | `87edc71cd53c4790` |
| `tiered-release-priority` | `92e6ccf6a02830f1` | `08c0d74134d1da34` | `047b67b40b6a605b` | `fba852b25ecbb15a` |

The largest step is the first, and it is the world model rather than the fleet:
ordinary teardowns complete in simulation now, so every arm turns capacity over
faster and admits more (`geekom-linux-amd64`: 57 → 62 spawns over the same four
seeds). Measured before that repair, when the traces still matched the merge base
exactly, the release rules alone moved **three of six** digests while leaving every
arm's plan, spawn, drain and instance counts **identical** — which is the shape of
this change: it moves *when* a plan is applied, not how much is admitted.

The control the issue asks for — removing exactly the draw entry — therefore does
**not** reproduce the merge base byte for byte, and the reason is stated rather
than discovered: two of the three changes here alter fleet and world behaviour on
traces that contain no misreport at all.

**Two reclaims pay more than one edge, and this is the real cost.** The occupancy
budget (ADR 0036) and the guest-liveness reclaim (ADR 0040) power the guest off
*before* they deregister, precisely so the vector comes back without waiting for
GitHub — `internal/lifecycle/executor.go` says so: *"the vector comes back on a
bound the fleet owns rather than on GitHub's"*. Under this record it comes back
one deregistration later, and GitHub refuses to remove a runner it still considers
busy for as long as its own grace timer runs — sixteen to eighteen minutes,
measured eight times in issue #236.

That guarantee is therefore weakened, deliberately, and it is weakened to the
weaker of two evils. The alternative is to go on releasing capacity on a premise
the fleet cannot honour, which over-admits the host on every arm the simulator can
reach. ADR 0039's ladder still bounds the stop, ADR 0007's dead-letter path still
bounds the drain, and property (o) is untouched: it judges only instances past
deregistration, where nothing about this rule has changed.

**The simulated refusal is shorter than the measured production one.** The harness
wedges a drain for at most six ticks (three virtual minutes); production measured
sixteen to eighteen. Property (p)'s twenty-four tick bound therefore holds across
the sweep without proving the production bound, and that gap is named here rather
than implied by a green run.

**ADR 0042's exemption is withdrawn** as unsound reasoning, though the code path it
justified is now unreachable: a tearing-down instance's stopped reading no longer
releases anything by itself, so whether it was corroborated first no longer decides
a vector. The exemption is kept, and its comment corrected, because it still
governs what `Power` *reads as* for an instance the fleet has decided to tear down,
which is what an operator sees.

**The corroboration bound is now reachable in simulation, and ADR 0042's damping is
exercised for the first time.** With the clock stated, a corroborated misreport
plans exactly the recovery drain ADR 0042 bounds, that drain aborts, and the
retraction factor keeps it from re-firing — pinned by
`TestAnAbortedRecoveryNeverLentItsVector` and now reachable by the sweep on every
arm.

**The simulated fleet completes ordinary teardowns for the first time**, which is
the second clock's doing rather than this rule's, and it changes what every arm
measures. The longest a tearing-down instance held its vector anywhere in the
corpus falls from **189 ticks to 10**, the total instance-ticks spent tearing down
falls by three quarters, and completed jobs rise by about a fifth on every arm.
Every number this record quotes is measured on the repaired world, because the
old one was answering a different question.

**A repository slot is held through `draining` too**, so a repository at its cap
waits for its drain to reach deregistration before the next job of that repository
is admitted. That is one edge on an ordinary teardown, and it is included in the
throughput measured above.

**The seam under the second clock is filed, not fixed.** `sqlite.Store` reaching
for `time.Now()` where its siblings take an injected clock is
[issue #249](https://github.com/vitalyiegorov/tart-runner-fleet/issues/249);
until it is closed the harness compensates by claiming on the later of the two
instants, which is what production does with one clock.

> **Closed.** Issue #249 gave `sqlite.Store` the injected clock at all five write
> sites, the harness now claims on its virtual instant alone, and the
> compensation above is gone. The numbers in this record were measured under the
> compensation and are unchanged by its removal in behaviour, but the corpus
> digests moved: the harness no longer skips a retry backoff, and a store that
> stamps `created_at`, `updated_at`, and `available_at` on the virtual clock
> writes different durable instants than one that stamped them thirteen days
> ahead. See ADR 0031 for the rule the simulation now states.

## The three questions

**Can I remove overengineering?** Yes: this record deletes release paths rather
than adding a mechanism. `ConsumesHostResources` gains one comparison and
`activeRepoCounts` loses a case; between them they lose a whole class of premise.
There is no new state, no new field on `domain.Instance`, no new configuration, no
new durable column and no new oracle. The four alternatives that would each have
added one — release at `deleted`, a shadow capacity state, a phase-aware release,
a latched release — are recorded above with why they were not taken.

**Can I reduce complexity?** Each rule is stated once, in the predicate its
consumers already read, in the same place ADR 0042 put its bound and for the same
reason: a rule about what capacity is free must not be re-derived by each caller
that wants some. The two predicates are deliberately not merged — they answer
different questions ("is this machine still executing?" and "can GitHub still hand
this runner a job?") and the states already distinguish them. The only genuinely
new production symbol is a clock field that already existed, spelled the same way,
on the object beside it.

**Can I test this?** Both paths of the vector defect are pinned deterministically
and both were proved red first: `TestATearingDownInstanceDoesNotLendAVectorItStillHolds`
(the reading corrects itself — seed 8's shape reduced to two instances and one
vector) and `TestAnAbortedRecoveryNeverLentItsVector` (the drain aborts, which is
the path the issue states). Both assert the world reached the incident state — the
harness really lied, the drain really aborted — because a green run over a trace
that never lied would prove nothing. The repository-slot half is pinned by
`TestTeardownStatesStopConsumingRepoAdmissionCapAtDeregistration` and was found by
the sweep, not by inspection.

**And the harness could not reach any of it.** Four separate world-model gaps had
to be closed before the sweep could judge this rule at all: the corroborator's
clock, the store's availability clock, the two mirrors reading one demand record,
and — from ADR 0042 — the fault itself. Each was labelled by #242's taxonomy
before it was fixed: in all four the fleet was right, the oracle was right, and the
simulated world was the thing that could not be reached or could not be built.
Extending the harness was part of the fix rather than an optional extra, and
`misreported_power` now lands in the generator's draw and in
`TestGeneratedTraceExercisesTheWholeWorld`'s required table, which is the
acceptance test this record is judged by.
