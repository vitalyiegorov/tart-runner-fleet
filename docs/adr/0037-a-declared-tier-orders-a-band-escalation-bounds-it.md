# ADR 0037: A declared tier orders a band; escalation bounds it

## Status

Accepted. Refines the scheduling order of
[ADR 0004](0004-bounded-control-plane-priority.md) by adding a key *inside* each
of its bands; ADR 0004's band structure — aged global FIFO, then young
control-plane, then young standard — is unchanged and still outermost. The
cross-platform aged FIFO of
[ADR 0012](0012-shared-cross-platform-capacity.md), the reservation contract of
[ADR 0017](0017-infeasible-reservation-residual-backfill.md) as amended by
[ADR 0029](0029-remainder-admission-behind-a-reservation.md) and
[ADR 0030](0030-a-reserved-head-holds-one-repository-slot.md), the
one-admission-per-tick rule of
[ADR 0027](0027-one-tick-admits-a-demand-once.md), and the repository caps and
profile `MaxActive` bounds all remain accepted unchanged and still bind every
demand admitted here.

## Context

### The incident, 2026-08-09

The owner asked for a store release to be prioritised: `vitalyiegorov/suuudokuuu`
run 31327479374, "Build and Publish to Stores" — the App Store and Play Store
submit jobs, on `macos-builder`. Both had waited over an hour.

When the mac mini's single builder slot finally freed, the fleet was about to
hand it to `budgie-at/budgie`'s "Build iOS E2E app" — a pull request's test job
that happened to be **sixteen minutes older**.

Nothing was broken. Aged FIFO is correct as a fairness rule and it was working
exactly as designed. It simply has no way to express that a store release
outranks a pull request's E2E build. Every fact the fleet had about the two jobs
said they were the same kind of work.

The only lever available was manual: cancel the other repository's two queued
runs so the release became the head of the queue, then re-run them afterwards.
That is an operator standing at the console overriding the scheduler by hand. It
was the second time that shape of request had come up. It does not scale, it is
not auditable, and if nobody is watching, the release simply waits.

### What was missing

Two things, and they are inseparable.

1. **A way to say a class of work outranks another.** The fleet already has one
   priority distinction — ADR 0004's control-plane class, which exists so the
   fleet can repair itself — and it is deliberately not a general mechanism: it
   is one bit, keyed on the repository, for the fleet's own repository. Nothing
   expresses "this workflow is a release".
2. **A bound on what that costs everything else.** ADR 0012 refused an unbounded
   numeric priority for exactly this reason, and it was right to. A tier with no
   bound is a licence for one class of work to starve another indefinitely, and
   a scheduler that can be configured into starvation is a worse outcome than
   one that cannot express a release.

## Decision

### 1. Configuration declares an ordered list of named tiers

```jsonc
"priority": {
  "escalateAfterSeconds": 1800,
  "tiers": [
    { "name": "release",
      "match": [ { "workflowRef": "*/.github/workflows/release*.yml@*" },
                 { "jobName": "*Publish to Stores*" } ] }
  ]
}
```

Tiers are ordered highest first, and the rank is the position in the list. An
operator never writes a number, which is ADR 0012's rule kept: the vocabulary is
names, and no caller can invent a rank of its own.

A tier claims a demand when **any** of its rules holds; a rule holds when
**every** facet it declares holds. The facets are `scope` (the repository slug),
`workflowRef`, and `jobName` — the three facts a scale-set `JobAvailable`
message already carries, so classification never fetches anything, never fails,
and never goes stale. Patterns are anchored, case-insensitive, and use `*` as
the only metacharacter; they are deliberately not regular expressions, because
an operator's configuration must not be able to make the planner do unbounded
work.

A demand that matches nothing lands in the **default tier**, rank zero — which
is the zero value every demand already had. Classification happens exactly once,
where a durable inbox row becomes a schedulable demand.

### 2. The tier orders a band; it does not reorder the bands

