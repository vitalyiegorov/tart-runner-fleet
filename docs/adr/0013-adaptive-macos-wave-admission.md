# ADR 0013: Adaptive macOS wave admission

## Status

Proposed.

## Context

The builder and Maestro profiles share one finite host but cannot coexist when
their declared memory vectors exceed the host envelope. Starting an entire
fresh Maestro wave can therefore make a builder job that becomes visible a few
minutes later wait for the longest Maestro shard to finish. GitHub Scale Set
`maxCapacity` cannot solve this: it is a promise of runners the listener can
produce, not a backlog-discovery knob, and overstating it creates assignments
the fleet cannot promptly fulfil.

## Decision

Configured profile limits and resource vectors remain hard safety ceilings.
Application repository caps may opt into `autoMaxActive`; the runtime resolves
that cap to the fleet's configured global slot envelope. The explicit
control-plane cap remains manual so application traffic cannot silently consume
its reserved fanout policy.

For a macOS profile whose ceiling is greater than one, the scheduler initially
admits one runner while every queued sibling is younger than the existing
global fairness window. This leaves one bounded profile-switch discovery
window. If an incompatible older profile appears, normal oldest-aged
arbitration prevents additional work from extending the active wave. If no
such demand appears, the remaining sibling ages through the fairness window
and the scheduler automatically restores the full configured profile limit.

This policy is pure and deterministic. Its feedback is current queue age,
fresh instance ownership, and the fresh host resource vector. Historical job
duration is deliberately not used as an authority input: duration estimates
can be telemetry, but they must not weaken resource ceilings or make replay
results depend on mutable learned state.

## Consequences

- Fresh multi-runner macOS waves may delay their second runner by at most the
  configured fairness window.
- Uncontended aged waves still use their full safe capacity.
- A newly visible older builder gets a drain boundary after the first Maestro
  runner instead of after the whole wave.
- No running job is preempted and no profile may exceed its configured or
  resource-derived capacity.
- GitHub Scale Set capacity remains a truthful hard bound and is not tuned
  above what the scheduler can produce.
