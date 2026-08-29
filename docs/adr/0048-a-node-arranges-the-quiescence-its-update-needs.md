# ADR 0048: A node arranges the quiescence its update needs

## Status

Accepted. Implements
[issue #230](https://github.com/vitalyiegorov/tart-runner-fleet/issues/230) and
[issue #282](https://github.com/vitalyiegorov/tart-runner-fleet/issues/282).

It **amends [ADR 0011](0011-atomic-production-updates.md)** in one clause and
leaves the rest of that decision standing.

## Context

ADR 0011 refuses to swap a generation out from under running work, and lists
its consequence plainly: *"A release cannot interrupt two Maestro VMs, an
exclusive builder, Linux work, or queued work; it retries after the fleet is
quiescent."*

The refusal is right. The retry is where it fails. Quiescence was something the
fleet could only *wait* for, and a node doing its job is never idle, so the gate
selected against exactly the machines that needed the release:

- Node A logged `autoupdate: fleet is not quiescent` **1011 consecutive times**
  while running v0.1.394 (#230). Four scheduler and lifecycle fixes written for
  that node's own contention were merged and could not reach it.
- On 2026-08-25 both Macs were **26 releases behind** for the same reason
  (#282), including the scheduler fixes for the contention keeping them busy.
- On 2026-08-29, after a node had been restarted by hand onto a current
  release, its updater log still ended `autoupdate: fleet is not quiescent` —
  the gate had not opened, an operator had simply bypassed it.

The perverse property is worth naming: **the more useful a node is, the longer
it runs code already known to be defective**, and a defect found in production
has no bounded path to production.

## Decision

**A node that has a generation waiting arranges the quiescence to install it.**

Two changes, and the second is only safe because of the first.

### 1. Queued work no longer defers activation

ADR 0011's "or queued work" clause is amended away. A queued job is not work a
generation swap can interrupt: nothing is running it, and the successor
re-observes the same durable queue on its first tick. Counting it made the gate
unreachable, and would have made the drain below self-defeating, since refusing
admission is precisely what grows a queue.

**Running instances and retrying operations still defer activation**, so the
guarantee ADR 0011 exists for is untouched: no release ever interrupts a job.

### 2. A node may refuse admission to reach quiescence

When a generation newer than the running one has been sitting unapplied under
`releases/` for `updateDrainPendingForSeconds` (default 30 minutes), the node
stops admitting new instances. Live instances finish their jobs normally; what
stops is the arrival of their replacements, which is the only thing standing
between a busy node and a quiescent one. The updater's existing gate then passes
on its own, and the ordinary transaction of ADR 0011 applies the release.

The drain is bounded, abandonable and loud:

- **Bounded.** `updateDrainMaxWaitSeconds` (default 2 hours) ends a drain that
  cannot reach quiescence — a two-hour Maestro guest, a job that will not end.
  Trading a stale binary for a starved queue is the worse bargain, so the node
  resumes admission and serves.
- **Cooled down.** After abandoning, the node serves for
  `updateDrainCooldownSeconds` (default 1 hour) before trying again, so an
  unreachable quiescence cannot become permanent half capacity.
- **Abandonable.** A candidate that disappears — rolled back, or applied by an
  operator — ends the drain at once. Disabling the policy on a draining node
  resumes admission rather than stranding it.
- **Visible.** A failing `update drain` doctor check, an `UPDATE DRAIN` block
  above the queues in `fleet status`, `fleet_update_drain_active`, and one
  logged transition naming the candidate. A healthy node that admits nothing
  looks exactly like a broken one; it must say which it is.

### What the node reads

The daemon decides from one fact it can establish about itself: whether
`releases/` beneath its own installation root holds a version greater than the
one it is running. The updater materialises and verifies a candidate there
*before* the quiescence gate refuses it, so a newer directory that is not
running means precisely "an update is waiting on me". No coordination with the
updater process is required, and no new API.

Every unreadable input — an unreadable directory, an unparseable version, no
configuration path — reports nothing pending. A node must never refuse
admission on the strength of a fact it could not establish.

## Consequences

A busy node now installs its own fixes within roughly `PendingFor + job
duration`, instead of never. The cost is a bounded window of reduced admission
per pending release, taken deliberately and once.

A drain grows the queue it is draining behind. That is expected, not evidence
against the mechanism: the queue-age SLO will breach on a node that drains
through a busy hour, and `MaxWait` is the bound on how long that may last.

This does not make updates urgent. A node with no candidate never drains, and a
node that is already quiescent installs on the updater's own schedule without
any of this machinery engaging.

## Alternatives considered

**A scheduled maintenance window.** Predictable, but it pauses admission on
nodes with nothing to install and still misses a node whose window arrives
mid-job.

**Operator-invoked `fleet update apply-latest`.** This is what actually
happened, twice, by hand. It works and remains available; it is not a fix,
because it requires a human to notice, and the evidence is that nobody did for
1011 attempts.

**Interrupting running work.** Never considered seriously. It is the one thing
ADR 0011 exists to prevent, and this ADR preserves it exactly.
