# ADR 0009: Recover GitHub message sessions in place

## Status

Accepted.

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

The scheduler stays fail closed until the replacement completes a successful
poll. No unavailable observation is treated as an empty queue.

## Consequences

A dead broker session repairs itself without restarting `fleetd` or disturbing
healthy Tart runners. During a broad network outage, recovery attempts are
bounded and retain the service loop's retry backoff. If broker-side cleanup
cannot complete, the source remains unavailable and retries safely rather than
creating overlapping consumers.
