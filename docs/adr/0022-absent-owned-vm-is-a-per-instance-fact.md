# ADR 0022: An absent owned VM during cleanup is a per-instance fact

## Status

Accepted. Amends [ADR 0007](0007-durable-runner-cleanup.md), which introduced the
running/stopped power observation, and refines the ordering rationale recorded in
[ADR 0021](0021-dischargeable-dead-letters.md) §5. It does not change
[ADR 0015](0015-canonical-inventory-and-truthful-capacity.md)'s rule that an
unavailable observation is never published as an empty collection, and it takes no
new destructive authority.

## Context

`ProductionInventory.Observe` turned the entire instance observation
`Unavailable("owned VM <id> missing from Tart")` whenever any single live,
non-`Planned` durable row had no matching VM in `tart list`. `PlanTick` blocks on
an unusable instance observation, so one such row stopped planning for **every
profile on the host** until an operator intervened.

Two things make that blast radius wrong rather than merely conservative.

**The state is an expected intermediate during cleanup, not an anomaly.**
`DrainExecutor` deletes the VM and only *then* advances the row to `deleted`
(`internal/lifecycle/executor.go`, `StateStopping` branch). A tick also reads the
durable rows and Tart at two different instants
(`internal/app/inventory.go`: `LiveInstances` then `Tart.List`). Any interleaving
of the two therefore legitimately observes a live `stopping` row with no VM. The
condition is benign and self-clearing, and it blocked the whole host while it
lasted.

**Nothing about the absence stops the reconciler.** `Adapter.Stop` and
`Adapter.Delete` both return success when the VM is already gone
(`internal/adapters/tart/adapter.go`), and the drain worker does not consult the
inventory observation at all. A cleanup row whose VM vanished therefore reaches
`deleted` on its own; the host-wide block was pure collateral damage. When the
drain is instead permanently stuck — the 2026-07-25 `deregister:runner_busy`
registration leak — the row persists, and so did the host-wide block, forever.

The hazard was proven by code reading. It did **not** fire in that incident: the
VM was present and stopped, so this branch was never reached.

## Decision

**1. Absence is a distinct observed existence state, `InstancePowerAbsent`.** It
is set only from a Tart read that *succeeded*: a failed or unparseable
`tart list` remains `Unavailable("Tart inventory unavailable")`, and absence is
never inferred from an error. It is distinct from `stopped` (the VM exists,
powered off) and from `unknown` (nothing was read), so no caller can confuse gone
with unread. This places no new trust in the enumeration: `Delete` already treats
a miss as "already gone", and `Running` already returns false for a miss, which is
what releases the stopped-recovery drain.

**2. The narrowing is bounded to the states where a reconciler exists.** A live
row whose owned VM a successful enumeration did not list carries per-instance
`absent` power, and the rest of the inventory stays `Fresh`, only when the row is
in a cleanup state (`draining`, `deregistering`, `stopping`). `Planned` keeps
`unknown` power, since its VM has not been cloned yet and nothing was observed.
Every other live state keeps the host-wide `Unavailable`: a row leaves `Planned`
only after `Clone` succeeded, so absence there is an out-of-band removal that no
operation is reconciling, and reclaiming it would mean deregistering a runner
GitHub may still hand a job to — new destructive authority derived from an
enumeration miss. That block is loud, correct, and resolved with
`fleet operations discharge`.

**3. No destructive guard moves.** `assignmentRecoveries` keeps an exact
`Power == stopped` comparison, so an absent VM plans no drain, no deregistration
and no delete. `DrainExecutor`'s per-attempt re-verification, the ownership match,
the confirmation freshness window, and the discharge guards are untouched.
`domain.InstancePower.ProvenIdle` exists for the non-destructive judgements and
carries that warning at its definition.

**4. Absence frees the host vector exactly where stopped already does.**
`ConsumesHostResources` treats proven-absent like proven-stopped inside the
cleanup states and stays fail-closed everywhere else. The alternative — an absent
VM consuming its vector — would make a deleted VM pin the host to its platform
harder than a live one and starve the other platform through `DeriveHostMode`.
Admission remains bounded independently by the measured host probe in
`hostObservation`, which never consults Tart.

**5. Absence is parked, for ADR 0021's own reason.** `deadLetterMetrics` admits a
proven-idle VM, absent or stopped, and still refuses `unknown`. A generation swap
cannot interrupt a guest that does not exist, so withholding it would let the
wedge ADR 0021 removed return in a new shape: a dead letter whose VM is already
gone disabling automatic updates forever.

## Consequences

One stale cleanup row no longer stops every CI queue on the machine, and the
benign delete-then-advance race no longer costs a planning tick.

No new alert is needed, because the persistent case is already alertable: a
cleanup row whose VM is gone is either finishing within seconds, or its drain is
visible in `operations.retrying` with a classified failure code (ADR 0020), or it
is dead-lettered and now counted by `fleet_operations_parked` (ADR 0021).

`fleet.v1` is unchanged: instance power is not published in any DTO.

`StateFailed` is deliberately left host-wide fail-closed even though
`reapableStates` admits it. No code path currently writes that state, so
exempting it would be speculative.
