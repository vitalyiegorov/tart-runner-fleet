# ADR 0029: Remainder admission behind a held reservation

## Status

Accepted. Extends [ADR 0017](0017-infeasible-reservation-residual-backfill.md)
from `safeBackfill` to the complementary remainder passes, and amends the
residual admission rules in
[ADR 0012](0012-shared-cross-platform-capacity.md); both remain accepted
everywhere else. The aged-FIFO and scheduling-class rules of
[ADR 0004](0004-bounded-control-plane-priority.md) and the one-admission-per-tick
rule of [ADR 0027](0027-one-tick-admits-a-demand-once.md) are unchanged and still
bind every demand admitted here.

## Context

Aged work joins absolute global FIFO, where the head may reserve its exact
resource vector so that nothing younger can take it. ADR 0017 settled what
backfill may do underneath that reservation *on the Linux queue*: admit inside
the component-wise remainder `free - reservation`, except when the reserved
vector does not fit the free envelope at all, in which case admit inside the
full residual — a head that does not fit is blocked by live instances holding
what it needs, so it cannot start until they release no matter what backfill
does. It is not waiting on backfill to stop.

`fillMacRemainder` — the second, complementary pass that admits macOS work in
the envelope a Linux tick leaves over — never received that distinction. It
returned early on `plan.Next.Reservation != nil`, treating every reservation as
a veto, and PR #112 reported the asymmetry and deferred it.

It reached production on 2026-08-02 at ~13:50Z on the Mac mini (Mac16,10: 10
cores, 24 GiB):

- a live `xl` Linux VM (6 CPU / 12288 MB) had been busy for over an hour;
- the aged global-FIFO head was a second `xl`, which cannot fit beside it
  (6 + 6 > 10), so `planLinux` correctly held its reservation;
- a queued macOS `maestro` (4 CPU / 7168 MB) fits the four free cores exactly;
- it was refused every tick for 60+ minutes, and the queue breached its SLO
  while a quarter of the machine sat idle.

Nothing was protected by that refusal. The reserved `xl` could not have used
those four cores; it was waiting for the busy `xl` to exit, not for the maestro
to stay unadmitted.

### What the bug class costs

The cost of a wrongly vetoed remainder is not a scheduling nicety, and it is
worth stating in units so the invariant's motivation survives this incident: an
idle vector the size of the starved profile, for the entire duration of the
blocking job.

Here that is 4 cores and 7168 MB idle for 78 minutes (12:47Z→14:05Z) — 5.2
core-hours, 40% of a ten-core machine, on a host whose own probe reported 58%
CPU idle and `admissionAllowed: true` throughout. Three `maestro` jobs each paid
1h18m of queue latency for capacity that was standing free the whole time. The
loss scales with the blocking job's runtime, which the controller neither bounds
nor can predict, so a single long CI job converts one wrong predicate into an
arbitrarily large hole.

The 2026-07-25 incident ADR 0017 repaired had the same shape and a comparable
size (3 vCPU / 4096 MB idle for ~45 minutes). Two incidents of one shape in nine
days is why this decision states the rule as an invariant over *every* pass
rather than patching a second call site.

A survey of every pass that can admit while a reservation is held found one more
of the same shape, in the opposite direction: `planLinuxHandoff`'s one-shot macOS
backfill wave consulted no reservation at all and could therefore admit a macOS
VM straight into the vector reserved for the aged Linux head it exists to
unblock. Bounded is not the same as safe — one wave is enough to make a head
wait for a whole job.

`fillLinuxRemainder` does not share the flaw: it delegates to `planLinux`, which
owns the reservation and already applies ADR 0017. `boundedDrainBackfill` is
left as it is: it admits at most one VM of the smallest Linux profile, latched
once per handoff by ADR 0005 precisely so it cannot starve what waits behind it,
and the reserved head — always aged — is itself its first-ranked candidate when
its profile is eligible.

## Decision

Every pass that admits work while another pass holds a reservation — the macOS
remainder pass and the bounded handoff wave alike — admits it only when doing so
cannot delay the reserved head. Two conditions together are the invariant:

