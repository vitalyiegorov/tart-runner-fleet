# ADR 0009: Recover GitHub message sessions in place

## Status

Accepted. The delivery-only queue-lookahead clauses are superseded by ADR 0013.
The unconditional release-before-open clause is amended below: release failure
no longer withholds recreation indefinitely.

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

## Amendment: bounded release, terminal classification, and stated reasons

Release-before-open assumed the broker would always accept the release of a
session it had just refused to serve. It does not. When GitHub invalidates a
session, the subsequent `DELETE .../sessions/{id}` fails too, so an
unconditional "release must succeed before open" rule pins that binding to a
dead handle for the daemon's whole lifetime. One scope lost all ingestion for
four hours while every other scope stayed fresh, and only a manual daemon
restart recovered it. A successful release followed by a failed open had the same
shape: the next attempt tried to release an already-released session, which can
only fail.

The decision is therefore amended:

- ingest failures are classified terminal-for-this-session or transient. A
  permanently rejected message-queue token, an unknown session identity, and a
  scale set acquired by another session are terminal and recreate immediately;
  rate limits, broker 5xx, secondary limits, and deadlines keep the session;
- failures that cannot be classified escalate on a bound: after
  `githubSessionMaxIngestFailures` consecutive failures for that binding, or once
  `githubSessionFailureWindowSeconds` has elapsed since the first failure of the
  run, the session is discarded even if the broker refuses to release it.
  Defaults are 5 failures and 5 minutes, and both bounds are per binding;
- a session whose release already succeeded is recorded as released, so a
  retried recovery only opens a replacement and never deletes it twice;
- a successful poll or a successful replacement clears the accumulator, so a
  healthy binding never recreates and escalation cannot storm;
- every ingest failure carries a closed-vocabulary reason (`session_expired`,
  `session_release_failed`, `session_create_failed`,
  `recreated_after_failures`, `message_poll_failed`,
  `queue_observation_failed`, `queue_observation_stale`,
  `queue_reconcile_failed`). It is published on the binding's observation
  `Detail` and on the rate-limited component failure log. Concrete broker
  errors, tokens, JIT configuration, and upstream bodies remain internal.

Cursor semantics are unchanged: a replacement rereads the committed durable
cursor, so an unacknowledged message is redelivered to the new session exactly
once and at-least-once delivery survives a recreate. Discarding an unreleasable
session accepts a bounded risk of one overlapping broker consumer instead of an
unbounded scope outage; the scheduler still fails closed until the replacement
completes a successful poll.

One successor per profile remains visible to queue-SLO monitoring instead of
being hidden behind GitHub's scale-set capacity head of line. Existing
configurations whose `maxCapacity` equals executable capacity must be migrated
before authority validates under this decision.
