# ADR 0049: An aged head keeps its place across the platform boundary

## Status

Accepted. Resolves the deferral in
[ADR 0030](0030-a-reserved-head-holds-one-repository-slot.md)'s "Not addressed
here" — the shared residual arbitrated one platform at a time — and extends
[ADR 0045](0045-a-reservation-withholds-order-not-a-vector.md)'s no-jump rule to
the four passes that never applied it. Everything both records decide remains
accepted; this decision changes which demands a pass is offered, and nothing
about how it judges them.

## Context

On 2026-08-09 at 19:22:50Z a 6 CPU / 12288 MiB instance freed on the mac mini.
Two demands wanted exactly that vector:

| demand | platform | vector | waiting |
|---|---|---|---|
| `sudoku-repo/builder` — the owner's App Store release | macOS | 6 CPU / 12288 MiB | **2h 01m** |
| `rnw-repo/xl` — a pull-request E2E job | Linux | 6 CPU / 12288 MiB | ~1h 20m |

At 19:22:55Z — five seconds later — the scheduler admitted the Linux demand. The
two-hour-old macOS demand kept waiting, and got the host only when an operator
cancelled the Linux job by hand. Identical vectors, identical host, identical
envelope; the only difference was platform, and the younger demand won by
forty-one minutes.

ADR 0030 predicted this in as many words and declined to fix it: "a young Linux
demand can still take a residual that an older macOS demand would have won under
a single global ordering ... it is a separate decision". The deferral was
reasonable when the two platforms rarely contended for the same vector. ADR 0034's
shared-label amendment made them contend constantly.

### One hole, four passes

The issue filed against this named `planLinux` running before the macOS passes.
That is half the mechanism, and the missing half is what decides the size of the
fix.

`PlanTick` dispatches on the head of a single global order — `priorityOrder` over
every demand of both platforms — so a tick whose highest-ranked demand is macOS
runs the macOS passes FIRST. A younger Linux demand cannot outrank an older
macOS one merely by being planned earlier, because on that tick it is not planned
earlier. There is no unconditional Linux-first pass to delete.

What there is, is one hole appearing in every pass that plans Linux admission,
and it always has the same shape: the pass receives the WHOLE envelope and admits
from it without ever being told about a demand of the other platform that
outranks its candidates. Which lane the tick is in decides only which pass has
the hole.

**The macOS-headed lane** is the production incident. The macOS head ranks first
and cannot spawn — on 2026-08-09 because a live builder held the profile's only
`maxActive` slot — so `planBehindInfeasibleMacHead` fills the residual behind it
rather than idling the host. That is right, and it filled the residual by handing
`planLinux` the full envelope. Reservations are authored inside `planLinux` over
Linux demands, so an aged macOS head never had one, and `jumpsTheReservedHead` —
the predicate ADR 0045 spent an entire record establishing — was never asked
about it. Nothing checked the head at all.

**The Linux-headed lane** is the issue's own description, and it is real once
priority tiers exist. The property this record adds found it on seed 1 tick 71 of
the tiered world: an aged `large` of the DEFAULT tier (21m30s) was admitted into
the 4 CPU / 7168 MiB vector an aged `maestro` of the RELEASE tier (20m0s) was
waiting for. `planLinuxWithCoexistence` had planned the whole Linux queue against
the whole envelope; `fillMacRemainder` then found the cores gone. Age alone would
have ranked the `large` first and the tick would have been correct — it is the
tier that inverts, which is exactly the interaction with issue #224 that #225
predicted and that no test covered.

**A macOS head that DID spawn** leaves the same hole behind it. `fillLinuxRemainder`
fills what the macOS pass left over, and it too took the whole residual: property
(r) found it on seed 3 tick 55 of the mac-mini world, where an aged `maestro` was
admitted and an aged `xl` then took the 6 CPU / 12288 MiB vector the SECOND macOS
demand — an older `builder` — was waiting for.

