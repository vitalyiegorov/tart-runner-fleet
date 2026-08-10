# ADR 0038: A reserved head its repository cap holds lends its vector

## Status

Accepted. Extends
[ADR 0017](0017-infeasible-reservation-residual-backfill.md) from the vector
axis to the repository-cap axis, and carries that extension to the complementary
passes of [ADR 0029](0029-remainder-admission-behind-a-reservation.md) and
[ADR 0030](0030-a-reserved-head-holds-one-repository-slot.md). Everything those
records decide remains accepted: the vector condition, ADR 0030's slot count,
the aged-FIFO and scheduling-class rules of
[ADR 0004](0004-bounded-control-plane-priority.md), the repository caps of
[ADR 0012](0012-shared-cross-platform-capacity.md), and the
one-admission-per-tick rule of
[ADR 0027](0027-one-tick-admits-a-demand-once.md).

Also amends the feasibility oracle of
[ADR 0031](0031-deterministic-simulation-testing.md), whose 2026-08-09
refinement modelled one of the two axes.

## Context

Aged Linux work joins absolute global FIFO, where the head may reserve its exact
resource vector. `scheduler.feasible` decides whether that head can be admitted
from **two** terms:

```go
func feasible(resources, free domain.Resources, repo string, base map[string]int, selected []domain.Demand, caps map[string]int) bool {
	if !free.CanFit(resources) { return false }          // vector
	...
	return count < repoCapLimit(caps, repo)              // repository cap
}
```

`planLinux` holds the reservation when that boolean is false — for **either**
reason. ADR 0017 gave the first term a release rule after the 2026-07-25
incident: when the reserved vector does not fit the free envelope at all,
admission proceeds inside the full residual, because

> a head that does not fit cannot start until live instances release the
> resources it needs, whatever backfill does. It is not waiting on backfill to
> stop.

The second term never got that rule. Its decision text is stated on the vector
because the vector was the only axis in the predicate when ADR 0017 was written,
and neither ADR 0029 nor ADR 0030 revisited it when they introduced the cap axis
— both used the cap only in the protective direction, as a bound on which
repository may bid. **No record ever decided that a head its repository cap
holds keeps its vector.** It is an unexamined consequence of a predicate that
predates the axis.

The rationale transfers word for word. A head at its repository's cap cannot
start until one of that repository's own instances exits, whatever backfill
does. It is not waiting on backfill to stop either. No amount of freed CPU can
admit it, so withholding CPU for it cannot be justified by "the pass must not
delay the head", which is the entire warrant ADR 0029 condition 1 rests on.

### The state, and its cost

Issue #226, reduced to a single tick:

```
next reservation=&{c/repo/1009/1/500009 xl {6 12288 1} ...}
  demand   a/repo/1010/1/500010 profile=large age=9m30s
  demand   c/repo/1009/1/500009 profile=xl    age=13m0s
  instance trf-large-…  repo=c/repo profile=large  state=assigned
  instance trf-medium-… repo=c/repo profile=medium state=running
host available={CPU:6 MemoryMB:18432 Slots:4}
```

`c/repo` is at its cap of two. The head is **resource-feasible and
cap-infeasible**. `safeBackfill` subtracted its whole `{6, 12288, 1}` from a
`{6, 18432, 2}` envelope and offered `a/repo`'s `large` — from a repository with
no live instance at all — the `{0, 6144, 1}` that was left. The cost has the
units ADR 0029 states: an idle vector the size of the starved profile, for the
entire duration of the blocking job.

Production reachability is plain rather than exotic. Configured caps are 3–4 per
target and an **unconfigured repository caps at 1**, so the state needs only a
repository at its cap whose live instances are small enough that an aged head
from it still fits free.

### This is the whole remainder path, not a corner of it

The head is judged in the starvation envelope (`linuxFreeAged`) and backfill
plans in the throttled one (`linuxFree`), and the first always contains the
second — they differ in exactly one term, the advisory CPU-idle clamp, which
`aged` lifts. So a head refused on the **vector** can never satisfy
`free.Sub(reservation.Resources)` in `safeBackfill`, and the withheld branch was
reachable **only** for a cap-held head.

Verified as well as argued: a panic probe on that branch is not reached once
across `internal/scheduler`, `tests/replay`, or 60 seeds × 200 ticks × 3
simulation arms. `chargeReservedHead`'s charge branch is the same — reached only
by its own direct unit test, never through `PlanTick`.

### The harness could not see it, and that is the more serious half

`feasibleDemands` — the independent oracle properties (a) and (b) rest on — read
the same two facts and used one of them. `reservedResidual` decided "the head
fits" from `free.Sub(profile.Resources)` alone and never read `RepoCaps`, so it
withheld six vCPU on behalf of a head that `feasibleDemands` itself drops four
lines later on `repos[...] >= repoCap(...)`. Property (a) reported nothing, on
every tick, for as long as the cap holder ran.

