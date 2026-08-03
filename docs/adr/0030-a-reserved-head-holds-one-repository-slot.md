# ADR 0030: A reserved head holds one repository slot, not its repository

## Status

Accepted. Amends the repository condition of
[ADR 0029](0029-remainder-admission-behind-a-reservation.md); everything else in
that decision — the vector condition, the infeasible-head rule it inherits from
[ADR 0017](0017-infeasible-reservation-residual-backfill.md), and the passes it
binds — remains accepted unchanged. The aged-FIFO and scheduling-class rules of
[ADR 0004](0004-bounded-control-plane-priority.md), the repository caps of
[ADR 0012](0012-shared-cross-platform-capacity.md), and the
one-admission-per-tick rule of
[ADR 0027](0027-one-tick-admits-a-demand-once.md) are unchanged and still bind
every demand admitted here.

## Context

ADR 0029 made every pass that admits work while another pass holds a reservation
prove two things: that the admission cannot take the reserved head's **vector**,
and that it cannot take the reserved head's **repository cap slot**. The second
condition existed because `activeRepoCounts` counts every live instance
regardless of platform, so a macOS spawn in the head's repository can consume the
exact slot the head is waiting for — a blocker remainder arithmetic cannot see.

It was stated as a wholesale exclusion: *the pass never bids for the reserved
head's repository*. That is sound but far stronger than its own justification,
and the gap showed up in production nine hours after the decision shipped.

### The counterexample

Production Mac mini (Mac16,10: 10 cores, 24 GiB), 2026-08-03 ~06:50Z, running
v0.1.332 — the build that shipped ADR 0029.

- The reserved aged head was a Linux `xl` (6 CPU / 12288 MB) from
  `rnw-community/rnw-community`, infeasible beside a live `xl` from that same
  repository. ADR 0029's vector condition correctly released the residual: a
  head that does not fit is waiting on the live instance, not on backfill.
- Free residual: 4 cores, ~8 GiB.
- Three macOS `maestro` demands (4 CPU / 7168 MB), aged **4h39m**, from
  `rnw-community/rnw-community` — the head's own repository. That repository's
  cap is **4**, and exactly **one** instance was live.
- Result: they were not candidates at all. A **younger** `large` demand from a
  different repository took the residual instead.

Three of four cap slots stood free. The head needs one. Admitting a maestro
would have left two spare, and the cap answer `planLinux` computes for the head
on the tick its vector frees would have been identical either way. The refusal
protected nothing.

Worse, it did not merely refuse — it **inverted aged FIFO**, the property the
whole reservation mechanism exists to protect. Excluding the head's repository
removes its demands from the candidate list entirely, so a young demand from any
other repository wins a residual that the oldest work on the host was never
allowed to bid for. That is the starvation shape ADR 0017 and ADR 0029 were both
written to end, reintroduced by the guard itself.

The cost has the same units ADR 0029 stated: an idle vector the size of the
starved profile for the duration of the blocking job — here 4 cores and 7168 MB,
against demands that had already waited 4h39m.

## Decision

The repository condition becomes a **slot count, not a veto**.

A complementary pass may admit work from the reserved head's repository exactly
up to the repository's **slack**:

```
slack(headRepo) = RepoCaps[headRepo] - occupied(headRepo) - 1
```

where `occupied` is the same occupancy `planLinux`'s `feasible()` will read on
the tick the head's vector frees — live instances counted by `activeRepoCounts`
plus everything this plan already admits, charged by `appendPlannedSpawns`
exactly as the vector condition charges it — and the trailing `- 1` sets aside
the head's own future slot before anything else may bid.

Consequently:

- `slack <= 0` — the head is waiting for the last free slot, or for one that is
  already gone. Nothing from its repository may be admitted. This is ADR 0029's
  wholesale exclusion, and it remains exactly right in this case.
- `slack > 0` — admitting up to `slack` demands from that repository leaves the
  head's slot free by construction. The cap answer for the head is unchanged, so
  the admission cannot delay it and must not be refused.

Where slack is scarce it goes to the **highest-priority** demands of that
repository, in the same global order every other admission obeys: aged before
young, then the existing control-plane and round-robin lanes. Aged work from the
head's own repository therefore outranks young work from any other repository
for the residual, which is what aged FIFO always said.

The **vector condition is unchanged**, including its infeasible-head rule. Both
conditions still bind, and both still bind every pass ADR 0029 named:
`fillMacRemainder` and `planLinuxHandoff`'s bounded one-shot wave.

