# ADR 0001: Shadow-first replacement

## Status

Accepted.

## Decision

The Go daemon starts in observe-only mode and records the plan it would have
executed. It cannot mutate runners or VMs until an explicit deployment phase
enables those capabilities. Promotion proceeds through observe, shadow, canary,
and authority, with atomic rollback to the incumbent shell manager.

The scheduler is pure: an immutable observation and durable prior state produce
one deterministic plan. Side effects are durable idempotent operations executed
after the plan is committed.

## Consequences

This avoids a flag-day migration and makes every production incident replayable
as a test fixture. It also makes stale or unavailable observations first-class
values instead of silently treating them as empty collections.