That blindness was **introduced by a fix**. The 2026-08-09 refinement (issue
#216) taught the oracle that a reservation withholds a vector, which is correct
on the vector axis and is why the refinement stands. But the tick it was
anchored to — seed 92 of the container-node arm — is itself a **cap-held** head,
so property (a)'s original report there was a **true positive**, and the
refinement taught the harness to disbelieve it. Its own pin,
`TestFeasibilityWithholdsAFittingReservedHeadsVector`, was built from a head
held out by `ops/fleet`'s cap of two, and the PR body said so.

The finding the harness then lost, finding 7, was retired as *"retired pending
#226"* on the honest observation that no seed reproduced it any more. The
suspicion recorded with that retirement was right: the wedge never went away,
the oracle stopped being able to see it.

The direction-of-safety argument is what inverted, and it is the part worth
keeping. ADR 0029 says:

> Judging a head as fitting is the fail-safe answer: it withholds the head's
> vector.

That is fail-safe **for the scheduler**, where withholding protects the head. In
an **oracle**, withholding is the unsafe direction: it suppresses reports. A
blind oracle does not merely permit a defect — it certifies the fleet as correct
while the defect runs in production.

### Withholding is not worthless, and a blanket release is wrong

Two counterweights, both measured, because the obvious fix is not the right one.

**Withholding buys a conditional protection.** On this topology, with the
`a/repo` `large` hypothetically backfilled: if `c/repo`'s `large` exits, the head
starts immediately; if `c/repo`'s `medium` exits instead, the head waits for the
backfill. That is real. What is also true is that it is the *identical* trade
ADR 0017 already made on the vector axis, twice, after two production incidents:
a certain and unbounded cost against a conditional benefit bounded by one
backfill wave.

**A blanket release breaks a guarantee ADR 0017 states.** Its Consequences
promise:

> Because anything admitted in this path is by construction too small to fit the
> reserved vector, no equal-or-larger job can jump the queue.

"By construction" is exact, and the construction is the **fit test**: a head that
does not fit `free` overflows it in some dimension, so any candidate bounded by
`free` is strictly smaller there and cannot contain the head's vector. A
cap-held head **fits** `free`. The moment it lends, the construction ends, and an
equal-or-larger peer could take exactly the vector the oldest aged demand is
entitled to and invert the aged FIFO the reservation exists to protect.

## Decision

**A reserved head its own repository cap holds out lends the vector it cannot
use — to work it still outranks, and to nothing else.**

Two clauses, and both bind:

1. **The lend.** Wherever a reservation is held, the head's vector is charged
   against the pass's envelope only while the head can still use it. A head held
   out by its repository cap charges nothing, exactly as ADR 0017's
   vector-infeasible head charges nothing, and admission proceeds in the full
   envelope. The occupancy the cap is read against is the same one `feasible`
   will read on the tick the cap slot frees — live instances plus everything
   this plan already admits — so the two predicates can never disagree about
   which axis is holding the head.

2. **The bound, stated rather than inherited.** No demand that could itself take
   the reserved head's vector whole may be admitted **into** it. This is
   ADR 0017's no-jump guarantee promoted from a by-product of the fit test to a
   predicate, applied on **both** axes: provably a no-op on the vector axis,
   load-bearing on the cap axis.

   "Into it" is exact, and it is the whole of clause 2. Work that fits
   `free - reservation` sits **beside** the head and cannot delay it by a tick,
   so no jump is possible and the rule does not apply — such work is admitted
   for the reason the remainder has always admitted work. The rule binds only on
   a candidate that must consume part of the head's own vector.

Keeping clause 2 on the axis where it cannot bind is deliberate. The guarantee
is now stated in one place and tested directly, instead of being re-derived from
arithmetic every time somebody changes what a reservation lends — which is
precisely the failure this whole issue is an instance of.

The narrower reading of clause 2 — refusing every vector-taking candidate,
including those that fit beside the head — was written first, and the
deterministic simulator refused it: seed 104 of the container-node arm reported
a wedge at tick 117 with a `small` head reserved and a feasible `medium`
refused. That reading would have reintroduced the sterilization this decision
exists to end, inside the fix for it. It is recorded here because the correction
came from the harness rather than from review, which is the arrangement
ADR 0031 exists to produce.

Everything else is unchanged and still binds work admitted here: the exact CPU,
memory and slot vectors; the four-slot Linux ceiling; repository caps; profile
`MaxActive`; aged-before-young ordering and ADR 0037's tiers; the DRR cursor;
ADR 0030's slack (a cap-held head has `slack <= 0` by definition, so nothing
from its own repository may bid — still exactly right); and ADR 0027's rule that
one tick admits a demand once. The reservation contract is preserved as it
always has been, by **ordering, not by idleness**: the head is re-checked first
on every later tick, so it wins the first cap slot and vector large enough for
it.

### The oracle models both axes

`reservedResidual` reads the cap term too. A demand other than the head is
judged against `free - reservation` when the head is withheld, and against the
full `free` when the head is cap-held **unless** that demand could take the
head's vector whole. Making the head's own feasibility predicate stricter makes
`holds` bind less often, which **widens** what the oracle reports — the direction
a harness is allowed to err in, by the merged PR's own argument.