The reservation contract is preserved, as in ADR 0017 and ADR 0029, by
**ordering, not by idleness**: the reserved head is re-checked first on every
later tick, so it wins the first vector large enough for it.

### Amendment 2026-08-03: macOS admission enforces the cap it is charged for

The slack arithmetic above rests on an assumption this record stated but did not
enforce: that `RepoCaps` bounds every platform, because `activeRepoCounts`
charges every platform. It did not. `appendMacSpawns` — the single function every
macOS spawn in the planner is emitted by, across the head branch, the mixed
remainder, both handoff waves, and the exclusive path — bounded admission by the
resource envelope and by the profile's `MaxActive`, and read `Config.RepoCaps`
nowhere. A repository's cap bounded its Linux work and not its macOS work, so
`slack = cap - occupied - 1` set aside a slot that a later macOS spawn could take
anyway. Deterministic simulation found the resulting over-cap occupancy on 11 of
150 seeds (ADR 0031, finding 2).

`appendMacSpawns` now applies the same cap the Linux allocator applies, against
the same occupancy every other pass reads: `activeRepoCounts` over live
instances plus this plan's own spawns (`appendPlannedSpawns`). A capped
repository is SKIPPED rather than ending the pass — every candidate there shares
one profile vector, so an exhausted envelope ends admission but an exhausted cap
only ends that repository's turn, and the next repository's work takes the
vector. That is what `exactSelect` already does for Linux.

The reserved-slack arithmetic is unchanged and is now sound: a slot the head sets
aside cannot be taken by a platform that ignores the cap, because no such
platform remains.

## Consequences

The pass is strictly more permissive than ADR 0029 in exactly one direction —
same-repository work that provably cannot cost the head its slot — and identical
everywhere else. Every case ADR 0029's tests proved still holds: a feasible head
keeps its whole vector withheld, a head at its last cap slot keeps that slot, one
tick still admits a demand once, and the head still starts first when its vector
frees.

Aged FIFO is restored for the residual: the oldest eligible demand wins it,
whether or not it shares the head's repository.

The rule reads the cap and occupancy at plan time, so it inherits their
semantics: `activeRepoCounts` ignores idle and teardown instances, and an
unconfigured repository cap is 1 — under which `slack` is always `<= 0` and the
behavior is byte-for-byte ADR 0029's.

## Evidence

- `tests/replay/reserved_repo_slot_incident_test.go`: the incident over 12 ticks
  — a control arm at the last free slot admitting nothing on any tick, then the
  live cap admitting the aged same-repository maestro on the first tick, the
  reservation surviving every later tick with no drain planned, and the reserved
  head taking the released vector FIRST ahead of the maestros still queued.
- `internal/scheduler/scheduler_reserved_remainder_test.go`:
  `TestMacRemainderAdmitsSameRepositoryWorkTheHeadCanSpare` (one topology, two
  caps: slack lends, the last slot does not),
  `TestAgedSameRepositoryWorkOutranksYoungOtherRepositoryWork` (the aged-FIFO
  inversion, red before this decision: the young other-repository demand was the
  one admitted), `TestBoundedHandoffWaveAdmitsSameRepositoryWorkTheHeadCanSpare`
  (the same two answers from the second bound pass, with the handoff latch
  proving which pass decided), and
  `TestReservedRemainderDemandsKeepsOnlyWhatTheHeadCanSpare` (the filter itself:
  no reservation drops nothing, and scarce slack goes to the aged demand).
- Every ADR 0029 test is retained unchanged, including
  `TestMacRemainderNeverTakesTheReservedHeadsRepositorySlot`, which is now the
  `slack == 0` case and still passes.

## Not addressed here

The residual is shared across platforms but arbitrated one platform at a time:
`planLinux` (with `safeBackfill`) plans before `fillMacRemainder`, so a young
Linux demand can still take a residual that an older macOS demand would have won
under a single global ordering. That is what admitted the young `large` in the
tick above once the maestros were excluded, and it is a separate decision —
co-planning the shared residual across platforms — not a repository-cap
question. This ADR removes the exclusion that starved those maestros for 4h39m;
it does not change which pass plans first.

macOS admission still has no repository-cap accounting of its own, exactly as
ADR 0029 recorded. This decision only refines the guarantee that a macOS spawn
cannot take a *reserved head's* cap slot.
