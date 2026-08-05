# ADR 0035: A broker message id is unique only within its sequence

## Status

Accepted. Amends the durable idempotency ledger that
[ADR 0009](0009-recover-github-message-sessions.md) relies on for at-least-once
delivery, and adds the ingest-failure reason the operability gap of
[ADR 0031](0031-deterministic-simulation-testing.md)'s property set could not
name.

## Context

`ApplyDemandBatch` is the seam every scale-set message passes through. It writes
the message's events and records the message itself in `scale_set_inbox`, so a
redelivery — the normal outcome of at-least-once delivery, and the reason
`ScaleSet.Handle` commits before it acknowledges — is recognized and applied
once. Its key was `(scale_set_id, message_id)`, and a row whose stored digest
disagreed with the incoming message returned `ErrConflict`.

That key assumes a broker message id is unique for the life of the database. It
is not. It is unique only within one broker sequence, and GitHub restarts the
sequence.

On 2026-08-01T18:32Z GitHub restarted the sequence for scale set
`8077185082566234948` (`knee-repo`/`large`, `vitalyiegorov/knee-doctor`) at
`100000001`. The inbox still held `100000004..100000086`, written between 13 and
20 July under the previous sequence. The restarted sequence's first three
messages had ids the ledger had never seen and applied normally; the moment it
reached `100000004` — an id the July sequence had already used for a different
job — the digests disagreed:

```
message_id  created                 sequence
100000001   2026-08-01 18:32:52     restarted
100000002   2026-08-01 18:37:52     restarted
100000003   2026-08-01 18:37:53     restarted   <- last message ever ingested
100000004   2026-07-13 21:46:20     retired     <- every later delivery collided here
...
100000086   2026-07-20 13:03:42     retired
```

From that point the loop was closed and self-sustaining: `ErrConflict` →
`ScaleSet.Handle` nacks → the session is recreated (ADR 0009) → GitHub
redelivers on its five-minute cadence → the same collision. Daemon restarts,
workflow cancellation, and re-runs all fed the same loop. Every
`linux-large` job in the repository was undispatchable for three days, and the
durable cursor stayed frozen at the retired sequence's high-water mark
(`100000086`), which is the `lastMessageId` every new session opened with.

Nothing named it. `IngestFailureDetail` degrades an unclassified failure to
`message_poll_failed`, and `SessionRecoveryPolicy.OnFailure` degrades a
non-terminal one to `recreated_after_failures`; a durable write conflict was
therefore reported for three days as a network condition and a session
condition, which are the two diagnoses that lead an operator away from the
cause. No log line, health reason, or metric carried the affected scope,
profile, or scale set. `fleet queues` read 0, `fleet doctor` read PASS, and
`fleet_queue_oldest_age_seconds` read 0, because a binding that cannot hear
GitHub has an empty queue and an empty queue is indistinguishable from an idle
fleet.

The simulator was blind the same way, and for the same reason: every property
oracle in ADR 0031 reads the fleet's own view of demand.

## Decision

**The durable idempotency key is `(scale_set_id, generation, message_id)`.**
Every inbox row and the cursor carry the generation they were written under.
Within a generation nothing changes: a redelivered message is byte-identical,
its digest matches, and it is recognized and not applied twice. That is the
property at-least-once delivery rests on and it is not weakened anywhere here.

**A generation is adopted on one piece of evidence: divergence.** The ledger
already holds this id, in this generation, under different content. A redelivery
of the same message cannot produce it, because a redelivery is the same bytes;
so the id has been reissued, and the ledger that claims it is describing a
sequence that no longer exists. The incoming message is applied under the next
generation.

**The fresh message always wins.** It was just delivered by the broker and has
not been acknowledged. Applying it is at worst a re-application of demand the
fleet already holds, which the projection is built to absorb; refusing it is a
job that never runs. Failing closed here means never dropping a delivered job,
which is the direction this contract exists to enforce.

**The cursor follows the live sequence.** Inside a generation it is still the
maximum committed id, so a delayed message cannot rewind it. When a generation
is adopted the cursor is set to the adopted message's id, because the retired
high-water mark asks the broker to continue from an id the live sequence will
not reach for thousands of jobs.

