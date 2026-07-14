# ADR 0007: Keep owned runner cleanup durable

## Status

Accepted

## Context

GitHub can cancel the request used to create a JIT runner and then assign that
registered runner to another queued job in the same scale set. Deregistration
correctly refuses while the replacement job is running. A fixed total retry
budget can expire before a long job completes, leaving an owned VM and a dead
cleanup operation that permanently consumes fleet capacity.

## Decision

Runner cleanup retries indefinitely with exponential delay capped at thirty
seconds. Provisioning retains its bounded retry budget. A one-shot database
migration revives only effect-free deregistration dead letters whose owned
instance is still in the first draining phase.

## Consequences

Owned runner cleanup cannot be silently abandoned. Persistent GitHub or Tart
failures remain visible as retrying and degraded without releasing dependent
cross-platform work or bypassing ownership checks.