The complementary direction was never broken. `fillMacRemainder` runs
`reservedRemainderDemands` and `chargeReservedHead` over its macOS candidates, so
a macOS demand has never been able to take a reserved LINUX head's vector. The
defect is an asymmetry between two passes that face each other, which is the
third time this codebase has shipped exactly that shape: ADR 0029 repaired it for
the reservation veto, ADR 0024 for the identity veto, and this record for the
no-jump rule. The lesson each time is the same one, and it is why the fix here is
applied at four call sites rather than at the one the incident named: when a rule
lives in a pass rather than in the thing it protects, every new pass is a fresh
chance to forget it.

## Decision

Every pass that plans the Linux queue while a macOS queue is waiting is offered
only the demands that may sit BESIDE an aged macOS head — never the ones that
would land INTO its vector.

`linuxBesideAgedMacHead` builds the reservation the macOS head would have had if
it were Linux — its key, its profile's vector, its creation time — and runs each
Linux candidate through the same `jumpsTheReservedHead` ADR 0045 defines. It is
applied at the four call sites that plan Linux admission with a macOS queue in
hand:

| pass | lane |
|---|---|
| `planBehindInfeasibleMacHead` | macOS head that cannot spawn — the production incident |
| `fillLinuxRemainder` | macOS head that DID spawn, Linux filling what is left |
| `planLinuxWithCoexistence` | Linux head beside a live macOS cohort — the tiered inversion |
| `PlanTick`'s plain Linux lane | Linux head on a host with no macOS instance |

This is ordering, not capacity. No veto changes, no envelope shrinks, nothing is
charged, and `Next` is untouched — the head is a predicate argument for the
length of one pass and is not persisted anywhere.

Three terms keep it from becoming a platform preference, and each of the three
was put there by a failing property rather than by argument:

- **The head must be AGED.** Within a pass it is the fairness age that turns
  waiting into precedence (ADR 0004's global FIFO lane). A rule that gave a
  one-minute-old macOS demand this standing would be "macOS first" wearing an
  ordering rule's clothes.

- **Only a candidate the head OUTRANKS is bound.** `priorityRank` gives every
  demand its position in the one order ADR 0037 defines over both platforms, and
  a Linux demand ahead of the macOS head is never filtered — it is entitled to go
  first and the head is the one that must wait. Without this term the rule reads
  "macOS beats Linux", which is not what the incident was about.

  The rank must be taken from `normalizedDemands`, not from the caller's slices.
  `priorityOrder`'s aged band is a STABLE sort by tier ALONE; age order is
  inherited from the order the caller already had, which `normalizedDemands`
  establishes once per tick. A rank built from a re-assembled slice ranks by
  whichever platform was appended first — and did, silently reversing the
  incident test until the test caught it.

- **Only the candidate that eats INTO the vector is bound.** Work that coexists
  with the head cannot delay it by a tick, and refusing it would sterilize the
  host — the whole of issue #226, and the reason ADR 0045 draws this distinction
  for the within-Linux case. The remainder is taken from `agedLinuxEnvelope`,
  because the head is aged and aged work does not pay the advisory CPU-idle
  clamp; judging it against `linuxFree` would withhold capacity the head was
  never denied.

## Consequences

The 2026-08-09 tick is pinned as
`TestAgedMacHeadIsNotJumpedByYoungerLinuxWantingItsVector`: a live builder holds
the profile's only active slot, a 2h01m macOS head waits behind it, and an 80m
Linux demand of an equal vector is refused the residual. It fails without
`linuxBesideAgedMacHead` and passes with it. Two guard tests fail if the bound
over-reaches — one admitting work that fits beside the aged head, one admitting
Linux behind a YOUNG macOS head.

The harness gains property **(r)**, `crossPlatformInversionChecker`: no tick
admits a demand that could take the whole vector an older aged demand of the
other platform is waiting for, when it could have been admitted beside it
instead. It is the first oracle in this package that compares two demands across
the platform boundary — property (l) is confined to one lane by construction, and
property (b) never counted these ticks because the head was not feasible on them.
It exempts what ADR 0045 exempts (work that fits beside the head), what ADR 0038
exempts (a head its own repository cap holds out, which lends its vector), and
anything the head does not outrank. It deliberately does NOT require the head to
be feasible on the tick it judges; that requirement is precisely what let the
incident run for two hours unseen.

The property paid for itself three times before the sweep went green, and each
time it named a defect the incident report did not:

1. **Seed 1 tick 99, tiered world** — the oracle's own first draft judged every
   aged waiting demand rather than the head, and called a queue doing its job an
   inversion. The oracle was narrowed to the head, which is what ADR 0045's rule
   is actually about.
2. **Seed 1 tick 71, tiered world** — a real scheduler defect, and the one the
   issue described: `planLinuxWithCoexistence` admitted a DEFAULT-tier `large`
   into the vector a RELEASE-tier `maestro` was waiting for. Age alone would have
   ranked the `large` first; the tier is what inverts, which is the interaction
   with issue #224 that #225 predicted and nothing covered.
3. **Seed 3 tick 55, mac-mini world** — a real scheduler defect in a third pass,
   `fillLinuxRemainder`, plus an oracle bug: it clamped its envelope by the
   host's measured availability, so it read a 6-core head as unable to fit five
   available cores on a tick the scheduler had judged it against ten.

The simulation worlds already generated this contention: `simProfiles` has had a
Linux `xl` and a macOS `builder` at an identical 6 CPU / 12288 MiB, with
`MaxActive: 1` on the builder and `MixedPlatformAdmission` on, since before the
incident. No generator change was needed; the harness could always produce the
tick and had no property that looked at it.

**Corpus movement.** Four of six arms are byte-identical —
`mac-studio-4x10-budget`, `sequence-reset-linux-large`, `federated-maestro-scope`
and `geekom-linux-amd64`. The two that moved are exactly the two whose worlds mix
platforms, which is the only place this rule can bind:

| arm | applied | spawns | instances | digest |
|---|---|---|---|---|
| `m4-mac-mini` before | 69 | 51 | 51 | `666eb149486c754f` |
| `m4-mac-mini` after | 69 | 51 | 51 | `3c89e09f93d9bdb1` |
| `tiered-release-priority` before | 81 | 54 | 54 | `22dbe1024fc5a918` |
| `tiered-release-priority` after | 84 | 54 | 54 | `b33edaa50fe87d2b` |

The same 51 and 54 spawns produce the same 51 and 54 instances on both sides of
the change. No work is refused and none is added; what moved is WHICH tick each
spawn lands on, which is what an ordering rule is. Swept one seed at a time, both
arms are identical at seed 1 and diverge from seed 2 onward. The tiered arm needs
three more applied plans to reach the same 54 spawns — the cost of the rule, and
the mirror image of ADR 0045's own arithmetic, which bought one applied plan back.

## Not addressed here

This does not give a macOS head a durable reservation. A cross-platform
reservation would have to answer what happens when the two platform lanes
disagree about the same slot across ticks, and no incident has asked that
question.

`planMacHandoff`'s bounded drain backfill is left alone. It is reachable only
with `mixedPlatformAdmission` off, it is latched to one job, and
`boundedDrainBackfill` restricts its candidates to the SMALLEST Linux profile —
which cannot hold a larger macOS head's vector, so the rule would be a no-op on
every configuration in the fleet. A configuration whose smallest Linux profile
equals or exceeds a macOS profile would need it; none exists, and inventing the
branch would add an argument no evidence supports.

It does not revisit which pass plans first. Nothing here reorders `PlanTick`; the
global order that chooses the head is ADR 0037's and is unchanged. The fix is
that each pass is told about the head it was previously planned in ignorance of,
not that the passes are merged into one.