**Retirement is bounded, not destructive.** Adopting a generation deletes rows
older than the one being retired, so the ledger stays finite across repeated
restarts, and keeps the retired generation itself, because those rows are the
evidence that diagnosed this incident on 2026-08-04.

**A durable commit conflict is its own reason.** `demand_commit_conflict` joins
the closed ingest vocabulary and is classified before both degradations, so a
write conflict can never again be reported as broker flapping or as session
churn.

**A failing binding names itself.** Every ingest failure now logs the scope,
profile, and scale set beside the closed-vocabulary reason, and an adopted
generation is logged with the retired high-water mark and the id that proved the
restart. The store heals itself, but a durable event that used to require three
days and hand-written SQL is never silent.

**Property (j): every advertised job is heard.** The simulator gains an oracle
that reads GitHub's truth rather than the fleet's — a job the broker has
actually delivered a `JobAvailable` for must be in the durable ledger within a
bounded number of ticks — and the harness now models at-least-once redelivery,
so a refused message comes back exactly as it does in production.

## Alternatives considered

**Qualify the key by broker session identity.** The session id changes on every
recreation, which happens routinely — token refresh, restart, any failure. Keyed
on it, the redelivery that follows a session recreation would no longer be
recognized as a duplicate, which is precisely the case at-least-once delivery
needs deduped. Rejected: it trades a rare failure for a constant one.

**Treat a message id below the cursor as a restart.** It is tempting because it
would have fired on the restarted sequence's first message, before any collision
— at 18:32:52Z rather than three days of nothing. But inside one sequence a
delayed message legitimately arrives below the high-water mark, and by id alone
the two are indistinguishable. Acting on it would rewind the cursor and retire a
ledger whose messages are still live, weakening dedupe for real deliveries in
order to detect earlier a restart that divergence catches anyway — on the first
message that could have done any harm. Rejected as a detector; the harm it
prevents is exactly the harm divergence prevents.

**Bound the inbox by age or by distance below the cursor, so ancient ids cannot
collide.** Retention is a probability, not a contract: a restart one day after
the rows were written still collides, and a restart the day after a prune does
not. It cannot be the primary rule. It survives here only in its deterministic
form — retirement is bounded to two generations, which is a statement about
generations rather than about time.

**A guarded `fleet inbox reset --scale-set ... --confirm ...` operator command.**
Not added, deliberately. The remedy this incident needed by hand is now taken by
the store itself, on the first delivery that would have collided, including on a
database already poisoned before this change: the colliding message proves the
restart and heals the binding with no operator action. Adding a second mutating
command — `internal/discharge` is the one guarded operator mutation, and its
whole design is about ordering guarantees a human cannot be asked to reproduce —
would create a hand-operated path to retiring durable delivery evidence whose
only remaining use is a case the fleet now handles for itself. The evidence an
operator needs instead is the log line naming the binding and the adopted
generation, which is what this ADR adds.

**Leave session recreation unchanged on a durable conflict.** Kept. Recreating a
session cannot resolve a conflict the store raised, and the churn was part of
the incident's noise, but after this change a conflict means a genuine durable
contradiction rather than a sequence restart, and narrowing the recovery path is
a separate decision with its own evidence.

## Consequences

- Migration 13 rebuilds `scale_set_inbox` with `generation` in its primary key
  and adds `generation` to `scale_set_cursors`. Existing rows are generation
  zero: they were all ingested under one sequence.
- `ApplyDemandBatch` returns `operations.DemandBatchResult` rather than a bare
  applied flag, so the adopted generation reaches the daemon through
  `DemandCoordinator.OnSequenceReset` instead of dying inside the adapter.
- Divergent content under one message id is no longer refused. The behaviour
  change is deliberate and is pinned by
  `TestDemandInboxAtomicDuplicateConflictRestartAndSanitization`.
- `ErrConflict` remains, and still means what it always meant elsewhere: a
  durable contradiction, such as the synthetic-request-id collision guard that
  refuses to merge two jobs. It now reaches operators as
  `demand_commit_conflict`.
- The four pre-existing simulation arms produce byte-identical corpus digests,
  so nothing about the fleet's scheduling behaviour moved; the new
  `sequence-reset-linux-large` arm is red before this change and green after.
