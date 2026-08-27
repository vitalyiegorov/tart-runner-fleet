# ADR 0047: A node that cannot admit yields its sessions

## Status

Accepted. Implements
[issue #297](https://github.com/vitalyiegorov/tart-runner-fleet/issues/297),
which forked the symptom recorded in
[issue #292](https://github.com/vitalyiegorov/tart-runner-fleet/issues/292).

It amends nothing in
[ADR 0034](0034-a-node-serves-the-scale-sets-it-owns.md) — a node still serves
exactly the scale sets it owns and still reads no other node's state — and it
does not anticipate the hub ADR still owed by
[issue #168](https://github.com/vitalyiegorov/tart-runner-fleet/issues/168). It
answers a narrower question ADR 0034 left open: what a node owes the fleet when
it owns a scale set it cannot currently serve.

## Context

GitHub binds a queued job to exactly one runner scale set at queue time, and
ADR 0034 gives that scale set exactly one session: a second listener is refused
with 409. Both rules are load-bearing and neither is ours to change.

Their consequence had never been stated. When two nodes advertise the same
labels through their own scale sets — `trf-budgie-large` on node A and
`trf-budgie-large-studio` on node C, which
[ADR 0034 amendment 2026-08-04b](0034-a-node-serves-the-scale-sets-it-owns.md)
explicitly permits — a job bound to one node's set is unreachable by the other.
A node that cannot admit is therefore not idle. It is holding work no sibling
is allowed to take.

On 2026-08-26 node C sat below its `minFreeDiskGb` reserve for a full day while
node A ran at load 1.4 with empty queues. Node C's guardrail was correct: its
disk had 22 GiB free against a 60 GiB reserve, and admitting a Tart clone there
would have been the incident. What was wrong was that node C kept its sessions
while refusing everything they delivered. One `budgie-at/budgie` pull request
gate waited 11h26m; suuudokuuu's iOS shards, this repository's own `Preflight`,
and fourteen other jobs waited behind it. Every signal the fleet publishes read
healthy on both nodes, because a node that has withdrawn and a node that is
merely idle produce identical queues, identical instance counts, and identical
host pressure.

The measurement that decided this ADR was taken on 2026-08-27, by stopping node
C's daemon: its sessions closed, and GitHub bound the re-queued jobs to node A
within seconds. Two facts followed. A scale set whose session is closed stops
attracting new work — session presence, not scale-set existence, is what makes
a set eligible. And a job already bound does not migrate; it had to be
cancelled and re-run by hand.

## Decision

**A node that has concluded it cannot admit releases the sessions behind its
scale sets, and says so.**

The rule is node-local and needs no knowledge of any sibling, so shared-nothing
survives intact. The node is not asked whether another node could run the work
— it cannot know that — only whether *it* can, which is the same question its
admission guard already answers every tick.

Four constraints make the rule safe:

1. **Withdrawal is not drain.** Running instances finish, and the sessions they
   report completion through are never cut: a node with a live instance or a
   retrying operation holds its sessions regardless of admission.
2. **Both directions are hysteretic.** Admission must be refused continuously
   for `sessionYieldBlockedForSeconds` (default 600) before withdrawing, and
   allowed continuously for `sessionYieldHealthyForSeconds` (default 120)
   before rejoining. A node oscillating around its disk reserve must not
   oscillate its sessions.
3. **Reconciliation is level-triggered.** Every tick drives the sessions toward
   the conclusion the policy holds. A release GitHub refuses is not recorded as
   a withdrawal, and is retried; a node that believes it dropped a session the
   broker still holds is the exact state that would strand the jobs this exists
   to free.
4. **A withdrawn node is loud.** It fails its own `session yield` doctor check,
   prints `SESSIONS WITHDRAWN` above its queues in `fleet status`, publishes
   `fleet_session_yielded`, and logs the transition with the admission reason
   that caused it. The invisibility, not the waiting, is what cost eleven
   hours.

## Consequences

Timeliness is bounded by prevention, not rescue. Withdrawal cannot move a job
already bound, so the value is entirely in vacating before a backlog forms —
which is why the blocked bound is minutes and why an operator draining a node
for maintenance should still expect to re-queue whatever was already assigned.

A node that is the only one advertising its labels loses nothing by
withdrawing: the jobs it cannot run wait either way. It gains the honest signal
that it is not serving them.

A withdrawn node reports NOT READY, because its bindings record `stale`
observations with the detail `session_yielded` while it holds no session. This
is deliberate. `stale` says the node observed nothing; `unavailable` would say
something failed, and nothing did.

This does not place jobs, and it is not a step toward doing so node-side.
Cross-node placement with real spillover remains the hub's, whose ADR
[issue #168](https://github.com/vitalyiegorov/tart-runner-fleet/issues/168)
still owes. Yielding is the
honest node-local approximation available without one, and it stays useful
afterwards as a node's own admission of unfitness — a fact any placer would
have to ask for anyway.

## Alternatives considered

**Delete or resize the scale set while blocked.** Definitive — GitHub cannot
bind to a set that does not exist — but it destroys durable IDs the
provisioner wrote back, and a node that fails to recreate them on recovery has
lost its scope rather than paused it.

**Accept the job and fail it fast.** Turns a fleet capacity problem into a red
check on someone's pull request. Rejected outright: the fleet's failures must
not become the consumer's.

**Wait for the hub.** The hub is the right answer and remains scheduled. It is
also unbuilt, and a node hoarding a sibling's work for eleven hours is not a
thing to leave in place until it exists.