1. **Vector.** The pass plans inside `free - reservation`: the reserved head's
   whole vector is charged against the pass's envelope as if it were already
   live, exactly as this tick's planned spawns are charged. When the reserved
   vector does not fit the free envelope at all, nothing is charged and the pass
   plans inside the full residual — ADR 0017's rule, for the same reason.
2. **Repository.** The pass never bids for the reserved head's repository.
   Resources are not the only way to delay a head: repository caps are counted
   over every live instance regardless of platform, so a macOS spawn in the
   head's repository can consume the exact cap slot the head is waiting for and
   block it again the moment its vector frees. Remainder arithmetic cannot see
   that blocker, so the repository is excluded outright.

Feasibility of the reserved head is judged against the starvation-guard
envelope, the same one `planLinux` judges it in, because only an aged head ever
holds a reservation. Judging a head as fitting is the fail-safe answer: it
withholds the head's vector.

As in ADR 0017, the reservation contract in the infeasible case is preserved by
**ordering, not by idleness**. The reserved head is re-checked first on every
later tick, so it wins the first vector large enough for it. The remainder pass
may use stranded capacity; it may never take the head's turn.

Every other constraint is unchanged and still applies to work admitted here: the
shared host envelope and fresh host observation, profile `MaxActive`, the
single-cohort rule, the four-slot ceiling, aged-before-young ordering, the DRR
cursor (which the Linux head keeps), the drain guard that suspends both
remainder passes while any drain is in flight, and ADR 0027's rule that one tick
admits a demand once.

## Consequences

A bounded host keeps working instead of stranding free cores behind a
reservation that cannot use them. In the incident topology the maestro is
admitted on the first tick instead of never.

While the reserved head fits the envelope, this pass is *strictly more*
constrained than the early return it replaces in one respect and equally
constrained in the other: it may admit only what is left after the head's whole
vector, and only outside the head's repository. So the change cannot introduce a
delay in the case the early return was protecting.

In the infeasible case the head's wait can be extended by at most the runtime of
one admitted wave, in the window where the blocking instance releases while that
work is still live. That is the trade ADR 0005 and ADR 0017 already accepted and
it is deliberately not duration-bounded: GitHub exposes no safe
remaining-runtime contract, and the controller does not terminate a legitimately
busy runner to meet an internal scheduling deadline.

## Evidence

- `tests/replay/reserved_remainder_incident_test.go`: the incident itself over
  16 ticks of the 78-minute window — a control arm with no complementary pass
  reproducing the hole, then the free vector put to work exactly once, the
  reservation surviving every tick with no drain planned, and the reserved head
  taking the released vector FIRST at 14:05Z ahead of the maestros still queued.
- `internal/scheduler/scheduler_reserved_remainder_test.go`: the unit replay
  (infeasible `xl` head, busy `xl`, feasible maestro admitted), both guard cases
  (a feasible head keeps its whole vector withheld; a feasible head still lends
  what it does not need), the counterfactual proving the refusal is what lets the
  head start, the head winning the vector on the next tick after a fill, the
  repository-slot exclusion, the bounded handoff wave under both directions of
  the invariant, the `fillLinuxRemainder` symmetry check, and the envelope
  arithmetic pinned directly.
- `internal/scheduler/scheduler_mixed_test.go`:
  `TestMixedFillMacSkippedWhileReservationHeld` still refuses macOS admission
  into a reserved remainder that cannot hold it.
- `internal/scheduler/scheduler_duplicate_admission_test.go`: ADR 0027's
  property — one tick admits a demand once — still holds through both remainder
  passes, which now filter twice (already-spawned, then reserved repository).
- `internal/scheduler/simulation_test.go`: the drain-safety, capacity,
  macOS-cohort and host-bound oracles run on every tick of the 200-tick
  liveness property.

## Not addressed here

macOS admission still has no repository-cap accounting of its own; this decision
only guarantees that a macOS spawn cannot take a *reserved head's* cap slot. It
does not change the envelope itself — that is
[ADR 0018](0018-second-pilot-elastic-host-envelope.md) — and it does not give
macOS demands their own reservation: `State.Reservation` remains singular and
Linux-authored.
