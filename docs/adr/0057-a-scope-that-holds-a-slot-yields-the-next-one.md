# ADR 0057: A scope that holds a slot yields the next one

## Status

Accepted 2026-09-22. **Amended 2026-09-22 (issue #350, see "Amendment" below):
occupancy is the HOST VECTOR a scope holds, not the repository cap slots it is
charged. The two differ on four observations, and on every one of them a scope
occupying a core read as holding nothing.**

Amends ADR 0004's aged global FIFO, ADR 0012's shared
capacity ordering, and the no-jump clause of ADR 0017/0038, in the one case
named below. ADR 0005's repository round-robin, ADR 0037's declared tier and
escalation, and ADR 0049's aged head are unchanged.

## Context

Apple's Virtualization framework allows two guests per host, so each Mac has
exactly two job slots (ADR 0055). Both Macs serve more than one GitHub scope.

Measured on the mini and the studio, 2026-09-21/22: the `budgie` scope queues
30-to-60-minute Maestro jobs in batches and refills them continuously; the
`pony` scope queues a single 20-minute job. At 03:32Z the mini's queue read
one `budgie` maestro job 36 minutes old and three `pony` maestro jobs
**3 h 01 m** old, with `budgie` holding both slots — and it went on holding
them. The same shape ran 3–6 hours on both hosts.

The ordering rule is in `internal/scheduler/scheduler.go`. `priorityOrder`
splits the queue at `FairnessAge` into an aged band and young lanes:

- the **young lanes** have round-robined repositories since ADR 0005
  (`throughputOrder` buckets by resource vector, `fairOrder` round-robins
  repositories inside a bucket);
- the **aged band** was pure FIFO by creation time, with a declared tier over
  it (ADR 0037) and a lexicographic key as the final tiebreak.

On a two-slot Mac running half-hour jobs, `FairnessAge` (5 minutes in
production) is crossed by everything within minutes, so the one band that had
no fairness key was also the only band that ever ran. A scope that queues a
batch then owns every slot until the batch drains, because a batch that
arrived first is older than anything another scope will ever queue. Age cannot
separate the two: both scopes are aged, and the older one is the one already
running.

The diagnosis is a **fleet defect**, not a property or world-model defect. It
reproduces from the pure scheduler with no adapter, no clock skew and no
GitHub involvement: `TestScopeFairShareBoundsTheWaitBehindAStreamingScope`
measured 61 ticks of wait for the second scope behind a batch of six, and the
wait is unbounded in the size of the batch and the length of the stream.

**It is not the only defect that window carried, and this decision claims only
its own half.** PR #347 found a second one in the same hours: with no Linux
demand queued — the steady state of a macOS-only node under ADR 0055 — a macOS
head that could not spawn (a `builder` that does not fit beside a live
`maestro`) returned the attempted plan unchanged, so a feasible `maestro`
behind it was never admitted at all. The mini's queue at 03:32Z carried that
arrangement too. The two are independent, and the evidence that they are is
that every reproduction here fails identically against #347's fixed planner:
the wedge is "nothing is admitted", this is "a slot IS admitted and goes back
to the scope that already holds the node". Neither decision fixes the other's
defect, and this one adds nothing to `planTick`'s arms.

## Decision

Inside the aged band, among the demands of **one profile** — the demands
competing for the very same slot — order by the fair-share rank of the
demand's **scope** before ordering by age:

```
rank(demand) = instances this node currently runs for the demand's scope
             + demands of that scope earlier in this band, any profile
```

Lowest rank first; a declared tier still comes first of all; age remains the
order within one scope and the tiebreak between equal ranks.

The rank is counted over the whole band while the reordering is confined to one
profile, and both halves are load-bearing. Counting over the whole band is what
keeps a scope's own FIFO intact across vectors: a `builder` this scope queued
earlier raises the rank of its `xl`, so the key can never lift one demand of a
scope past an older demand of the same scope. The simulator proved that
necessary -- seed 2, tick 47 of the mini arm, a scope's younger Linux `xl`
taking the vector the same scope's older macOS `builder` was waiting for, which
is property (r)'s cross-platform inversion.

- **Scope** is the owner of the `owner/repo` slug — the GitHub scope a scale
  set is registered in (ADR 0034).
- **Occupancy** is the one `activeRepoCounts` already charges repository caps
  against, folded to the scope. Nothing new is measured, and a teardown
  releases its scope's share at the same edge it releases its repository slot
  (ADR 0043). The reserved head's own synthetic charge is excluded: it is a
  cap bookkeeping entry for work that has not started, and reading it as
  occupancy would make a scope yield the vector its own head is waiting for.
- **Only demands of one profile are ever exchanged.** Asking which scope holds
  more slots is only meaningful between demands that want the same slot, and
  confining the key there leaves the band's aged FIFO between vectors, the
  reserved head it mints, and the handoffs that hang off it as they were.
- **The platform boundary is a barrier.** A demand is never reordered past a
  demand of the other platform: ADR 0049 owns that boundary, the two platforms
  share one envelope, and a demand that crosses it takes a vector rather than
  a turn. The simulator said so twice before this clause existed -- seed 82
  tick 104 and seed 2 tick 47 of the mini arm, both property (r) -- and a rule
  that has to be exempted from another property is a rule stated wrongly.
- The queue-position term of the rank is what holds on the tick that frees
  several slots at once — the shape a batch produces, since a batch starts
  together and ends together. Occupancy alone reads zero for every scope on
  that tick and hands the whole node back to the batch.

**No new configuration.** `targets[].maxActive` already reaches the scheduler
as `RepoCaps` and is already effective; it bounds one repository's
concurrency, which is a ceiling, and a ceiling above the node's slot count
(4 against a Mac's 2) can never order two scopes against each other. Fair
share is an ordering key, not a cap, and needs no knob.

The one case where this amends the no-jump clause of ADR 0017/0038: an
equal-or-larger demand MAY now take the vector a cap-held reserved head is
holding when that head's scope already holds more of this node than the
candidate's does. It is bounded by its own key — the candidate's scope holds a
slot the moment it is admitted, so the next tick reads the key the other way.

Authority, observe and shadow limits are untouched: this is a planning-order
change inside the pure scheduler and changes no operation, no API and no
promotion gate.

## Consequences

- A scope with queued work waits at most one job length behind the slot that
  frees next, instead of behind another scope's whole backlog -- **when the two
  scopes ask for the same profile, are not separated in the band by a demand of
  the other platform, and the incumbent holds no higher effective tier.** All
  three are conditions of the key, not caveats about it, and an operator reads
  them before calling a wait a defect (`docs/AGENT_RUNBOOK.md`,
  `docs/OPERATIONS.md`).
- Throughput is unchanged: the same slots are filled on the same ticks by the
  same number of guests. Only the owner of the next one changes.
- A scope that is the only one queuing sees byte-for-byte the old behaviour:
  one scope means one rank sequence, which is age order.
- The simulation harness gained a multi-scope batch workload and the
  fair-share property (`tests/simulation`), so the regression is generated,
  not only asserted.

## Alternatives rejected

- **A per-scope `maxActive`.** A cap cannot express "yield the next one": set
  to 1 it halves a quiet node's throughput, set to 2 it is inert. The
  three-questions rule asks for one rule, not one more knob.
- **Round-robin across the whole aged band, regardless of vector.** It
  reorders demands that were never competing, and it moved the reserved head
  of ADR 0017 in cases that have nothing to do with fairness.
- **Shrinking `FairnessAge` so the young lanes' round-robin runs.** That
  demotes the starvation guard itself, and on half-hour jobs no threshold
  keeps a 20-minute queue "young".

---

## Amendment, 2026-09-22 -- occupancy is the vector, not the cap (issue #350)

### The live trace

`v0.1.609` on the mac mini, with #345 (this ADR) and #347 both deployed.

```
13:45Z  pony's running job is cancelled by its owner; pony holds 0
        instances for the rest of the window.
13:46Z  budgie starts "iOS Maestro E2E / Maestro shard 0" on runner
        trf-maestro-0ef249257cb3ca65. It runs until 15:00Z.
fleet queues through the window:
        pony    maestro  set 7   2 jobs  created 13:07:34Z, 13:07:58Z
        budgie  builder  6x12    1 job   created 12:46Z
        budgie  maestro  4x7     1 job   created 11:37Z
14:16Z  the OTHER slot frees.  IT GOES TO BUDGIE'S 11:37Z MAESTRO.
15:00Z  shard 0 ends.          The next slot goes to budgie's 12:46Z builder.
```

The mini's `hostBudget` is 10 vCPU / 23 GiB, so it runs **two** 4x7 maestro
guests side by side, and budgie was demonstrably holding one of them at 14:16Z.
That is precisely the arrangement this ADR's occupancy term exists to decide:
one profile, both scopes hours past `FairnessAge`, one scope on a core and one
on none. It decided it the wrong way, for hours.

The 15:00Z decision is the rule working and is unchanged: `builder` and
`maestro` are different profiles, only demands of one profile are ever
exchanged, and pony queued no builder.

### The cause: one function, two questions

The rank asked `activeRepoCounts` and folded it to the scope, on the reasoning
that "holds a slot" should be stated once. It is the same words about two
different questions:

- `activeRepoCounts` answers **"how many of this repository's concurrent cap
  slots are spent?"**. ADR 0043 releases that slot EARLY, at deregistration,
  precisely so a cap cannot block a replacement the fleet has already committed
  to. Releasing early is the whole point of it.
- fair share asks **"how much of this node does this scope hold?"**. The answer
  is the host vector, which `ConsumesHostResources` already names, and which the
  same instance goes on holding for one or two lifecycle edges longer.

They disagree on four observations, and in every one of them a scope occupying a
core reads as holding nothing:

| observation | holds a core | charged to the cap |
| --- | --- | --- |
| `online-idle` (a warm runner between jobs) | yes | no |
| `deregistering` (ADR 0043's early cap release) | yes | no |
| `stopping`, before the guest is proven idle | yes | no |
| `failed` | yes | no |

With the occupying scope read as zero, both scopes rank zero, the tie falls
through to ADR 0004's aged FIFO, and FIFO gives it to whoever queued first --
which, for a scope that has been streaming work for hours, it always is. The
mismatch runs the other way too: a `draining` guest whose VM is proven absent
holds no vector and was still counted.

**Label: a fleet defect, with a property defect beside it.** It reproduces from
the pure scheduler with no adapter, no clock skew and no GitHub involvement
(`tests/replay/scope_vector_hold_incident_test.go`,
`internal/scheduler/scheduler_scope_vector_hold_test.go`). The simulation's
`scopeOccupancy` oracle read the same cap edge, so it could not judge the tick
either -- the same seam #345's own sweep found, moved the ORACLE to ADR 0043's
edge, and labelled an oracle defect. The direction was right; it stopped one
edge short and left the fleet's reading unchallenged.

**Two readings are refuted and recorded as refuted:**

- *the reserved/infeasible `builder` head minted a reservation whose backfill
  re-derived its own ordering.* It did not. Reservations are authored over Linux
  demands, the mini's queue was entirely macOS, `plan.Next.Reservation` is nil
  on the tick, and striking the builder row out leaves the decision identical.
  Since #345 every pass reads one band order through `agedQueueOrder`.
- *fair share collapsed because the node frees one slot at a time.* It does not:
  the mini's budget fits two maestros and budgie was holding the second. The
  single-slot story was built on the studio's 6 vCPU budget and does not
  describe this incident.

### The decision

```
rank(demand) = held(scope) + demands of that scope earlier in this band, any profile

held(scope)  = instances of the scope that are holding this node's vector
               (`ConsumesHostResources`), the reserved head's synthetic
               cap charge excluded.
```

Everything else is unchanged: the tier still comes first, age is still the order
within one scope and the tiebreak between equal ranks, only demands of one
profile are exchanged, and the platform boundary is still a barrier.

The bound is unchanged too, and is now the honest one. A scope stops holding
this node when its guest stops charging the host -- `stopping` with the guest
proven idle, or a VM proven absent (ADR 0022) -- and the slot it releases on
that tick is the one being given away, so it is right that it counts for
nothing. The reserved head's own charge stays excluded: it is cap bookkeeping
for work that has not started, and reading it as occupancy would make the head's
scope yield the very vector the head is waiting for.

### Why the simulation did not catch it

Both a property defect and a generator gap, and neither alone would have been
enough.

- **Property/oracle defect.** `scopeOccupancy` read the cap edge, so a scope
  visibly on a core looked empty to the judge as well as to the fleet, and the
  oracle excused the very tick it exists to catch. It now reads
  `ConsumesHostResources`, the same fact the fleet reads, because a rule and the
  oracle that judges it may not disagree about what "holds a slot" means.
- **Generator gap.** Every scope-fairness world admitted a scope's guests on the
  same tick, so its guests walked the teardown in lockstep and were never in two
  different states at once; and no trace ever wedged a teardown in a world where
  fairness between scopes was the question. `wedgedTeardownScopeShareTrace` is
  the intersection: the streaming scope holds both slots, both jobs end
  together, one guest is held in `deregistering` by a guest that will not power
  down (issue #233) while the other hands its slot back, and the scope's own
  queued work is older than the waiting scope's. On the merge base the freed
  slot goes back to the scope on the core, at tick 25.

### Consequences of the amendment

- A scope occupying a core yields the next slot, in every observation of that
  core -- including the teardown ticks, which are the ticks a slot changes hands.
- A scope whose vector is genuinely released holds nothing, which is what lets
  the slot it released be given away at all.
- Nothing else moves: no new state, no new configuration key, no new pass, no
  new axis. `activeRepoCounts` is untouched and ADR 0043's early cap release is
  untouched; fair share simply stops borrowing an answer to another question.
- Authority, observe and shadow limits are untouched: this remains a
  planning-order change inside the pure scheduler.
