# ADR 0084: A gated node keeps no standing offer

## Status

Accepted. Issue [#428](https://github.com/vitalyiegorov/tart-runner-fleet/issues/428).
Amends [ADR 0082](0082-a-bound-scale-set-keeps-a-standing-offer-of-one.md): the
standing free slot is withheld while the node's own guardrails refuse admission
for host pressure. Nothing else in ADR 0082 or
[ADR 0073](0073-scale-sets-advertise-the-residual-envelope.md) changes.

## Context

ADR 0082 gave every bound scale set a standing free slot,
`min(configured, max(held + fit, placed + 1))`, so that a job queued while every
matching node was full would still be assigned somewhere (issue #422). It
accepted the cost: a full node may be assigned one job per set that it cannot
start yet, even if a sibling has room.

Issue #428 measured that cost three times:

| When (UTC) | Assigned to | Why it could not start | Sibling with room | Waited |
| --- | --- | --- | --- | --- |
| 2026-10-25 21:10 | MacBook (2 slots, 14 GiB) | both slots full; a 4x8 does not fit beside them | Omarchy and Ubuntu idle from about 21:45 | 44 min |
| 2026-10-25 22:12 | MacBook | both slots full | Ubuntu 2 of 4 slots, 47 GiB free, admitting | 13 min and more |
| 2026-10-26 06:26, 06:47 | Omarchy | **admission refused: memory reserve** (about 2.7 GiB available, the owner's own workloads) | ubuntu-vitalii idle, admitting | still waiting at 06:49 |

The first two are a full node. The third is a different case: a node whose
own guardrails have closed. Its idle sets still offered one, because ADR 0082
measured the slot from `placed` and never asked whether the node could admit at
all.

### Why session yield did not cover it

[ADR 0047](0047-a-node-that-cannot-admit-yields-its-sessions.md) already makes a
node that cannot admit release its sessions, so GitHub stops assigning to it.
But it withdraws only after admission has been refused continuously for
`sessionYieldBlockedForSeconds` (default ten minutes), and never while a guest
is running or an operation is busy, because the sessions are also how running
work reports completion. A node gated by its owner's workloads often still runs
guests, and a gate that opens and closes restarts the clock. Until then, the
standing offer kept inviting work. Yield is the large tool (close every session)
for a long refusal. The standing offer is a small tool (one slot per set) that
should never have been offered by a node that refuses admission in the first
place.

## Decision

`ScaleSetHeadroom` carries `Gated`, set from the existing `hostHealthy`
predicate: a fresh host observation whose guardrails refuse admission. When a
set is gated, `AdvertisedCapacity` is `min(configured, held + fit)`. That is ADR
0073's value, with no standing slot.

- **Gates that withhold the slot:** `memory reserve`, `swap pressure`,
  `cpu pressure`, `disk reserve`, `inode reserve`. These are the reasons
  `executor.Guardrails.Evaluate` gives.
- **Update drain keeps the slot.** `hostHealthy` already defines a drain as
  not host pressure, and this ADR reads the same predicate rather than
  restating it. A drain is chosen by the node, bounded by `maxWait`, and ends
  in a restart. In #422's 17:05Z reproduction, two of the four nodes were in
  update drain or the swap gate. A drain is the gate most likely to close on
  several nodes together, and so the most likely to leave a job no set takes.
- A gated set still advertises what it holds. Work GitHub has already given it
  is never taken back.
- Unknown headroom, including a stale or unavailable host probe, still
  advertises the configured value (ADR 0073). `Gated` exists only on a usable
  observation.
- Nothing about admission, the scheduler or session yield changes. The rule
  decides only what GitHub may assign.

### The #422 interaction

If **every** node that serves a job's labels is behind a host-pressure gate when
GitHub queues the job, no set offers room and the job is assigned to no set, as
in #422 (`TestIssue428EveryMatchingNodeGatedLeavesTheJobUnassigned` records
this). The rule accepts that for three reasons:

1. **#422's own incidents do not reach it.** In every reproduction at least one
   matching node was full or draining, not pressure-gated. At 15:33Z and 16:15Z
   Omarchy was saturated (ubuntu-vitalii was the inode-gated one). At 17:05Z
   Ubuntu was full and two nodes were in update drain or the swap gate. A full
   or draining node keeps its slot, so each of those jobs would still have been
   assigned.
2. **ADR 0047 already accepts it in a stronger form.** A gated, idle node
   withdraws its sessions entirely. If every node does that, the same job is
   assigned nowhere. This ADR withholds less (one slot, not the sessions) and
   reaches the same state no more often.
3. **The cost on the other side is measured, not hypothetical.** A gated node
   with a sibling idle is the ordinary shape on a shared fleet. A gate held by
   the owner's workloads can last for hours, and every job it attracts waits
   for that time while the sibling is idle.

A job that no set takes still needs an alarm: a `doctor` check for a REST job
assigned to no set, already owed by ADR 0082. This ADR makes that check more
useful but does not build it.

## Rejected

- **Withhold the slot on a full node whose free vector cannot fit beside its
  running shape** (#428's MacBook cases). On a full node this is the same
  condition as `fit == 0`. Withholding the slot there is ADR 0082 reversed,
  and #422's 15:33Z reproduction, with Omarchy saturated and the only node
  that could take the job, would strand again. A node cannot see whether a
  sibling has room, so it cannot tell "small and full" apart from "the only
  hope". The MacBook cases stay ADR 0082's accepted cost.
- **Keep the slot for a bounded time after the gate closes.** This needs a new
  clock and new state. It is also backwards: the longer a gate holds, the more
  likely it is to be the owner's own load, and the worse it is to attract work.
  Session yield already supplies the bounded, long-refusal answer.
- **Release or decline jobs already acquired by a gated node.** No existing safe
  mechanism moves an assigned job. ADR 0047 measured that a closed session does
  not move it either, and a cancel turns a fleet capacity problem into a
  consumer's red check. This is left for the hub (issue #168), as ADR 0047
  leaves it.

## Consequences

- `TestAdvertisedCapacityOnlyLowersAndNeverBelowWhatIsHeld`: a gated idle set
  advertises 0, and a gated busy set advertises what it holds.
- `TestScaleSetHeadroomMarksANodeItsOwnGuardrailsRefuse`: each host-pressure
  reason gates. `capacity available` and `update drain` do not.
- `TestIssue428AGatedNodeLeavesTheJobToAnIdleSibling`: a job queued while the
  node is behind its memory reserve goes to the sibling. Once the gate reopens,
  the next job is assigned here and runs.
- `TestIssue428EveryMatchingNodeGatedLeavesTheJobUnassigned`: the accepted
  trade-off, stated as a test.
- Every #388, #397 and #422 simulation is unchanged and green. None of them
  gates its node.
- A node whose own guests push it under its memory reserve counts as gated, not
  merely full, and loses the slot too. That is the correct reading for that
  node: it cannot start the next guest either way. It does mean "full" in the
  #422 analysis above means slots or envelope, not the memory gate.
