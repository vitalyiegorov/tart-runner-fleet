# ADR 0082: A bound scale set keeps a standing offer of one

## Status

Accepted. Issue [#422](https://github.com/vitalyiegorov/tart-runner-fleet/issues/422).
Amends [ADR 0073](0073-scale-sets-advertise-the-residual-envelope.md) (the
advertised value is floored at one) and
[ADR 0076](0076-a-nodes-residual-is-offered-once.md) (the issue #412 floor in
`scheduler.PartitionHeadroom` is removed; this record subsumes it).

## Context

Since ADR 0073 each scale set advertises `min(configured, held + fit)`. A node
that is full, draining for an update, or behind the swap or CPU admission gate
has a residual of nothing, so `fit` is 0 and every set on it that holds nothing
advertises **0**. The issue #412 floor did not help: it offered one only when
the residual could start one instance, and a full node cannot.

GitHub assigns a job only to a scale set advertising room when it queues the
job, and does not look again. On 2026-10-25 every node serving the budgie and
rnw-community Linux labels was full or gated at once, three times:

- 15:33Z: two `trf-linux-amd64-2x4` jobs, while ubuntu-vitalii was out of
  inodes and Omarchy was saturated, stayed queued for more than 90 minutes.
  Both nodes recovered within the hour, and Omarchy then sat at occupancy 1
  with the #412 floor offering 1.
- 16:15Z: the same shape again.
- 17:05–17:12Z: sixteen budgie re-run jobs, while Omarchy and the mini were in
  update drain or the swap gate, Ubuntu was full, and the MacBook was
  swap-gated. `fleet scale-sets audit` showed `available=0 assigned=0` on every
  budgie-org Linux set of all four nodes.

An earlier rnw job queued at 14:54Z, with the same nodes busy, was placed six
minutes later. So the trigger is not "a node was full". It is "every set that
matches the job's labels advertised 0 when GitHub queued it". A job queued
against a set advertising room that is all assigned waits at GitHub as ARC's
ordinary `maxRunners` queue does and is assigned when the set frees. A job no
matching set could take is assigned to none.

Nothing in the fleet saw these jobs. A shared-label set counts a REST-observed
job as its own only once GitHub has assigned it there (`boundToOwnShare`, issue
#153). A job assigned nowhere is therefore trimmed from every node's share,
and `doctor` stayed green.

## Decision

`app.AdvertisedCapacity` is

    max(1, min(configured, held + fit)), never above configured

whenever the node's headroom is known. Concretely:

- A set holding nothing on a full node advertises **1**. A job its labels match
  is assigned here and waits in this node's queue until admission starts it,
  which is ordinary queueing.
- A set holding anything advertises at least what it holds, as before. The
  floor adds nothing to it, so a full node takes **at most one job per set that
  it cannot start yet**.
- A set configured at 0 still advertises 0.
- Unknown headroom keeps the configured value (ADR 0073's fail-open rule).
- Admission, the scheduler, and every guard are unchanged. The floor decides
  only what GitHub may assign, never what starts. A burst still starts no more
  than the residual allows (#397).

The floor is in one place: the advertised value. The issue #412 floor in
`scheduler.PartitionHeadroom` is deleted. It offered one to a set the division
left at zero, but only when one instance of that set's vector fitted the
residual. For a set holding nothing it now gives the same 1 that the
advertisement floor gives. For a set already holding work it offered one beyond
`held`, and that was never what #412 was about: #412 was about sets reading
zero, and a set holding work never reads zero. Without the #412 floor, `fit` is
once again exactly the round-robin division of what the residual can start at
once, and property (t) again bounds the **sum** of a macOS profile's shares, not
each share separately.

## Trade-off

ADR 0073 (issue #388) set out to stop a busy node collecting work it cannot
start while its siblings are idle. This record gives back a bounded part of
that. While a node is full, each of its sets that holds nothing may be assigned
one job that waits for this node, even if a sibling has room at that moment.
With fourteen sets on an amd64 node, that is at most fourteen waiting jobs,
one per set. #388 hoarded without bound.

The alternative is worse. A job that every matching set refused stays queued
until a human cancels and re-runs it, and later runs of the same branch wait
behind it on their concurrency group. A job assigned to a full node starts as
soon as that node frees a slot. A node cannot see whether its siblings have
room, so it cannot advertise 0 only when another set would take the job.

## Rejected

- **Floor `fit`, not the advertisement** (`held + max(1, fit)`): each newly
  assigned job raises `held` again, so a full set would keep inviting one more
  job. That is #388's unbounded hoarding again.
- **Report an unassigned REST job in `doctor`**: a job queued in a scope that
  matches a served binding but assigned to no set, for longer than a bound,
  is the missing alarm. It needs a new per-scope "assigned to no set" count
  carried from REST reconciliation into the admin DTO, and a threshold. That is
  not small, and the floor removes the cause, so it is left for a separate
  issue.

## Consequences

- `TestIssue422AJobQueuedWhileEveryNodeIsFullIsAssigned`: the shared-label Mac
  is full and every sibling is full too (`SiblingsFull`). Both jobs that arrive
  beside the long suite are assigned here and run after it. Before this change,
  both were assigned to no set.
- `TestIssue388SmallNodeStopsAdvertisingAVectorItCannotStart` now allows one
  waiting 4x8 job per set beside the maestro. None of them starts beside it, and
  the scope's second job still goes to the sibling.
- The #397 burst still lands at most one job per scope on the busy node.
