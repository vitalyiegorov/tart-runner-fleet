# ADR 0009: Recover GitHub message sessions in place

## Status

Accepted. The delivery-only queue-lookahead clauses are superseded by ADR 0013.

## Context

Each configured scope/profile owns one official GitHub Actions Scale Set message
session. A session can become permanently unusable after startup even while the
other sessions and the daemon remain healthy. Repeatedly polling that same
client keeps one critical observation unavailable. The scheduler correctly
fails closed, but all unrelated capacity then waits for a manual daemon restart.

A message source is also the lifecycle-control client used to acquire work,
generate JIT registration, inspect runners, and deregister them. Replacing only
the ingestion pointer would split those operations across two generations.

## Decision

Wrap every official scale-set source in one atomically replaceable source:

- any non-cancellation ingestion failure closes the failed official session and
  creates a replacement before the service loop backs off;
- recreation rereads the committed durable cursor instead of reusing startup
  memory;
- ingestion and lifecycle control always resolve through the same current
  source generation;
- a read/write lock lets healthy lifecycle calls proceed concurrently with the
  long poll and makes replacement exclusive;
- the existing bounded cleanup width also bounds concurrent recoveries;
- concrete close/open errors remain internal and health exposes only bounded
  component state;
- cancellation never starts recovery, so normal shutdown remains deterministic.

Each scale set also reserves one delivery-only queue lookahead above the
maximum runner concurrency for its profile. `maxCapacity` must therefore be
strictly greater than the resource-bounded runtime capacity. This lets GitHub
deliver and persist a queued successor while all executable slots are occupied;
it does not increase scheduler `maxActive`, host CPU, memory, or VM slots.

The scheduler stays fail closed until the replacement completes a successful
poll. No unavailable observation is treated as an empty queue.

## Consequences

A dead broker session repairs itself without restarting `fleetd` or disturbing
healthy Tart runners. During a broad network outage, recovery attempts are
bounded and retain the service loop's retry backoff. If broker-side cleanup
cannot complete, the source remains unavailable and retries safely rather than
creating overlapping consumers.

One successor per profile remains visible to queue-SLO monitoring instead of
being hidden behind GitHub's scale-set capacity head of line. Existing
configurations whose `maxCapacity` equals executable capacity must be migrated
before authority validates under this decision.
