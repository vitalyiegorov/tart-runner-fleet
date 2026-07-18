# ADR 0015: Authoritative runner assignment correlation

## Status

Accepted.

## Context

A scale-set request acquired for JIT provisioning is a reservation. GitHub may
later schedule two acquired requests on the opposite registered runners. The
original lifecycle projection matched events only by reservation request ID.
In a cross-assignment this drained the runner provisioned for the completed
request instead of the runner that actually executed it. The fresh-job guard
correctly refused to deregister an active runner, but the completed runner and
its Tart VM remained allocated and blocked queued successors.

## Decision

Treat `RunnerName` on the durable official scale-set record as the authoritative
execution identity. Before projecting an assigned, started, or completed event:

1. resolve the live fleet-owned instance by exact runner name;
2. resolve the unique reservation instance by scope repository and request ID;
3. require compatible platform, profile, route, and resource vectors; and
4. atomically swap both instances' repository and demand scheduling metadata
   with state, version, and prior-metadata compare-and-swap guards.

The swap preserves a one-to-one request-to-runner mapping even when the sibling
event is delayed. Instance IDs, Tart ownership metadata, and operation ownership
remain immutable. A missing active runner, missing or duplicate reservation,
incompatible runner, storage conflict, or partial-write attempt fails closed.
A completion for a runner already deleted remains idempotent.

Events without a runner name retain request-based projection because no more
authoritative execution identity exists yet.

## Consequences

Lifecycle state, deregistration safety, deletion confirmation, fleet inventory,
and successor admission now follow the runner that actually executed the job.
The database transaction may change scheduling identity without changing the
ownership signature used to authorize Tart mutation. Redelivery is monotonic:
after a committed swap, the named runner already owns the request and the same
event becomes an idempotent lifecycle projection.
