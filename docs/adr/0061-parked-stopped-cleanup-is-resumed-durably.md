# ADR 0061: Parked stopped cleanup resumes through the durable worker

## Status

Accepted for implementation, issue #371. Amends ADR0039's stop counter and
ADR0021's operator-only recovery for the narrow safely inactive cleanup case.

## Evidence and cause

On2026-10-16 MacBook held an already stopped VM in draining, with a stop dead
letter after200 lifetime attempts. Guarded operator recovery restored capacity
and a GitHub-confirmed replacement job. The original adapter error remains
unknown; persisted stage codes intentionally withhold raw child-process text.

A deterministic replay proves a separate controller defect: StopAttempts took
Operation.Attempts whenever the last failure was stop. Thus198 guard failures,
one failed stop, and a retry falsely exhausted all nine stop attempts. The
exhausted branch also failed to recognize a stop that had since completed.
Finally no ordinary controller path resumed safely idle parked cleanup.

## Decision

Persist a bounded1–9 stop-local failure count in the existing LastError text.
FailureCode accepts only exact authored variants and still publishes stop.
Legacy rows prove at most one stop failure, never hundreds. There is no schema
migration, new configuration knob or public fleet.v1 field/enum change.

Before parking an exhausted stop, reobserve power. Stopped or absent means the
stop effect has landed; the remaining ordinary deletion guards still apply.
Running or unavailable power authorizes no additional destructive action.

On the existing authority tick, after coherent operation observations, consider
only parked deregister operations whose closed failure code is stop or delete,
with a matching cleanup-state instance and proven idle backend. Re-read backend
power and fresh runner/job deletion confirmation before requesting recovery.
Unknown, busy, active operations, running guests, and non-authority modes do
nothing. A VM that comes back during stopped recovery aborts to Running.

One SQLite transaction validates dead-operation identity, instance ownership,
cleanup state and no pending/claimed work. It atomically preserves the prior
attempt as discharged and enqueues a deterministic replacement with its own
bounded retry budget, same effect key and ownership payload. Pending dependency edges follow the replacement and remain blocked until its
actual completion; already failed successors are never resurrected. The old
attempt's timestamps/error/count remain evidence. A repeat is idempotent. No live instance
is retired by this observer: only the existing lifecycle worker deregisters,
confirms inactivity, stops and deletes it. A failed delete stays durably owned
and retryable instead of leaving an untracked VM after an observer-side reap.

This reuses existing lifecycle states, operation generation, worker, retry and
confirmation gates. No new timer, service, direct database operation or generic
mutation API is introduced. Observe/shadow/canary authority limits remain intact.

## Evidence required

Red lifecycle replays for mixed failure history and a completed late stop;
red durable-store replay for absent automatic recovery; store transaction fault
rollback, concurrency and idempotency tests; busy/unavailable/future/stale evidence
refusals; and deterministic simulation calling the real recovery service. The
previous simulation mirror omitted parked cleanup becoming externally safe: a
world-model blind spot. Required CI and normal updater gates precede promotion.
