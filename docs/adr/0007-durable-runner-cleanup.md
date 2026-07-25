# ADR 0007: Keep owned runner cleanup durable

## Status

Accepted. Bounded by [ADR 0020](0020-diagnosable-drain-failures.md), which
escalates past 720 attempts into a published dead letter, and completed by
[ADR 0021](0021-dischargeable-dead-letters.md), which makes such a dead letter
parked rather than busy and gives an operator a guarded way to discharge it.
Neither shortens a retry: a refusal that a running job can still explain is still
retried indefinitely.

## Context

GitHub can cancel the request used to create a JIT runner and then assign that
registered runner to another queued job in the same scale set. Deregistration
correctly refuses while the replacement job is running. A fixed total retry
budget can expire before a long job completes, leaving an owned VM and a dead
cleanup operation that permanently consumes fleet capacity.

The same replacement assignment can also finish without a completion event
that matches the request ID stored on the original instance. The ephemeral
guest then powers off while durable state remains assigned, so no drain
operation exists at all.

## Decision

Runner cleanup retries indefinitely with exponential delay capped at thirty
seconds. Provisioning retains its bounded retry budget. A one-shot database
migration revives only effect-free deregistration dead letters whose owned
instance is still in the first draining phase.

Fresh Tart inventory also records whether each owned VM is running or stopped.
Before admitting new work, the deterministic scheduler emits a recovery drain
for an assigned or running instance whose VM is stopped. That drain carries a
durable recovery phase and may proceed only after the exact scale-set source
also confirms the runner name is absent. Unknown power state, running VMs, and
ordinary assigned runners remain fail-closed.

## Consequences

Owned runner cleanup cannot be silently abandoned. Persistent GitHub or Tart
failures remain visible as retrying and degraded without releasing dependent
cross-platform work or bypassing ownership checks.

A stopped replacement assignment cannot reserve fleet capacity forever, while
one stale or unavailable observation still prevents destructive recovery.
