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
across the whole corpus — six arms, 64 seeds, 200 ticks, **6393 jobs per arm and
identical traces on both sides**, which is possible because the fleet change alone
does not touch the generator:

| arm | jobs completed | instance-ticks charged while tearing down | mean queue wait (ticks) |
|---|---|---|---|
| `m4-mac-mini` | 1475 → 1491 | 10946 → 11467 (+4.8%) | 47.75 → 47.68 |
| `mac-studio-4x10-budget` | 293 → 292 | 2660 → 2894 (+8.8%) | 69.49 → 70.03 |
| `geekom-linux-amd64` | 1529 → 1529 | 12100 → 12279 (+1.5%) | 52.79 → 52.69 |
| `federated-maestro-scope` | 760 → 748 | 5641 → 5924 (+5.0%) | 66.43 → 65.93 |
| `sequence-reset-linux-large` | 1260 → 1260 | 8663 → 8849 (+2.1%) | 64.39 → 64.40 |
| `tiered-release-priority` | 1376 → 1365 | 10535 → 10962 (+4.1%) | 49.30 → 48.92 |
| **total** | **6693 → 6685 (−0.12%)** | **50545 → 52375 (+3.6%)** | — |

Per teardown that is **6.80 → 7.06 ticks**: the average instance holds its vector
about **eight virtual seconds** longer than it did, and the queue behind it waits
neither longer nor shorter to a resolution this corpus can measure. The price of
never over-admitting the host is a quarter of a tick per teardown.

**Three of six corpus digests move, and no arm's counts do.** Plans applied,
spawns, drains and distinct instances are identical to the merge base on every
arm; what changed is when a plan was applied, not how much was admitted. The two
other changes in this record are attributed the same way: the corroborator's clock
alone reproduces the merge base **byte for byte on all six arms** (no corpus trace
carries a misreport until the draw entry lands, so no power run ever accumulates),
and adding `misreported_power` to the draw moves all six, because the draw index
shifts every fault every trace draws — the #237 precedent.

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

## The three questions

**Can I remove overengineering?** Yes: this record deletes a release path rather
than adding a mechanism. `ConsumesHostResources` gains one comparison and loses a
whole class of premise; there is no new state, no new field on `domain.Instance`,
no new configuration, no new durable column and no new oracle. The three
alternatives that would have added one are recorded above with why they were not
taken.

**Can I reduce complexity?** The rule is stated once, in the predicate every
consumer already reads, in the same place ADR 0042 put its bound and for the same
reason: a rule about what capacity is free must not be re-derived by each caller
that wants some. The one genuinely new line of production code outside that
predicate is a clock field that already existed, spelled the same way, on the
object beside it.

**Can I test this?** Both paths are pinned deterministically and both were proved
red first: `TestATearingDownInstanceDoesNotLendAVectorItStillHolds` (the reading
corrects itself, seed 8's shape reduced to two instances and one vector) and
`TestAnAbortedRecoveryNeverLentItsVector` (the drain aborts, which is the path the
issue states). Both assert the world reached the incident state — the harness
really lied, the drain really aborted — because a green run over a trace that
never lied would prove nothing.

The harness could not reach the second path at all until the corroborator's clock
was stated, which is a **world-model defect** of exactly the kind #242 asks to be
named precisely: the fleet was right, the oracle was right, and the simulated
world could not build the state. Extending the harness was part of the fix, not an
optional extra, and `misreported_power` now lands in the generator's draw and in
`TestGeneratedTraceExercisesTheWholeWorld`'s required table, which is the
acceptance test this record is judged by.
