# ADR 0045: A reservation withholds order, not a vector

## Status

Accepted. Retires condition 1 of
[ADR 0029](0029-remainder-admission-behind-a-reservation.md) — the charge of the
reserved head's vector against a complementary pass — and the corresponding
`free - reservation` branch of
[ADR 0017](0017-infeasible-reservation-residual-backfill.md)'s `safeBackfill`.
Everything else those records decide remains accepted, and this decision is the
end of the argument they started rather than a reversal of it: ADR 0017's
"preserved by **ordering, not by idleness**" is the rule, and each record has
released one more piece of the idleness.

Extends [ADR 0038](0038-a-cap-held-reserved-head-lends-its-vector.md), whose
clause 2 (nothing equal-or-larger is admitted into the head's vector) becomes
unconditional and is now the whole of what a reservation withholds from
capacity. ADR 0029 condition 2 as amended by
[ADR 0030](0030-a-reserved-head-holds-one-repository-slot.md) — the head's own
repository slot — is untouched and is the other thing a reservation still holds.

Amends the feasibility oracle and the property set of
[ADR 0031](0031-deterministic-simulation-testing.md).

## Context

### The incident

Live on the mac studio, 2026-08-18, `fleet doctor`:

```
FAIL   reservation   reservation for budgie-at/budgie/31389581206/1/7647894864237434209
                     of profile linux-2x4 has withheld 2 cpu / 4096 MiB for 1h5m0s
                     on the unjudged axis
FAIL   queue SLO     queue_incident,queue_slo_breached
```

One live `macos-4x7` (4 CPU / 7168 MiB) serving `budgie-at/budgie` against a
declared `hostBudget {cpu: 6, memoryMb: 16384}`, so 2 CPU / 9216 MiB free. The
reserved head was a `linux-2x4` — 2 CPU / 4096 MiB — which fits that envelope
exactly. Nine demands waited behind it, the oldest 3h07m. Host pressure was not
the cause (`cpuIdlePercent 69.66`, `admissionAllowed: true`) and no Linux
instance was live, so the four-slot ceiling was not either.

Reconstructed as a single `PlanTick`, the node's own numbers come back:
`linuxFreeAged` and `linuxFree` both compute `{2 CPU, 9216 MiB, 3 slots}`, the
figure the node published. And on that input **the head is admitted**. Its
vector fits, and `budgie-at/budgie` was one instance into a cap of four, so
`feasible` says yes and `planLinux` spawns it — with the reservation carried in
and with none at all.

### `unjudged` is not a fifth axis. It is the absence of a judgement

`scheduler.feasible` folds exactly two terms, and `reservationAxis` covered
exactly those two, so a plan that JUDGES a held head always names `vector`,
`repository_cap`, or `both`. An empty axis therefore cannot mean "an axis the
classifier does not know". It means the plan never judged the head at all.

A reservation is Linux-authored and only `planLinux` judges one. Every other
exit of `PlanTick` carried `in.Prior` verbatim:

| exit | why the reservation was not judged |
|---|---|
| unusable or invalid observation | nothing was planned at all, correctly |
| `assignmentRecoveries` | a recovery-only tick returns before either lane plans |
| a macOS demand heads the queue and `planLinux` is never reached | the Linux lane did not run |
| `fillLinuxRemainder` returning early | mixed admission off, a drain in the plan, or a non-ready plan |
| `planMacHandoff` | ADR 0005's bounded drain wave plans macOS only |

The mac studio was the third of those: a `macos-4x7` demand aged 3h07m headed
the queue, so `PlanTick` planned the macOS lane and the `linux-2x4` reservation
— minted an hour earlier under a tighter envelope that had since opened — was
carried, tick after tick, unjudged.

### The carried reservation was not inert, and this is the fleet defect

`fillMacRemainder` and `planMacHandoff` charge the held reservation through
`chargeReservedHead`, which withheld the head's vector whenever
`reservedHeadLendsItsVector` was false — that is, whenever the head **fits** the
starvation envelope and is **under** its cap. That is precisely the state
`planLinux` cannot produce, because it admits such a head.

So after ADR 0038 the arithmetic had already closed:

- **`safeBackfill`.** The head is judged in the starvation envelope and backfill
  plans in the throttled one, and the first always contains the second. A head
  refused on the vector can therefore never satisfy `free.Sub(reservation)`, and
  a head refused on the cap lends by ADR 0038. `lends` was a tautology and the
  `free - reservation` branch was **dead code**.
- **`chargeReservedHead`.** Its withhold branch survived only for a reservation
  the tick had not judged — and there it sterilized the head's vector against
  the only pass that was running, on behalf of a head that would have been
  admitted outright had anything asked.

The mac studio is that branch, held for 1h5m0s, published as a fact about
capacity by a plan that had established nothing about capacity.

### The general question, and the verdict

Issue #235 asks whether the axis-by-axis approach is itself the defect. Four
feasibility axes had been found one at a time — identity and `maxActive`
(#217), the envelope (#221/#222), the repository cap (#226/#232) — and it asks
for the one rule that would have pre-empted all four:

> a reservation should withhold its vector **only** when the vector is what the
> head is waiting for, and every other reason to be unadmittable should release
> it.

**The generalisation is sound, and the record shows it is already reached.** It
is exactly what ADR 0017 decided and ADR 0038 completed: `feasible` has two
terms, both release, and there is no third. What the axis-by-axis defect leaves
behind is not a missing axis but three places DERIVING the one fact:

1. `feasible` decided admissibility as a boolean;
2. `reservationAxis` re-stated the same two terms to name the reason;
3. `daemon.recordReservation` re-derived "the head lends" by enumerating the
   axis vocabulary — `vector || repository_cap || both`.

A third feasibility term added to (1) would have been refused by the decision,
left unnamed by (2) — published as `none`, "no axis refuses this head" — and
counted as **not lending** by (3), which is a `fleet doctor` FAIL claiming a
withheld vector. The fifth instance of this seam's one bug was already wired
into the diagnostic, and issue #235 is what it looks like when the enumeration
and the decision disagree.

Two counterweights were weighed and neither survives.

**"Withholding buys a conditional protection."** ADR 0038 established this on the
cap axis, correctly, and it is why a blanket release was refused there. It does
not apply here, because the state this decision changes is one where **the head
is admissible**. There is nothing to protect it from: the next tick that plans
its lane admits it, and until then the fleet is holding capacity for a job it
could have started.

**"A blanket release breaks ADR 0017's no-jump guarantee."** True, and it is why
clause 2 below is unconditional rather than deleted. ADR 0038 already promoted
that guarantee from a by-product of the fit test to a predicate; this decision
removes the last condition on it.

## Decision

**A reservation withholds ORDER and one repository slot. It never withholds a
vector.**

Two clauses, and both bind:

1. **No capacity charge.** Wherever a reservation is held, the head's vector is
   charged against nobody. `safeBackfill` admits inside the full envelope,
   `chargeReservedHead` models the head as an instance holding no resources, and
   the only thing it occupies is the one repository slot ADR 0030 says the head
   will need when its turn comes. The two capacities remain on different clocks
   and the asymmetry is the point: the vector is what the head cannot use NOW,
   and a head blocked on any axis is waiting for live instances or its own
   repository to release whatever this pass admits — but the slot is what it
   will need THEN, and an admission that takes it keeps it taken past the
   release.

2. **The bound, unconditional.** No demand that could itself take the reserved
   head's vector whole may be admitted **into** it. Work that fits
   `free - reservation` sits BESIDE the head, cannot delay it by a tick, and is
   admitted for the reason the remainder has always admitted work. The rule
   binds only on a candidate that must consume part of the head's own vector,
   and there it is ADR 0017's guarantee verbatim.

   ADR 0038 gated this rule on the head lending, which was correct while
   something else was withheld. Nothing is, so the gate is deleted rather than
   narrowed: `jumpsTheReservedHead` is asked of every candidate, on every axis,
   on every tick a reservation is held.

**And a plan that carries a reservation judges it.** The axis is now derived
from the same function that makes the decision — `feasible` is
`admissionAxis(...) == none` — and any exit of a usable tick that carries a
reservation names the axis holding it, against the same envelope and the same
occupancy `planLinux` would use. `none` is reachable and means what it says: no
axis refuses this head. `unjudged` survives with exactly one meaning left, an
observation the tick could not use.

Everything else is unchanged and still binds work admitted here: the exact CPU,
memory and slot vectors; the four-slot Linux ceiling; repository caps; profile
`MaxActive`; aged-before-young ordering and ADR 0037's tiers; the DRR cursor;
ADR 0030's slack; and ADR 0027's one-admission-per-tick rule. The reservation
contract is preserved as it always has been, by **ordering, not by idleness**:
the head is re-checked first on every later tick, so it wins the first vector
and cap slot large enough for it.

### What the fleet publishes

`fleet doctor`'s reservation check failed on a reservation that did not LEND its
vector. That check cannot survive this record — a reservation lends on every
axis there is, so "does not lend" is not a state the planner reaches, and the
only thing that could still fire it was a plan that published no judgement at
all. It now fails on the `none` axis held longer than the queue SLO: a turn held
for work the fleet could have started, which is issue #125's wedge wearing a
reservation. A single such tick is ordinary — the tick planned the other
platform — so the bound is the one the fleet already carries for "queued work has
waited too long", and no new knob is introduced.

`lendsVector` is deleted from the status document, from
`fleet_reservation_lends_vector`, from the `fleet status` column and from the
`fleet doctor` clause, rather than pinned to true. Deleting the daemon's
re-derivation of it is half the point of this record.

### The oracle models the same rule

`reservedResidual` withheld the head's vector from every other demand whenever
the head fit and was under its cap. Left there it would be NARROWER than the
fleet, and narrowing is the direction that blinds a property — the direction
that hid issue #226 for a month after the 2026-08-09 refinement taught this
oracle to withhold on one axis. It now withholds from exactly one candidate, the
peer that could take the head's vector whole, and lends to everything else. That
widens what the harness reports, which is the direction a harness is allowed to
err in.

## Consequences

A carried reservation costs the fleet nothing but the head's turn. On the mac
studio's tick the 2 CPU are lent to the macOS lane that is actually planning,
and the `linux-2x4` head is admitted by the first tick that plans its own lane —
which is what the ordering rule always promised.

The reserved head's wait can still be extended by at most the runtime of one
backfill wave. That is the trade ADR 0005, ADR 0017, ADR 0029 and ADR 0038 have
each already accepted, and it is deliberately not duration-bounded: GitHub
exposes no safe remaining-runtime contract, and the controller does not terminate
a legitimately busy runner to meet an internal scheduling deadline.

Nothing equal to or larger than the reserved vector can be admitted into it, on
any axis, by rule. Several jobs each smaller than the head can still occupy it
collectively — that is the one backfill wave above, priced and accepted, and not
a hole in clause 2, whose guarantee is per-job exactly as ADR 0017 states it.

`internal/scheduler` is a net deletion: `reservedHeadLendsItsVector`,
`reservedHeadAtRepositoryCap`, `reservationAxis` and two envelope-recomputation
sites are gone, and `judgeCarriedReservation` and `agedLinuxEnvelope` are what
replace them.

Three pins move, and each was built on a head its own comment described as
fitting: the charge arithmetic (`TestChargeReservedHeadIsExact`, renamed), issue
#255's slot test (where the equal-sized `builder` is now refused EARLIER, by
clause 2), and PR #220's oracle pin. That pin has now moved twice — anchored
first on a cap-held head, which is the axis ADR 0038 proved must lend, then on a
head under its cap, and now on the only rule that survives on either. The two
moves are this record's argument in miniature.

**A reservation whose head is admissible is not dropped.** It could be — the
next tick that plans its lane would re-mint it — but dropping it would reset
`Since`, and `Since` is what makes an hour-long hold visible. The reservation is
kept, judged, and published as `none`, which is the state an operator needs to
see rather than one to tidy away.

## Evidence

- `internal/scheduler/scheduler_unjudged_reservation_test.go`: the mac studio's
  tick as a single `PlanTick`. Two assertions pass on the pre-fix tree and
  establish that the reconstruction IS the incident — both envelopes compute
  `{2, 9216, 3}`, the number the node published, and the head is admissible on
  both terms. Three failed: the carried reservation named no axis, its whole
  vector was charged to the macOS remainder pass, and the no-jump rule was off
  in exactly that state.
- `internal/scheduler/scheduler_reserved_cap_test.go`:
  `admissionAxis` in all four of its answers, with `feasible` asserted equal to
  `axis == none` on every row, so the decision and the diagnosis cannot drift
  apart.
- `internal/scheduler/scheduler_reserved_remainder_test.go`: the charge is the
  repository slot and never the vector, in all three of its cases; and clause 2
  binding on the same-repository candidate that used to spend the head's spare
  slot.
- `tests/simulation/oracle_reservation_test.go`,
  `tests/simulation/oracle_repository_cap_test.go`: the lend and its one
  exception asked of the ORACLE, in both directions, on both axes.
- `tests/simulation/properties_test.go`: property (q) — a ready plan that holds
  a reservation names the axis holding its head. It is structural: it compares
  the plan with itself and models nothing.
- **Mutation evidence.** With `judgeCarriedReservation` removed and nothing else
  changed, seed 1 of the `m4-mac-mini` arm fails property (q) at tick 191 — a
  recovery-only tick carrying `b/repo`'s `xl` reservation with no judgement,
  which is the shape the mac studio published for an hour.
- **Reachability**, 40 seeds x 200 ticks per arm: `ready/none` — issue #235's
  own axis — now occurs 2, 11, 10 and 4 times on `m4-mac-mini`,
  `geekom-linux-amd64`, `sequence-reset-linux-large` and
  `tiered-release-priority`. It was unreachable before this change, published as
  an empty axis at the recovery site. The `chargeReservedHead` withhold branch
  this record deletes was reached twice across 240 seed-histories on the
  pre-fix tree, through `fillMacRemainder`.
- `internal/telemetry`, `internal/cli`, `internal/daemon`: the reservation check
  re-founded on the `none` axis and the queue SLO, and `lendsVector` deleted from
  every surface that carried it.

## Not addressed here

This decision does not bound how long a head may hold a reservation. ADR 0036
bounds how long an *instance* may hold its vector; a reservation held for hours
behind a legitimately long job remains legal, and is now at least judged.

It does not change what a macOS-headed tick does with the Linux queue. ADR 0005's
bounded drain wave and `mixedPlatformAdmission` decide that, and the mac studio's
idle cores are a consequence of that arrangement rather than of the reservation
— which is precisely what the old diagnostic could not say and the new one can.

It does not revisit ADR 0030's repository slot as a charge against other demands.
That charge is protective, correct, and the only capacity a reservation still
holds.
