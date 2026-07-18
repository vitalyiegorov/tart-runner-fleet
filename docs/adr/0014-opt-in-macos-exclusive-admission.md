# ADR 0014: Opt-in macOS-exclusive admission

## Status

Accepted. This amends ADR 0012 without changing its default.

## Context

ADR 0012 made work-conserving Linux/macOS coexistence the fleet default. That
is still the best general-purpose utilization policy. High-parallelism iOS UI
experiments have a different objective: establish a complete, same-profile
macOS simulator cohort quickly and keep Linux disk, memory, and CPU activity
from entering the measurement window.

Resource envelopes alone cannot express that isolation requirement. Operators
need an explicit policy that is safe to test on selected configurations without
silently changing existing installations.

## Decision

`macosBurst.admissionPolicy` accepts exactly `"shared"` or
`"macos-exclusive"`. Omission normalizes to `"shared"`, preserving ADR 0012
and all existing configurations.

Under `"macos-exclusive"`:

1. Any eligible macOS demand or consuming macOS instance prevents new Linux
   spawns.
2. A consuming single macOS profile with queued demand for that profile remains
   the target cohort. A different queued macOS profile cannot strand a partial
   cohort before the active profile reaches its configured `maxActive` and
   resource envelope.
3. Linux instances and non-target macOS instances are foreign consumers. Busy
   foreign instances are never interrupted. Idle foreign instances are drained,
   and replacement macOS spawns depend on successful drain operations.
4. A blocked macOS transition persists `MacHandoff` state across ticks and
   restarts. It admits no bounded Linux drain backfill.
5. With an active macOS cohort but no macOS demand, queued Linux work waits in a
   durable `LinuxHandoff`. Once every macOS instance is idle, dependency-safe
   drain and Linux admission may occur in the same plan.
6. Simultaneously consuming different macOS profiles fail closed. The planner
   does not guess which live profile owns the cohort.

CPU, memory, slot, repository, host-pressure, freshness, lifecycle, and
ownership checks remain unchanged. `maxActive` is a ceiling, not a promise that
enough eligible jobs or physical resources exist to fill it.

## Consequences

The default remains work-conserving and backward compatible. Selected iOS
experiments can instead trade Linux throughput and cross-platform fairness for
a stable macOS-only execution window. Continuous macOS demand can delay Linux
indefinitely by design, so this policy must be opt-in and measured before any
rollout.

The scheduler may report a transition period with existing Linux and macOS
instances, but it creates no new Linux work during the exclusive cohort and
never drains a busy instance.