`priorityOrder` sorts by **(tier, age) inside each of ADR 0004's bands**:

| band | rule |
| --- | --- |
| aged global FIFO | tier first, then age |
| young control-plane | tier first, then the existing throughput and round-robin lanes |
| young standard | tier first, then the existing throughput and round-robin lanes |

Aging stays the outermost key. ADR 0004 calls it the absolute starvation guard,
and this decision does not demote it: a declared tier decides between demands
that have waited comparably, not between a fresh job and one already past the
fairness age. The control-plane lane likewise stays above the tier — the fleet's
own repair path is not application work and does not queue behind a release.

That is enough for the incident, because the incident's two demands were both
aged: the release had waited 65 minutes and the E2E build 81, against a
five-minute fairness age. Both were in the aged band, and inside that band only
the tier could tell them apart.

It is also the composition the rest of the planner already assumes.
`planLinux` re-derives the aged/young split from the list `priorityOrder` hands
it, so a tier that reordered the *bands* would be silently discarded there. The
simulator found exactly that shape on the first seed of the tiered arm before
this composition was settled.

`exactSelect`'s band vector is composed the same way — ADR 0004's band major, the
tier minor, tiers compressed to the ranks actually present — so the candidate
list and its bands still say one thing, which is what ADR 0031's finding 3
established they must.

### 3. Escalation is mandatory, and it is the bound

A demand's **effective tier** is the rank it was classified with plus one rank
for every whole `escalateAfter` it has waited:

```
effectiveTier(d) = d.rank + floor(wait(d) / escalateAfter)
```

This converts "a tier may overtake" into "a tier may overtake for a bounded
time". A demand of the default tier is outranked by a tier of rank `N` only
while its own effective tier is lower, and it gains one rank per threshold, so
**one declared tier costs at most one threshold of extra waiting, and `N` tiers
at most `N` thresholds.** The bound is written in the configuration file rather
than left to luck.

Escalation is monotonic by construction — `floor` never falls — and demands of
one tier escalate together, so age order inside a tier can never invert.

`fleet config validate` refuses tiers without a threshold **and** a threshold
without tiers. The second refusal is not symmetry for its own sake: escalation
with no tier declared would still group demands by age band and would silently
reorder a fleet that declared no policy at all.

### 4. The reservation obeys the same order it was derived from

A reservation is made for the aged band's head (ADR 0017), so the test that
decides whether it *still* heads the queue has to be the aged band's own rule:
higher tier first, then older first. Comparing ages alone kept obeying a
reservation the tier order had already overtaken — the 2026-08-09 incident
returning through the mechanism that exists to protect the head. The simulator
found this too, on the tiered arm's first seed.

With no tier declared the tiers are equal and the comparison is the age
comparison it always was.

### 5. No preemption

