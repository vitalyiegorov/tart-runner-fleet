# ADR 0081: A dead stop with a running guest is resumed once the host heals

## Status

Accepted. Issue [#424](https://github.com/vitalyiegorov/tart-runner-fleet/issues/424).
Extends ADR 0061's automatic cleanup recovery, and bounds it.

## Context

On 2026-10-25 ubuntu-vitalii's root filesystem ran out of inodes for about 45
minutes (#423). Three event drains already in `deregistering` failed every
`stop` the ladder offered (ADR 0039) in about two and a half minutes and died
with the plain `stop` code. Their containers kept running. When the host
recovered, nothing retried them: ADR 0061 resumes only a *parked* cleanup, one
whose VM is proven stopped, and the operator discharge (ADR 0021) refuses a
running VM and never stops one. Three of the node's four slots stayed held
until a human ran `podman stop`.

The stop budget counts attempts, not time. A host-wide transient fault is
exactly the failure that exhausts it in minutes and then goes away.

## Decision

**The authority tick also resumes a dead `deregister` that failed at `stop`
while its VM is still running, once the host is healthy again.** It reuses ADR
0061's machinery unchanged: the same tick, the same fresh runner/job deletion
confirmation, the same transaction that preserves the dead attempt as
discharged and enqueues one deterministic successor, and the same worker. All
of these must hold, or the dead letter stays:

1. the instance row is in a cleanup state (draining, deregistering, stopping);
2. the tick's host observation is fresh and its guardrails pass. An update
   drain's admission refusal is not host pressure; a stale, unavailable or
   pressured host is never healthy;
3. GitHub freshly confirms the runner inactive and its jobs inactive (the
   confirmation ADR 0061 already requires);
4. the tick's inventory reads the VM running, and a fresh backend read agrees.
   Unknown power, or a disagreement between the two reads, proves nothing.

The tick never stops the VM. The successor starts its own stop ladder from the
graceful rung, and the worker climbs it. A running VM in `draining` keeps its
drain phase, whose guard re-verifies its own premise; only a stopped VM restarts
through the stopped-recovery guard as before.

**Automatic recovery is bounded, for both the stopped and the running case.**
`operations.CleanupResumptionDue` is the one rule: an effect may be resumed at
most `MaxCleanupResumptions` (3) times; the first resumption is immediate, and
each later one waits `CleanupResumptionBackoff` (10 minutes), doubling, after
its predecessor died. The count is the number of discharged attempts sharing
the dead attempt's effect key on the same resource, so it needs no new column.
The tick checks the rule before spending a probe, and the transaction rechecks
it, so racing ticks cannot exceed it. After the bound the dead letter stays for
an operator, exactly as before ADR 0061.

No schema migration, configuration knob, lifecycle state or `fleet.v1` field
changes. `TickResult` gains an internal `HostHealthy` verdict, and the internal
dead-letter projection carries the resumption count and death time.

## Consequences

- A host-wide transient fault no longer turns into slots held until an
  operator intervenes.
- ADR 0061's stopped-VM recovery, previously unbounded, now also hands an effect
  to the operator after three resumptions. A cleanup that has died four times is
  not transient.
- A busy runner, an unhealthy or unobserved host, and unreadable power all keep
  the dead letter exactly as before.

## Evidence

`tests/integration/dead_stop_drain_recovery_test.go` replays the incident with
the real store, recovery service, worker and drain executor. Red on main (the
dead letter was never superseded); after the fix exactly one successor runs,
the worker stops the VM once, and the instance reaches `deleted`. It also
covers stale host, busy runner, unknown power, repeated ticks and a successor
that keeps dying. `tests/simulation/parked_cleanup_test.go` walks the host
fault and its recovery through the real service.
