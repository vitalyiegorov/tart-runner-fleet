# ADR 0086: A durable completion drains its runner on the tick

## Status

Accepted. Issue [#442](https://github.com/vitalyiegorov/tart-runner-fleet/issues/442).
Related to [ADR 0074](0074-drain-guards-re-verify-the-runner-not-the-spawning-demand.md):
the drain this ADR re-derives is checked by the same runner-keyed busy evidence.

## Context

Omarchy, v0.1.788, 2026-10-28. `trf-linux-4x8-0ab3245aa6dfc6d6` stayed
`running` for eight minutes after GitHub cancelled its job. The idle-runner
deadline reclaimed it at 06:17:19Z. All times are UTC:

| When | What |
| --- | --- |
| 06:09:18 | Message 100003501 is committed: one `JobCompleted result=canceled` for request 5871030784497826206, `runner_name` = the instance. The same transaction advances the durable cursor to 100003501. |
| 06:09:20 | `binding ingest failure ... reason=message_poll_failed phase=poll`. This is the projection's error, nacked as the message's commit failure. It is not `demand_commit_conflict`, so it was not a version race (`ErrConflict`). The cause text is deliberately not logged. The node had been behind a swap-pressure admission gate since 06:07, so a contended or slow SQLite write is the likeliest cause. |
| 06:09:20 | The ingest loop replaces the session. The replacement opens at `InitialCursor = DemandCursor` = 100003501. |
| 06:11 | Message 100003502 is handled. 100003501 is never delivered again. |
| 06:17:19 | `instance recovery drain planned ... cause="runner idle past its deadline"`. |

The batch was not lost and the projection was not skipped. The redelivery the
projection relied on cannot happen:

1. `ApplyDemandBatch` commits the events and advances `scale_set_cursors` in
   one transaction. That is before projection.
2. `IngestOnceResult` returns the projection error, so `ScaleSet.Handle` nacks
   the message. Its comment says "redelivery retries every failed
   projection".
3. `recoveringScaleSetSource.Handle` replaces the session after **every**
   failure. The replacement is built by `openSource`, which reads
   `DemandCursor`. The long poll then asks GitHub for messages after the failed
   one.

So a projection that fails after its batch commits is never retried. For a
JobCompleted this means the drain is never enqueued. A Tart guest usually powers
itself off and the stopped-VM recovery catches it. A podman guest's PID 1 is
`sleep infinity`, so it holds its vector until the idle-runner deadline or the
occupancy budget.

The simulator hid this. Its broker redelivered every batch the fleet failed to
ingest, including one whose batch had already committed. That is a world-model
defect: production's replacement session cannot do this.

## Decision

1. **Invariant.** A live instance in `online_idle`, `assigned` or `running`
   reaches `draining` through `event-drain-<id>` when a durable demand in a
   scale set it serves is `JobCompleted` with `runner_name` = the instance ID,
   and no durable demand shows a job started on that runner. The rule is
   re-derived from durable rows on every tick. It never depends on a message
   being delivered again.

2. **One drain, not a second path.** `Store.DrainCompletedRunners` selects
   those runners and calls the message path's own `projectDemandRank`. The
   operation, its ID, its idempotency key, its effect key and its drain phase
   are therefore identical. If the message path already enqueued the drain, the
   instance is no longer in a qualifying state and the sweep does nothing. A row
   that moved between the read and the write (`ErrConflict`) is skipped and
   read again on the next tick.

3. **Narrow evidence.**
   - Only a completion GitHub recorded against **this runner's name** counts.
     The spawning reservation completing proves nothing about a runner GitHub
     brokered a different job to (ADR 0033).
   - A runner with a `JobStarted` demand (`RunnerActiveJob`) is never drained.
     This is the evidence the event drain re-checks before deregistering
     (ADR 0074). Asking it first means the sweep never enqueues a drain that
     would only abort back to `running` on every tick.
   - Instances before registration and instances already tearing down are left
     to the message path. It orders a pre-registration drain behind
     provisioning, and a teardown in flight already is the drain.

4. **Where it runs.** `Engine.Tick` runs the sweep before it observes the
   inventory, and only in `canary` and `authority`, the modes that run
   executors. `observe` and `shadow` write nothing new. An unreadable sweep fails
   the tick as `demand_unreadable`, the same as the overdue-demand expiry.

5. **Said out loud.** Each re-derived drain logs
   `completed job drain re-derived instance=<id> reason=completion_projection_lost`,
   once per runner, never rate limited. In the normal case the message path
   drains first, so a line means the message path lost a projection.

6. **The simulator's broker matches production.** After a failed ingest it
   redelivers only messages above the durable cursor.

## Consequences

- A runner whose completion projection was lost drains on the next tick, not
  at its idle-runner deadline or occupancy budget.
- The sweep is one query per binding per tick: the scale set's completed rows, joined to `instances` on its primary key. In steady state it
  returns no rows, because completed runners are already draining or deleted.
- `IngestOnceResult`'s comment no longer promises a redelivery that cannot
  happen.
- `fleet.v1`, the store schema and the configuration are unchanged.
  Observe and shadow authority limits are unchanged.

## Rejected and deferred

- **Advance the cursor only after projection succeeds.** This would make
  redelivery real for every event kind. It also changes the at-least-once
  contract every ingest path relies on (ADR 0009, issue #165), and a projection
  that fails deterministically would pin the binding to one message forever.
  That is the three-day outage shape of #165. It is deferred to its own issue
  and ADR.
- **A separate tick-only drain operation.** Rejected. Two drain identities for
  one fact would race and would need their own idempotency rules. Reusing
  `projectDemandRank` keeps one.

## Open risk

The other projections (`JobAssigned`, `JobStarted`) have the same lost-retry
shape. A lost `JobStarted` leaves an instance `assigned` while its job runs.
The stalled-assignment drain then reads `RunnerActiveJob`, sees the job, and
aborts, so no work is lost. The lifecycle state is stale until the completion
arrives. This ADR closes only the case that holds a vector.
