# ADR 0087: An aborted drain ends its operation in the rollback

## Status

Accepted. Issue [#447](https://github.com/vitalyiegorov/tart-runner-fleet/issues/447).
Amends [ADR 0074](0074-drain-guards-re-verify-the-runner-not-the-spawning-demand.md)
decision 3: an abort still returns the instance to Running, and now the drain
operation ends in that same transaction. Extends
[ADR 0028](0028-a-repeated-decision-is-a-new-attempt.md)'s generation rule
(`DrainGeneration`) from recovery drains to the event drain.

## Context

ubuntu-vitalii, v0.1.793, 2026-10-28. `trf-linux-4x8-3cdd90db7494aebf` was
cloned for a hotel-provence job that GitHub ran somewhere else. Its demand
completed while the clone was still in flight, so the projection enqueued
`event-drain-trf-linux-4x8-3cdd90db7494aebf`. When the drain ran, GitHub had
brokered the runner another job:

```text
11:11:11.074 instance drain aborted by fresh evidence ... phase=event evidence=runner_busy
11:11:11.075 component loop failure component=operations
11:12:07.958 binding ingest failure ... reason=demand_commit_conflict phase=commit
```

The abort was correct (ADR 0074). Two defects came after it.

1. **The abort was two writes.** `DrainExecutor.abort` committed the rollback to
   Running in one transaction and returned nil. The worker then completed the
   operation in a second transaction. The second write was lost (the
   `component loop failure` one millisecond later), so the operation went back
   to pending. Every retry found a running instance, and the executor's only
   answer for that is `state conflict`. That is retryable, so it retried every
   30 seconds: 21 attempts by 11:19, and it would never succeed.
2. **The next event drain reused the identity.** The runner's own job completed
   at 11:12:06. Its drain is the same operation ID and idempotency key,
   `event-drain-<id>`, so `Transition` found the old row and refused it with
   `ErrConflict` (the instance was running, not draining). This happens even
   when the first completion lands. Any instance whose event drain aborts can
   never get a second event drain. #443's re-derivation sweep reaches the same
   `enqueueDemandDrain`, and it would skip that instance every tick.

Does a later recovery drain collide too? Not on identity. A lingering or stalled
drain's ID is the scheduler's content address, salted by `DrainGeneration`, and
`deregister:<id>` is not unique on `operations`. `operation_effects` is written
with `INSERT OR IGNORE` at completion, so the effect key cannot refuse an
enqueue. Before this fix the backstop was therefore admitted *beside* the
stranded event drain. That made two live drains of one runner, and the stranded
one, now seeing `draining`, would run the backstop's teardown a second time.

## Decision

1. **An abort settles its drain in the rollback.** `lifecycle.StateChange`
   gains `Settles` (the claimed operation the change ends) and `Outcome` (its
   closed-vocabulary result). `DrainExecutor.abort` passes its own operation
   and `aborted:<evidence>`. In the same transaction as the instance update,
   `sqlite.Store.Advance` sets that operation to `completed` with
   `last_error=aborted:<evidence>`. It records **no** `operation_effects` row,
   because nothing was deregistered. The settle requires the operation to be
   `claimed` by the caller's lease owner. If it is not (the lease expired and
   the row was recovered), the whole change is refused with `ErrLeaseLost`, and
   the instance stays draining for the next attempt to re-verify. The worker's
   `Complete` afterwards finds a completed row and is a no-op. `Settles` and
   `Outcome` must be set together, and the outcome is at most 64 bytes.
2. **A drain of an instance that is not draining resolves.** The only edge out
   of `draining` to a live pre-drain state is the abort back to `running`
   (`domain.InstanceState.CanTransitionTo`). A drain operation that finds its
   instance in any such state was therefore ended by an abort. It returns nil,
   with no state change and no call to GitHub or the backend, instead of
   `ErrConflict`. This is what drains the rows an upgraded node already holds
   (the live `event-drain-trf-linux-4x8-3cdd90db7494aebf` among them). The
   worker completes it as before, which records its effect key exactly as every
   pre-fix abort that did complete already did. Nothing reads effect rows by
   key to gate a drain.
3. **The event drain's identity is scoped to terminal prior drains.**
   `enqueueDemandDrain` asks `DrainGeneration`, which counts the instance's
   completed, dead and discharged drain operations. Generation 0 keeps
   `event-drain-<id>` byte-for-byte. Generation *n* > 0 is
   `event-drain-<id>-g<n>`. A replay of one attempt sees the same generation and
   stays idempotent. A redelivered JobCompleted for an instance that is already
   draining returns before the enqueue, as it always did. An aborted drain is
   terminal, so it raises the generation and never blocks the next one. The
   effect key stays `deregister:<id>`, because it names the one physical
   deregistration. A stranded pre-fix row is still pending, so it holds
   generation 0. The completion drain is refused until decision 2 resolves that
   row (at its next attempt, within 30 seconds), and then it is enqueued at
   generation 1.

Invariant: **when a drain is aborted back to running, its drain operation is
terminal in the same transaction. A later drain of that instance (event,
re-derived, lingering or stalled) can be enqueued. No drain operation retries a
state conflict.**

## Alternatives rejected

- **Make the executor's abort return a sentinel and let the worker settle.**
  That is still two transactions. A lost second write leaves the same stranded
  row, and only decision 2 would rescue it. The settle belongs where the
  rollback is.
- **Reopen the completed row for the next drain.** That rewrites an audit
  record and its attempt history. `operations.id` is a primary key anyway, so
  the next attempt needs its own identity, as spawns and recovery drains
  already have (ADR 0028).
- **A new `cancelled` operation status.** It would add an enum value to
  `fleet.v1` and to every status filter. `completed` with a closed
  `aborted:<evidence>` outcome and no effect row says the same thing.
  `OperationFailures` already ignores completed rows, so the outcome never
  shows up as a failure.

## Consequences

- An event drain that aborts no longer strands its instance. The completion
  drain for the job the runner was actually given is enqueued when it
  completes, and exactly one drain is live.
- A lingering or stalled backstop after an abort is the only live drain.
- A plain, non-aborted drain is unchanged: same identity, same operation, same
  teardown.
- The simulator's executor mirror settles its abort through the same
  `Settles` field (ADR 0043).
- No schema migration. `fleet.v1`, configuration and authority limits are
  unchanged. An aborted drain's row now reads `completed` with
  `last_error=aborted:<evidence>` instead of an empty `last_error`.

## Evidence

`internal/adapters/sqlite/aborted_drain_test.go` replays the incident through
the real store, the real `DrainExecutor` and the real `operations.Worker`, with
the post-abort completion dropped. Before the fix it fails four ways: the
aborted drain stays live, a stranded drain retries `state conflict`, the
completion and #443 sweep paths are refused even after a clean abort, and a
lingering backstop is admitted as a second live drain. A plain event drain
passes before and after.

## Rollback

Revert the change. Rows written while it ran stay valid for the old code. A
settled abort is a `completed` drain, which the old code already treats as
done. A `-g<n>` event drain is an ordinary pending or terminal drain operation.
Reverting brings back both defects.
