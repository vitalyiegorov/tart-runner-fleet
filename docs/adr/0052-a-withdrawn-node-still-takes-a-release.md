# ADR 0052: A withdrawn node still takes a release

## Status

Accepted. Implements
[issue #320](https://github.com/vitalyiegorov/tart-runner-fleet/issues/320).

It **amends [ADR 0048](0048-a-node-arranges-the-quiescence-its-update-needs.md)**
in one clause — what the update transaction's gate asks of the running daemon —
and leaves everything else in it, and in
[ADR 0011](0011-atomic-production-updates.md), standing. It changes nothing about
[ADR 0047](0047-a-node-that-cannot-admit-yields-its-sessions.md): a node that
cannot admit still releases its sessions, still reports NOT READY, and still
fails its own `session yield` check.

## Context

ADR 0047 gave a node under host pressure a duty: release the scale-set sessions
it cannot serve, so a sibling's work is not held behind a node that will never
run it. A withdrawn node's critical observations go `stale` with the detail
`session_yielded`, and the node reports NOT READY. That was deliberate, and it
remains right for the scheduler.

ADR 0048 then gave the update transaction a gate: the node must be quiescent
before a generation is swapped, and quiescence was proved with
`fleet status --require-ready`. The candidate was proved the same way after the
swap.

The two decisions compose into a trap. On 2026-09-14 the mac studio sat below its
`minFreeDiskGb` reserve from 04:38Z, withdrew every session, and ran zero
instances. v0.1.552 was downloaded at 06:08Z and the drain logged
`instances=0` at 06:38Z. Every updater run after that failed with
`prepare update: exit status 5` — the readiness gate refusing. The node was
maximally quiescent: nothing was running, and nothing could be admitted. It was
also permanently un-updatable, and the fixes written for exactly that outage
(#316, #317) reached it only because a human bridged them across by hand.

Dropping readiness from the quiescence gate alone would not have worked: the
transaction's proof of the *candidate* asked the same question and would have
rolled the release straight back.

The conflation is in the word. "Ready" meant two things at once: **this process
is working** — it is ticking, its store is writable, its observations are being
taken — and **this node is admitting work**. A release transaction needs the
first. The scheduler needs the second. A withdrawn node satisfies the first and
not the second, and it is the only state in which the two ever disagree.

## Decision

**Readiness splits in two, and a release is gated on health, not on admission.**

- **`healthy`**: the last successful tick is fresh, and every critical
  observation is either fresh or stale with the detail `session_yielded`. The
  store's own `operations` observation is never excused, so health still means
  the store is writable. A withdrawal is excused only while it is still being
  written every tick: an excused observation that has expired is a stopped loop,
  which is the fault this predicate exists to catch.
- **`ready`**: healthy **and** admitting — unchanged semantics, unchanged
  `/readyz`, unchanged `--require-ready`, unchanged meaning for the scheduler,
  for `fleet doctor`'s `scheduler ready` row, and for every operator who has
  learned to read it.

The update transaction gates on **healthy + zero live instances** for
quiescence, and proves the candidate **healthy as exactly that version and
mode** afterwards. Both the launchd and the systemd transaction share those two
helpers, so they cannot diverge. What still defers a generation swap is exactly
what ADR 0011 named and ADR 0048 left standing: a running instance, or a
retrying operation. Admission is the scheduler's concern; it is not the
release's.

`healthy` is published in the `fleet.v1` status document beside `ready`, and
`fleet status --require-healthy` exits 5 when it is false. A daemon older than
this ADR publishes no such field; a client reads its `ready` instead, which is
the strictly stronger claim and the only word that daemon had for the question.
Absence is never read as a pass — this is the one `Effective*` accessor that
does not treat silence as consent, because swapping a generation under a daemon
that never said it was working is the failure mode it exists to prevent.

## Consequences

A node withdrawn by host pressure now installs releases on the ordinary updater
schedule. This is the node most likely to be running defective code, because
withdrawal is usually a symptom of something an operator wants fixed.

`fleet status` on a withdrawn node now says both things: NOT READY, and healthy.
The `update drain` doctor row, on a drain that has reached zero instances, names
whether the release can land or what is still blocking the candidate, instead of
rendering a drain that appears able to run forever.

A generation is now swapped under a daemon that is not serving traffic. That is
strictly safer than the case already permitted — swapping under an idle
*admitting* node — because a withdrawn node holds no sessions GitHub could bind
work to during the swap.

## What this does not address

It does not change when a node withdraws or rejoins: ADR 0047's hysteresis,
reasons and reconciliation are untouched. It does not make a withdrawn node
serve; the jobs bound to its scale sets still wait for the hub owed by
[issue #168](https://github.com/vitalyiegorov/tart-runner-fleet/issues/168). It
does not relax ADR 0011: a release still never interrupts running work. And it
does not make health a service-level signal — a healthy node that admits nothing
is still a finding on `admission`, `session yield`, and `scheduler ready`.

## Alternatives considered

**Drop the readiness gate from quiescence only.** One line, and wrong: the
candidate proof asks the same question after the swap, so the transaction would
have rolled back every release on the same node instead of refusing it up front.

**Let the quiescence gate ignore health entirely and count instances.** A
generation swapped under a daemon that has stopped ticking or cannot write its
store is how a node ends an update in a state no journal describes. The gate has
to prove something; health is the least it can prove.

**Skip the drain for a withdrawn node.** The drain is already inert there —
there is nothing to stop admitting — so this would have added a special case
without changing an outcome.
