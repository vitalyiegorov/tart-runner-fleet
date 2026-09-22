# ADR 0057: A scope that holds a slot yields the next one

## Status

Accepted 2026-09-22. Amends ADR 0004's aged global FIFO, ADR 0012's shared
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
- **Demands of different profiles keep their relative places exactly.** Asking
  which scope holds more slots is only meaningful between demands that want
  the same slot, and confining the key there leaves the band's aged FIFO
  across vectors, the reserved head it mints, and the handoffs that hang off
  it untouched.
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
  frees next, instead of behind another scope's whole backlog.
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