Independence (ADR 0031) holds: the added term reads observed instance occupancy
and the configured cap through `repoCap`, the same pair of physical and
configured facts `feasibleDemands` already counts for every other demand. No
scheduler envelope computation reaches it.

### The reservation is published

Every tick now publishes which reservation is held, for which repository, and
**which axis is holding it** — `vector`, `repository_cap`, `both`, `none`, or
unjudged — through `fleet status`, a `reservation` check in `fleet doctor`, and
the `fleet_reservation_*` metrics. The axis is a closed vocabulary because it is
a metric label; the head's demand key and repository travel in the status
document only, never as labels.

This is not decoration. Issue #226 shipped, was reachable, and left **no
artifact**: `grep reservation` over the authority log returns nothing at all,
because there was nothing to find. A defect that strands a vector for the whole
runtime of a blocking job was invisible to the fleet running it and was found
only by a deterministic simulator. A `repository_cap` axis held for forty minutes
is now a thing an operator can see and an alert can fire on.

## Consequences

A bounded host keeps working instead of stranding a vector behind a head that
cannot use it. On issue #226's tick `a/repo`'s `large` is admitted immediately
instead of waiting for the cap holder to finish.

The reserved head's wait can be extended by at most the runtime of one backfill
wave, in the window where its cap slot frees while backfilled work still holds
capacity it needs. That is the trade ADR 0005, ADR 0017 and ADR 0029 have each
already accepted, and it is deliberately not duration-bounded: GitHub exposes no
safe remaining-runtime contract, and the controller does not terminate a
legitimately busy runner to meet an internal scheduling deadline.

Nothing equal to or larger than the reserved vector can be admitted into it, on
either axis, by rule. Several jobs each smaller than the head can still occupy
it collectively — that is the "one backfill wave" cost above, priced and
accepted, not a hole in clause 2, whose guarantee is per-job exactly as ADR
0017 states it. And a job larger than the head is still admitted freely when it
fits beside it, because that job takes nothing the head is entitled to.

Because the withheld branch of `safeBackfill` was only ever reachable for a
cap-held head, this decision changes that path's behaviour wherever a cap-held
reservation occurs and nowhere else. Three existing pins move for that reason,
and each was built on a head its own comment already described as cap-held.

Finding 7 is fixed rather than tolerated. Its signature is kept so a regression
is reported by name, and it is deliberately absent from `knownFinding`.

## Evidence

- `internal/scheduler/scheduler_reserved_cap_test.go`: issue #226's tick as a
  single `PlanTick`, with the prior reservation carried in and with none at all;
  the no-jump rule held directly, with the smaller demand admitted on the same
  tick so the refusal cannot pass on an empty envelope; the counterweight, a
  demand LARGER than the head admitted because it fits beside it;
  `takesTheReservedVector` and `reservedHeadAtRepositoryCap` pinned as
  predicates, including the proof that the cap predicate and `feasible` cannot
  disagree; and the published axis in all four of its values.
- `tests/simulation/oracle_repository_cap_test.go`: the same tick asked of the
  ORACLE, both directions — the vector is lent to work the head outranks and
  withheld from a peer that could take it whole — plus the arithmetic showing
  #216's and #226's states are one shape.
- `tests/simulation/oracle_reservation_test.go`:
  `TestFeasibilityWithholdsAFittingReservedHeadsVector` re-anchored to a head
  under its cap, asserting that anchor directly so it cannot drift back onto the
  cap axis.
- `tests/simulation/findings_test.go`: finding 7 as a regression — the admission
  through `PlanTick`, and its own seed 67 reporting nothing at all.
- **Mutation evidence.** With this decision's cap term removed from
  `safeBackfill` and nothing else changed, seed 67 of the container-node arm
  reports `tick 54: liveness_wedge (reserved_head_held_by_a_repository_cap)` —
  the tick issue #226 names, under the restored signature, failing the sweep
  because that signature is no longer tolerated.
- `internal/scheduler/scheduler_reserved_remainder_test.go`,
  `internal/scheduler/scheduler_mixed_test.go`: the complementary passes under
  both clauses, including a `builder` equal to an `xl` head that must still be
  refused.
- `internal/telemetry`, `internal/cli`, `internal/daemon`: the reservation and
  its axis through the status document, `fleet doctor`, and the metrics page,
  with the label vocabulary closed.

## Not addressed here

The residual is still arbitrated one platform at a time — `planLinux` plans
before `fillMacRemainder` — so a young Linux demand can take a residual an older
macOS demand would have won under a single global ordering. ADR 0030 recorded
that and it is unchanged.

This decision does not bound how long a head may hold a reservation. ADR 0036
bounds how long an *instance* may hold its vector, which is the other half of
the same fleet-level question; a reservation held for hours behind a legitimately
long job remains legal, and is now at least visible.

Nor does it revisit ADR 0030's repository slot as a charge against other
demands. That charge is protective and correct; the term this decision adds is
the head's **own** feasibility predicate, which is the opposite direction.
