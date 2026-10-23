# ADR 0073: Scale sets advertise the residual envelope

## Status

Accepted. Amended by [ADR 0077](0077-a-slot-count-is-backed-by-the-envelope.md):
Linux headroom is read from `linuxFreeAged` with the node's true slot count,
because the four-slot clamp `agedLinuxEnvelope` applied is now a per-tick
admission pace rather than a capacity.

## Context

Issue #388. On 2026-10-21 the MacBook (host budget 10 CPU / 14 GiB, two slots)
ran one maestro guest (4 CPU / 7 GiB). Its residual was 6 CPU / 7 GiB / one slot,
so no 4x8 guest could start there for up to another ninety minutes. Two 4x8 sets
from two different scopes were still assigned a job each, and those jobs waited
24 and 26 minutes while sibling nodes were idle. GitHub cannot move an assigned
job without a cancel and rerun.

The root cause is the advertised capacity. Every scale set sent the same static
`maxCapacity` on every long poll (`X-ScaleSetMaxCapacity`). Under ADR 0015 that
number is truthful **for one scope**: the smaller of the profile's host capacity
and the scope's repository caps. A node has one envelope (slots, CPU, memory,
the host budget, the physical bound), and every scope and profile on it draws
from that envelope. GitHub adds up what the sets advertise, so a small node with
many scopes kept being offered work its residual could not start.

## Decision

On each poll, a scale set advertises

    min(configured maxCapacity, held + fit)

- **held** is what the set already owns. It is the larger of two counts:
  - the set's own record: live instances of its profile and repositories, plus
    queued demand that no instance runs yet;
  - GitHub's fresh statistics for the set: `totalAssignedJobs`, which
    includes running jobs. This is the count the protocol scales by. The
    other counters (available, acquired) are not documented as disjoint from
    it, and one job can appear in both acquired and assigned, so adding them
    would count one held job twice.

  Taking the larger value is the conservative choice. Counting too much only
  advertises more, which is the old behaviour. Counting too little could
  advertise less than GitHub has already given the set.
- **fit** is `scheduler.Headroom`. It counts how many more of the set's own
  vector fit the node's residual envelope. It uses the functions the planner
  admits with (`linuxFreeAged`, and `agedLinuxEnvelope` for Linux). Every
  demand already waiting on the node is charged first, wherever it is queued,
  and that includes the reserved aged head. A macOS profile is also capped by
  its own `MaxActive`. The result is floored at zero.

The ticker computes both numbers on every ready tick (`TickResult.Headroom`),
from the same observations the plan used. The value lives in one node-wide
holder, and each binding's poll reads it through a provider.

### Fail open

If the headroom is unknown, the set advertises its configured capacity, exactly
as before. Unknown covers these cases:

- no tick has completed yet;
- the last tick was not ready;
- the last view is older than two minutes, or three poll intervals if that is
  longer;
- the view is dated in the future;
- the scheduler cannot size the profile;
- the set's statistics cannot be read.

Advertising zero on missing data would withdraw the node from every scope it
serves whenever a probe hiccups. The fail-closed scheduler would then hide that
failure, because a node that is offered nothing has nothing waiting to report.

### What does not change

- The configured value stays the ceiling. The adapter clamps the provider's
  answer to `[0, maxCapacity]`, so ADR 0015's truthful capacity and its
  validation keep their meaning. The dynamic value can only lower it.
- A set never advertises less than it holds, up to that ceiling.
- The residual advertisement applies only with `canonicalJobInventory: true`.
  Without it, the configured capacity is inflated on purpose for queue lookahead
  (ADR 0009), and that mechanism needs the number unchanged.
- ADR 0034's shared labels: GitHub splits a shared queue by advertised
  capacity. With this change, a busy peer advertises less, so its share moves to
  the node that can run the work, and that is the intended result. The
  own-share bound of issue #153 reads statistics, not the advertisement, so it
  is unaffected.

### Concurrent polls

Sets poll concurrently. Each one reads the same residual and may advertise all
of it, so for a short window a node can be offered that residual once per set.
The window lasts only until the poll carrying the stale figure ends, at most one
poll period of about 50 s. After that, the first placement is charged on the next
tick and every later poll advertises net of it. A placement does not
double-count in the meantime: the new job raises `held` by one and lowers `fit`
by one.

We accepted this bounded over-offer and rejected the alternatives:

- **Dividing** the residual among the node's sets would leave a single-node
  scope waiting for its turn on an idle machine. With ten scopes and two free
  slots, eight sets would advertise nothing.
- **Serialising** the residual through a lease taken at poll start would let a
  set with no work hold the residual for its whole poll, and re-take it the
  moment it re-polls. That trades a bounded over-offer for unbounded
  starvation.
- **Cancelling** open polls when the residual shrinks would make the window
  shorter, but it adds a mechanism to the session lifecycle that ADR 0063 and
  the recovery code would have to reason about. It is not built.

The over-offer needs several scopes' jobs to arrive within one poll on a node
whose residual is nearly exhausted. The defect it replaces was a permanent
over-offer.

## Consequences

- A small node with many scopes stops collecting work it cannot start, and
  GitHub places that work on siblings that have room.
- A node whose sets advertise only what they hold gets work again on the first
  tick after its residual frees. The simulation scenario pins this
  (`TestIssue388SmallNodeStopsAdvertisingAVectorItCannotStart`).
- Platform lanes (exclusive macOS, drain-and-switch, handoffs) are not modelled
  in `fit`. They decide what starts first, not whether one more instance fits,
  and modelling them could only lower the advertisement.

## Open risk

GitHub does not document what the broker does when a set advertises less than
it has already assigned. This decision avoids that case: the advertisement is
never below `held`, and `held` includes GitHub's own assigned count whenever
fresh statistics are available. The remaining gap is a job assigned in the
milliseconds between a poll response and the next poll, before its message is
committed. GitHub made that assignment under the previous, larger header. We
expect the broker to deliver its message on the next poll whatever that poll's
header says, because messages are cursor-ordered and not capacity-gated, but
this has not been verified against GitHub. If it turns out to be false, the fix is to floor
`held` at the set's last delivered `assigned` count without any freshness
bound.
