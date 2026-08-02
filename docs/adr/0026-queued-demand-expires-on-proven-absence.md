# ADR 0026: Queued demand expires only on proven absence

## Status

Accepted. Extends the canonical REST job inventory
([ADR 0015](0015-canonical-inventory-and-truthful-capacity.md)) with a retraction
rule, and completes the lifecycle-recovery class that
[ADR 0016](0016-authoritative-runner-assignment.md) and the stalled-assignment
and lingering-runner deadlines opened for instances.

## Context

The scale-set protocol is monotonic by construction. `runner_demands` only ever
ratchets forward — `JobAvailable` to `JobAssigned` to `JobStarted` to
`JobCompleted` — because redelivery must never move a job backwards. GitHub is
therefore the only party that can retract queued demand, and on 2026-08-01 it
did not.

A `knee-repo`/`large` job entered the queue at 18:32:52Z. Its pull-request branch
was force-pushed, GitHub cancelled the superseded run server-side, and no
terminal message ever reached the session. The broker kept advertising one
acquirable job in `Statistics.TotalAvailableJobs`, so the fleet kept exactly one
`JobAvailable` row alive for 10h35m while the repository's REST scope contained
zero non-completed workflow runs at all.

Three consequences followed, none of which any existing recovery path could
reach:

- The fleet provisioned `large` VMs for it. Each registered online, never went
  busy (`trf-large-901d732885e670ec`, `busy=false`, no job available), and was
  eventually reclaimed by the lingering-runner deadline — which then freed
  capacity for the next VM spawned for the same ghost.
- Queue depth never reached zero, so the release gate refused every five-minute
  attempt with `apply production release: prepare update: autoupdate: fleet is
  not quiescent`, and v0.1.304 could not deploy. The stale observation blocked
  the release that improves recovery — the same starvation shape v0.1.282 fixed
  for parked dead letters ([ADR 0021](0021-dischargeable-dead-letters.md)).
- The queue SLO stayed in `queue_incident,queue_slo_breached` all night.

Restarting the daemon did not help: observations were rebuilt from the same
durable row and showed the same job. The remedy was an operator proving by hand
that no busy runner existed anywhere.

This is the third instance of one class. A stalled assignment and a lingering
runner are both "the fleet holds a resource whose justification no longer
exists"; a ghost demand is the same fact one level up, in the queue itself.

## Decision

**Absence is proven by complete REST snapshots, never inferred from age.** The
canonical job inventory already replaces a whole scale set's observations in one
transaction from one successful scope fetch, and a partial fetch is discarded
rather than committed. That makes an empty snapshot a positive statement about
the scope, not a missing one. Each such snapshot records, per demand row, what
it saw: `corroborated_at` when the row's logical job was queued in it, and
`absent_since` plus `absent_observations` when it was not.

**Only demand REST has corroborated at least once can ever be expired.** A row
whose logical key no REST snapshot has ever matched — a repository outside the
observer, a label join that never held, an inventory that is not enabled — never
accrues absence. Evidence that was never capable of seeing a job may not be used
to conclude the job is gone.

**Two independent bounds must both close.** The row must have been missing since
at least fifteen minutes before the current snapshot, and must have been missed
by at least three distinct complete snapshots. The window rules out a slow or
eventually-consistent REST view; the count rules out a single lonely snapshot
after an observation outage concluding anything by itself. A snapshot that is
not strictly newer than the last one recorded for that scale set adds no
evidence, so a replayed or clock-skewed observation cannot count twice.

**Expiry is a revocable conclusion, not a deletion.** The row is retained in
full and marked `expired_at`; `ActiveDemands` simply stops listing it. Any newer
evidence from either source restores it unchanged, with its original queue age:
a REST snapshot that sees the job queued again, or any broker event for that
request. Nothing about the demand's identity, age, or ordering is lost.

**The status predicate is the execution-time gate.** The expiring `UPDATE`
requires the row to still be exactly `JobAvailable` at commit time. A job that is
assigned, starts, completes, or is re-advertised between the snapshot and the
statement fails the `WHERE` clause instead of losing its demand — the same
shape as the `JobStarted`/`JobActive` re-checks that guard the two instance
recoveries.

An alternative was to expire on age alone, since a job queued for eleven hours
is self-evidently dead. It was rejected: age is a property of the fleet's own
capacity, not of GitHub's state, and a genuinely queued job behind a saturated
host would have been dropped by it.

## Consequences

Ghost demand no longer holds capacity, no longer breaches the queue SLO, and no
longer defers a release. Recovery needs no operator, no restart, and no
destructive action: the only mutation is one column on one row, and it is
reversed automatically the moment either source contradicts it.

The blast radius of a wrong expiry is bounded to scheduling. Expiry never
touches an instance, never deregisters a runner, and never marks a job complete;
`RunnerActiveJob` still keys on `JobStarted` alone, so no drain-safety or
ownership guard changes meaning. The worst case is that one job waits for the
next broker message or REST snapshot — and the next REST snapshot that sees it
restores it.

Fleets without the canonical job inventory are unaffected: with no complete
scope observation there is no corroboration, no absence, and no expiry.

## Not addressed here

The release gate is unchanged. A queued job still defers an update, always;
what changed is that the queue now tells the truth. Narrowing quiescence itself
would weaken the invariant on the same evidence that this decision already
corrects at its source.
