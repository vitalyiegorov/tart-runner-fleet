# ADR 0053: A node declares the policy it runs with

## Status

Accepted. Implements option 3 of
[issue #304](https://github.com/vitalyiegorov/tart-runner-fleet/issues/304),
which was split out of [#263](https://github.com/vitalyiegorov/tart-runner-fleet/issues/263)
as that incident's remaining ask.

It changes nothing in
[ADR 0034](0034-a-node-serves-the-scale-sets-it-owns.md): no node becomes aware
of another, nothing probes a peer, and the comparison still happens in an
operator's hand or in CI, never in a daemon. It extends ADR 0034's enforcement
§2 — the cross-node rules `fleet config validate` asserts over two files — to a
second artifact those rules could never read: the status document a running node
publishes about itself.

## Context

On 2026-08-23 the mac studio's `macosBurst` lacked `"mixedPlatformAdmission":
true`. The mac mini's had it. Without the key, any tick whose global FIFO head is
an infeasible macOS demand goes to `planMacHandoff`, whose only remedy is
draining an aged **Linux** instance — and when there is none, the tick plans
nothing. The studio therefore stalled Linux admission behind every long macOS
build for **weeks**, across at least three observed episodes of 36+ minutes each,
with `linux-2x4` demands waiting beside two idle budget CPU.

It was filed as a scheduler defect. It was argued over SSH for hours across six
configuration knobs. The scheduler was correct throughout: the node had simply
never been given the setting.

Nothing caught it, and nothing could have:

- `fleet status`, `fleet doctor`, and every check either node ran are
  **single-node**. None of them knows another node exists, let alone what it is
  configured with.
- ADR 0034's cross-node rules assert only when two *paths* are handed to one
  command. Nothing runs that routinely, and nothing runs it as part of a deploy.
- The drift is silent in both directions, because **a missing optional key is
  indistinguishable from a deliberate one**. Neither node can report a setting it
  does not have.

`envelope` (added for #263) made the tick's arithmetic readable in one line, and
it would not have named this cause either: the envelope on those ticks looked
unremarkable. The tick was never asked the question.

## Decision

**A node publishes the load-bearing subset of its own effective configuration,
with a digest, and one command diffs two of them.**

- **`config.Policy` is a bounded, credential-free projection** of the effective
  configuration, hand-written rather than derived from the whole `Config`. Three
  rules govern it. Every field is read by a decision — the scheduler, the
  admission guardrails, or the placement of a label — because an unbounded dump
  becomes another thing nobody reads. Nothing secret and nothing path-like
  appears: credentials must never be published, and a per-host path legitimately
  differs between honest nodes, so including one would make every pair disagree
  and teach an operator to ignore the answer. And **nothing is omitted** — every
  field is encoded whatever its value, so an unstated `mixedPlatformAdmission`
  is published as the `false` the scheduler reads rather than as a gap.
- **The document carries its own identity.** `policyDigest` is the hex sha256 of
  the canonical encoding, which is deterministic by construction: fields in
  declaration order, map keys sorted, every slice sorted. Two nodes are compared
  by one twelve-character prefix before anything is diffed, and human
  `fleet status` prints exactly that: `policy 0123456789ab`.
- **`fleet config policy <a> [<b> ...]`** prints one node's policy, or a table of
  the keys on which two or more disagree — key path, then one column per node —
  exiting `5` on drift, `0` when identical, and `2` on anything it could not read.
  Each path is a node configuration **or** a `fleet status --output json`
  document, detected by the presence of `data.policy`, so an operator can compare
  two nodes with neither SSH nor file access. A key one node does not project at
  all renders as `absent`, never as `false`: conflating those two is the whole of
  #304.
- **`fleet doctor` gains an informational `policy` row** that never fails. A
  digest is an identity, not a judgement, and no single node can know whether its
  own policy is the right one — judging that is a comparison, and a comparison
  needs a peer. The row exists so the identity sits beside the checks an operator
  is already reading.

The `policy` object is an additive `fleet.v1` field. A daemon that predates it
publishes nothing, `EffectivePolicy` reports that silence as silence, and every
renderer says "not declared by this daemon" rather than inventing agreement.

The projection's field inventory lives in `internal/config`, which is the package
that can answer for what is load-bearing. The API contract owns only the
envelope — an object of bounded keys plus `policyDigest` — and carries the fields
as decoded JSON. That is what lets a newer daemon publish a key an older client
has never heard of and still be diffed correctly against its peer, which is the
entire point of publishing the document at all.

## Consequences

"What is this node actually running with" is answerable from the same document
that already answers "what is this node doing", for both nodes, from either
node, without a shell on either.

The manual step #304 called "the cheap version that will be skipped" is gone from
`docs/OPERATIONS.md`: the "breached queue SLO while the host still has free
capacity" procedure now ends in `fleet config policy` against the two nodes'
status documents rather than in a hand-written `jq` diff of one block.

A new load-bearing setting now has a second place to be added. A setting that
decides admission and is not projected is invisible to this mechanism exactly as
`mixedPlatformAdmission` was, so the projection is part of the cost of the
knob — and the doc comment each field carries, naming the decision that reads it,
is what makes that cost checkable in review.

Two nodes of different releases may differ in their projected key SET, not only
in values. The diff reports that as drift, which is correct: during a rolling
update it is true and temporary, and treating it as agreement would hide a
release that genuinely stopped projecting something.

## What this does not address

**The comparison is still an operator's or CI's, never a daemon's.** No node
polls another, and nothing alerts on drift. Doing that is the hub's job
([#218](https://github.com/vitalyiegorov/tart-runner-fleet/issues/218),
[#212](https://github.com/vitalyiegorov/tart-runner-fleet/issues/212)): it is the
only component that sees more than one node and already distributes configuration
versions, and it can say "these two nodes disagree on a load-bearing key" without
anybody running a command. This record makes that check cheap to write; it does
not write it.

It does not say which node is **right**. Two nodes may legitimately disagree —
different hardware, a deliberately heterogeneous partition (ADR 0034 §3) — and
nothing here knows the difference between drift and design.

It does not project the whole configuration, and it never will. Anything outside
the projection is exactly as invisible as it was.

## Alternatives considered

**The documented operator step** (#304's option 1). Already written, already the
thing that gets skipped. It survives here only as the sentence that now says to
run a command.

**The hub-side diff** (#304's option 2). The real fix, and it needs a hub. This
record is what that hub will read.

**Publishing the whole effective config, redacted.** Cheaper to write and worse
to own: the redaction list becomes the security boundary, per-host paths make
every honest pair of nodes disagree, and the reader who has to skim two hundred
keys to find one is the reader who stops looking.

**Publishing the file's bytes, or a digest of them.** A digest of the file
answers "are these two files identical", which is never true and never the
question: two nodes legitimately differ in hostname, budget, and scale-set names.
A projection is the only thing that can be equal between two correctly configured
different machines.
