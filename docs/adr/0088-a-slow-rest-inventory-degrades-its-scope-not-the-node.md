# ADR 0088: A slow REST inventory degrades its scope, not the node

## Status

Accepted. Implements
[issue #449](https://github.com/vitalyiegorov/tart-runner-fleet/issues/449).

It **amends [ADR 0052](0052-a-withdrawn-node-still-takes-a-release.md)** in one
clause — which stale critical observations `healthy` excuses — and leaves
`ready`, the release transaction's gates, and
[ADR 0011](0011-atomic-production-updates.md) standing. It keeps the budget
pacing of [issue #377](https://github.com/vitalyiegorov/tart-runner-fleet/issues/377)
and makes it visible while it waits.

## Context

On 2026-10-28, from ~14:05Z to ~14:22Z, a budgie burst (27 → 48 queued jobs)
turned `github-rest-budgie-org` stale on all four reachable nodes at once while
every other observation stayed fresh. Every node read NOT READY. The Mac mini
drained to zero instances for v0.1.796 and then refused it: `prepare update`
exited 5 on every updater run, because a stale critical observation made the
node unhealthy. It installed the release only after the burst drained.

It was neither budget deferral nor GitHub. The REST inventory is one listing per
run status plus one jobs listing per active run, issued in sequence, and the
15 s GitHub timeout bounded **the whole refresh**. With ~15–20 active runs at
~0.75 s an answer, the refresh outlived its own deadline. Every node failed
exactly 15.00 s after its 30 s-paced start, and the successful refreshes landed
at ~14 s. No `rate_limited` reason appeared anywhere, and the pacing never
deferred.

Two things were wrong, and either one alone blocks a release:

1. **A burst turned a slow answer into a failure.** The deadline meant to bound
   one call bounded a pass whose length grows with the queue.
2. **One scope's inventory decided whether the whole node was working.** The
   same block was waiting behind #377's pacing. A successful refresh that found
   the budget scarce deferred the next refresh up to an hour and wrote nothing
   in the meantime. Two minutes later the record read *expired*, the mark of a
   stopped loop, so a node that had decided to wait refused every release until
   the window reset.

## Decision

**The GitHub timeout bounds each request, never the refresh.** A request that is
not answered still ends at the timeout, so a hung connection cannot hold the
inventory loop. A burst can make a refresh longer; it can no longer make it
fail. The request count is unchanged, and so is its cost against the budget.

**`healthy` excuses a stale REST inventory that is still being written.** The
excuse covers a `github-rest-<scope>` observation that is `stale`, which means
it answered at least once in this process and keeps the last snapshot, and
whose record has not expired. `healthy` asks whether this process works. A
running loop that GitHub answers slowly, refuses, or that is waiting for a
budget reset is a working process reporting an external answer. The same
scope's scale-set stream is its own critical observation, and nothing excuses
it.

**A budget deferral keeps writing its observation.** While it waits for the
window reset, the ingester wakes at its ordinary pace, spends nothing, and
records the inventory as `stale` (or `unavailable`, if it never took one) with
the detail `rate_limited`. A wait therefore reads as a decision and never ages
into `critical_observation_expired`.

**`ready` is unchanged.** A stale inventory still makes the node NOT READY,
names `critical_observation_stale` on `scheduler ready`, and fails
`--require-ready`. That is the truthful answer to "is this node admitting on a
current view of the queue?". Nothing in the scheduler reads `ready`, and since
ADR 0052 the release transaction does not either.

### What still blocks a release

Everything ADR 0052 names still blocks a release, because the excuse is narrower
than a stale observation:

- `unavailable` is never excused. A candidate generation whose REST inventory
  has never answered is not proved healthy, and the transaction rolls it back.
  This is how a release that breaks REST authentication is caught.
- `expired` is never excused. An inventory loop that stopped writing is the
  fault health exists to catch.
- Only `github-rest-*` is excused. Scale-set streams, host, Tart, the scheduler,
  and the store's own `operations` observation keep their meaning.

## Consequences

During a burst the inventory of a busy repository refreshes more slowly; it does
not go stale. A refresh can now outlast the 30 s poll interval. The next refresh
then starts immediately, and the observation age stays a single refresh long,
well inside the 2-minute TTL.

A node whose REST inventory is degraded now installs releases. The worst case is
a candidate whose first refresh lands in an exhausted window: it reads
`unavailable` and is rolled back. That costs one drain per window. Before this
ADR the same node refused every release for the whole window.

`fleet doctor` names a budget deferral as `rate_limited` on the observation
instead of an unexplained expiry.

## What this does not address

It does not reduce the number of requests a refresh issues; the ETag
revalidation of #377 already makes unchanged pages free. It does not
parallelise the listings or share one inventory across the nodes of a site. If
a repository grows enough active runs that one sequential refresh outlasts the
TTL, those are the next steps, and that is a separate decision.

## Alternatives considered

**Raise `githubTimeoutSeconds`.** This only moves the threshold to a bigger
burst, and it lengthens the wait on every hung request.

**Excuse every stale critical observation from health.** A stale scale-set
stream means this node cannot hear GitHub's job assignments for that scope. That
is a fault of this process's connection, not a slow listing. ADR 0052's excuse
stays narrow.

**Make a stale REST inventory not-ready only for its scope.** `ready` is one
node-level predicate, and a per-scope readiness would be a new axis that nothing
consumes. The scope is already degraded where it matters: its demand reconciles
against the last snapshot, never an empty one.