A high-tier demand waits for the next free slot. It does not drain, kill, or
otherwise disturb a job in flight. The whole cost of the incident was queue
order, not occupancy; preemption is a separate decision with a separate blast
radius, and this one does not need it. (Occupancy budgets are issue #223.)

### 6. The classification is visible

`fleet queues` publishes, per scope queue, which tier its waiting demand landed
in — name, depth, and oldest enqueue time — over `fleet.v1` as an additive
`tiers` array and as a third table in the human view. A classification nobody
can read back is unauditable in exactly the moment it matters. The tier every
unmatched demand lands in is named `default`, and configuration reserves that
name so the vocabulary stays closed.

Both demand lanes feed the breakdown by the rule the aggregate already used: the
broker's delivered demand and REST's complete view, whichever is larger per
tier.

### Amendment 2026-08-19: a tier orders candidates, and ADR 0030 decides who is one

This decision has been corrected twice by the simulator and both corrections were
to the RULE: a tier that reordered ADR 0004's bands was silently discarded by
`planLinux` (§2), and a reservation made before an escalation went on being
obeyed after `priorityOrder` had stopped agreeing with it (§4). Issue #255 looks
like a third and is not. The rule is intact. What it needed was its boundary
stated — **a tier orders candidates; it does not create eligibility** — and the
two `tier_inversion` reports that made that necessary fall on opposite sides of
it: one is a property judging a decision the reservation rules are entitled to
make, and one is the reservation rules being applied wrongly.

Both are on the tiered arm's seed 82, and both were reached only once
`unreadable_power` entered the generator's draw
([ADR 0044](0044-an-unread-power-state-establishes-nothing.md)). Neither shrunk
trace contains a power event: the fault changed the history that reached the
state, not the state.

#### Tick 140 — an oracle defect

The tiered arm's seed 82, at tick 140, reported an inversion:

```
ops/fleet/1008  xl       59m0s   effective tier 11   <- reserved head
ops/fleet/1013  maestro  43m30s  effective tier 8    <- left waiting
b/repo/1021     maestro   7m30s  effective tier 2    <- took the slot
host available {CPU:5 MemoryMB:13312 Slots:4}; ops/fleet cap 2, one live instance
```

Every escalation above is exact — `0 + ⌊43.5/5⌋`, `1 + ⌊7.5/5⌋`, `0 + ⌊59/5⌋` —
`priorityOrder` ranked the three demands 11, 8, 2, and no band was reordered
anywhere. What decided the tick is
[ADR 0030](0030-a-reserved-head-holds-one-repository-slot.md)'s `slack <= 0`
clause, in its own words: *the head is waiting for the last free slot, or for one
that is already gone. Nothing from its repository may be admitted.*
`ops/fleet`'s cap is 2 with one live instance, so `slack = 2 - 1 - 1 = 0` and the
tier-8 demand was **not a candidate for that slot at all**. It shares the reserved
head's repository, and that head's slot is the one left.

**A tier orders candidates; it does not create eligibility.** That is §2 read
strictly: `priorityOrder` sorts a band, and every veto the planner already
applied — the resource envelope, `MaxActive`, the repository cap, and the two
conditions a held reservation imposes — still decides who reaches it. The demand
left waiting here is waiting for `ops/fleet/1008`, which outranks it on BOTH keys
this record orders the aged band by: effective tier 11 against 8, and 59 minutes
against 43m30s. The pass-over serves the tier order rather than inverting it.

Refusing `b/repo/1021` as well would not have advanced the tier-8 demand by one
tick — it was ineligible on every tier — and it would have held the residual
idle, which ADR 0004 forbids in as many words: *admission stays work-conserving:
the order decides who is served first, never that capacity is held idle.*

So the correction is to the **oracle**, and it is one term. Property (l) asks a
counterfactual — could the waiting demand have taken the slot the chosen one
took? — and its `capAllowsSwap` charged ADR 0012's repository cap but not
ADR 0030's reserved slot. It now charges both. The harness had already recorded
the gap on the other side of it: `reservedResidual` leaves that slot uncharged
DELIBERATELY, because there the answer decides admissibility and a narrower
answer blinds properties (a) and (b). Both are right, because the two oracles ask
different questions; a feasibility oracle errs safely by being wide, and a
counterfactual that ignores a written rule simply states something false.

The exemption cannot hide an inverted tier. A reservation is only ever held for
the demand `priorityOrder` ranked first in the aged band, by effective tier and
then age (§4), and it is re-derived the moment anything outranks it — so the
demand this term leaves waiting is always waiting for one that outranks it.
Property (b) is untouched and still counts these pass-overs with no signature and
no tier exemption, and property (n) still bounds every tier-based pass-over, so
an ADR 0030 hold that never ends still fails the sweep.

This is an **oracle defect** under
[ADR 0043](0043-a-released-vector-cannot-be-taken-back.md)'s taxonomy — property
(l) judged an ordering the reservation rules are entitled to produce — and
nothing in the planner changes for it.

#### Tick 298 — a fleet defect, in the rule the first one vindicated

With property (l) corrected, the same seed reported a second inversion 158 ticks
later — tier 13 waiting, tier 10 admitted — and this one is real.

```
reserved head  ops/fleet `xl`, vector withheld -> four cores left over
ops/fleet cap  2, nothing live: exactly one spare slot behind the head
took the slot  an ops/fleet `builder`, 6 CPU, the oldest of that repository
could it fit?  no
dropped        ops/fleet/1119 `maestro`, tier 13, fits the four cores exactly
admitted       b/repo/1139 `maestro`, tier 10, another repository
```

ADR 0030 states its slot as a COUNT. `reservedRemainderDemands` had turned it
into an ASSIGNMENT: it ranked the head's repository and gave the spare slots to
the top `slack` demands before asking whether any of them could be admitted. The
builder spent the budget and used none of it. That is the aged-FIFO inversion
ADR 0030 exists to end, and with tiers declared it is a tier inversion as well.
The slot is now charged where admissions happen, so a candidate that cannot be
admitted spends nothing; the fix and its reduction are recorded as the 2026-08-19
amendment to [ADR 0030](0030-a-reserved-head-holds-one-repository-slot.md).

So the two reports are the same boundary read from both sides. Tick 140 is the
scheduler obeying ADR 0030 and the oracle disbelieving it; tick 298 is the
scheduler misapplying ADR 0030 and the oracle right to say so. **A tier orders
candidates, and ADR 0030 decides who is one — which means ADR 0030 has to decide
it correctly.**

This amendment is recorded here rather than as a new record because it states the
boundary of §2, which is this decision's own rule. ADR 0037's escalation
arithmetic, its band composition and its reservation clause are all unchanged.

The draw entry for `unreadable_power` lands with these fixes rather than with
#252, which is ADR 0042's rule: a draw entry ships with the repair for what it
surfaces, so the sweep is never knowingly red.

## Consequences

**A fleet that declares no tier is unchanged, byte for byte.** Every demand is
rank zero, every band has one group, escalation is refused, and every ordering
function reduces to the code it was. This is not an argument, it is measured:
the deterministic-simulation corpus (ADR 0031) produces identical digests and
identical plan, admission, spawn, drain, and instance counts on all five
pre-existing arms across 64 seeds × 200 ticks.

**A fleet that declares tiers accepts a bounded, configured delay** on everything
below the top tier: at most `rank × escalateAfter` of extra waiting, on top of
whatever the queue would have cost anyway. An operator who wants a smaller worst
case lowers the threshold; one who wants the tier to matter more raises it. The
trade-off is explicit and it is one number.

**Ordering is all that changed.** The veto chain, the admission envelope,
`MaxActive`, repository caps, the reservation's vector and slack conditions, and
the one-admission-per-tick rule are untouched, and every pass still runs in the
order it ran in. There is no new pass.

**Classification cannot fail.** It reads three fields the message already
carried, with a pattern language that cannot backtrack. A configuration that
declares a tier nothing matches is inert, not broken.

## Evidence

- `internal/domain/priority_test.go`: the classification itself — declaration
  order sets rank, first match wins, every declared facet of a rule must hold, a
  rule with no facet matches nothing, matching is case-insensitive, and an
  undeclared policy returns the zero priority for every input.
- `internal/scheduler/scheduler_priority_tier_test.go`:
  `TestTheIncidentOf20260809` (the release takes the builder slot from the
  sixteen-minute-older E2E build), `TestAgedFifoIsUnchangedWhenNoTierIsDeclared`,
  `TestTierOrderIsRespectedWhenVectorsAreEqual` (three observation orders, one
  plan), `TestAgingRemainsTheOutermostKey`,
  `TestEscalationLetsADefaultTierDemandOvertakeABacklogOfReleases` (green with
  escalation, and the same queue hands the slot to the release forever without
  it), `TestEscalationIsMonotonic`, `TestEscalationIsInertWithoutAThreshold`,
  `TestAHighTierDemandWaitsForTheNextFreeSlot` (no drain is planned),
  `TestExactAdmissionCannotAdmitMoreLowTierWorkAheadOfAFeasibleHighTierDemand`,
  `TestAReservationYieldsToATierThatEscalatedAboveIt`, and
  `TestAReservationSurvivesEqualTierWork`.
- `internal/scheduler/scheduler_reserved_repo_slot_tier_test.go` (2026-08-19
  amendment): the tick issue #255 reduces to, both directions.
  `TestTheReservedHeadsLastRepositorySlotOutranksATierBelowIt` is seed 82's
  tick 140 as one `PlanTick` — it produces the identical plan the sweep reported,
  `plan-3a13529ee7cf2705e5b79331` — and
  `TestATierTakesTheResidualAsSoonAsTheReservedHeadCanSpareTheSlot` changes one
  number, the repository cap, and the tier-8 demand takes the residual. That
  second test is what makes the first a statement about ADR 0030 rather than a
  licence to ignore a tier.
- `internal/scheduler/scheduler_reserved_remainder_test.go` (2026-08-19
  amendment): `TestASameRepositoryCandidateThatCannotFitDoesNotSpendTheHeadsSpareSlot`,
  tick 298 reduced — red before the ADR 0030 amendment, where the six-core builder
  spent the head's one spare slot and the other repository took the vector.
- `tests/simulation/regression255_test.go`: the whole-fleet history that reaches
  both ticks, pinned as an explicit trace on the tiered arm.
- `tests/simulation/priority_test.go`: three new properties on a new arm
  (`tiered-release-priority`, two of four repositories declared as release work),
  plus the bound stated deterministically —
  `TestAnAdversarialReleaseStreamCannotStarveTheDefaultTier` runs a rolling
  backlog of aged release demands, one per tick, against a single waiting
  default-tier demand and one builder slot, and the waiter is admitted within one
  escalation threshold. With escalation removed the same test starves forever,
  which is what makes it a proof rather than a decoration.
- `internal/config/priority_test.go`: the wire round trip, the absent-key
  guarantee for a fleet with no policy, and every refusal — tiers without a
  threshold, a threshold without tiers, a reserved or malformed tier name,
  duplicates, a tier with no rule, a rule with no facet, and the bounds on tier
  count, rule count, and pattern length.
- `internal/adminapi/queue_tier_contract_test.go`,
  `internal/cli/queue_tier_test.go`, `internal/telemetry/queue_tier_test.go`,
  `internal/app/queue_tier_test.go`: the operator surface, including that the
  breakdown is absent — on the wire and in the table — for a fleet that declares
  no tier.

### New simulation properties (ADR 0031)

| id | name | statement |
| --- | --- | --- |
| l | `tier_inversion` | Two feasible demands of one platform, one resource vector, and one ADR 0004 lane are admitted in tier order — where "feasible" excludes a demand ADR 0030 makes ineligible, since the amendment above. |
| m | `escalation_regression` | A waiting demand's effective tier never falls. |
| n | `tier_starvation` | Escalation ends every tier-based pass-over within `T` ticks. |

Property (b), `bounded_starvation`, stops counting a pass-over that the declared
policy explains — an aged overtaker of a strictly higher effective tier — exactly
as it already stops counting the ADR 0017 reserved head. Property (n) is what
keeps that exemption honest, and a world that declares no tier never reaches
either clause, which is why the existing arms' histories are untouched.

## Not addressed here

- **Preemption.** A high-tier demand still waits for a slot to free. Draining a
  running job to make room is a different decision with a different blast radius.
- **Per-tier capacity reservations.** A tier orders the queue; it does not hold
  capacity aside for itself. That is the occupancy question of issue #223.
- **Cross-node tier arbitration.** Each node classifies and orders its own
  queue. Two nodes sharing a scope (ADR 0034) will each honour the policy their
  own configuration declares; nothing here coordinates them.
- **Tier-aware queue SLOs.** The queue SLO still measures the whole queue. A
  release-specific latency objective would be a separate telemetry decision.
