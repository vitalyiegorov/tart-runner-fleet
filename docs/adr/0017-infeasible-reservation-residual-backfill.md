# ADR 0017: Backfill the residual behind an infeasible reservation

## Status

Accepted. Amends [ADR 0005](0005-bounded-drain-backfill.md) and the residual
admission rules in [ADR 0012](0012-shared-cross-platform-capacity.md); both
remain accepted everywhere else. Extended by
[ADR 0029](0029-remainder-admission-behind-a-reservation.md), which carries this
rule from `safeBackfill` to the complementary remainder passes and states the
non-resource half of the invariant.

## Context

Aged Linux work leaves throughput optimization and joins absolute global FIFO,
where the head may reserve its exact resource vector. Backfill then admits work
only inside the component-wise remainder, `free - reservation`, so that backfill
can never delay the reserved job once its non-resource blocker clears.

That remainder is computed with a subtraction that fails when the reserved
vector does not fit the current residual at all. The implementation treated the
failure as "no backfill is safe" and admitted nothing. Holding the residual idle
in that state protects nothing: a head that does not fit cannot start until live
instances release the resources it needs, whatever backfill does. It is not
waiting on backfill to stop.

The 2026-07-25 incident on the production Mac mini (Mac16,10: 10 cores, 24 GiB)
made this concrete, and its forensics matter because the occupancy was itself a
bug. A macOS builder was wedged in `Draining`: the fleet issued an event drain
after the demand it was spawned for reached `JobCompleted`, but GitHub had
brokered the runner a *different* job and refused to deregister a busy runner.
The deregister retried 60+ times while the instance kept consuming
7 vCPU / 12288 MiB. With `maxLinuxCpu` raised to 10 that left 3 vCPU / 4096 MiB
free -- room for a `medium` -- and:

- five medium and five large jobs sat queued for roughly 45 minutes;
- the daemon was ready and ticking, with `queue_slo_breached`;
- its own probe reported 62-77% CPU idle and `fleet_host_admission_allowed 1`.

The aged FIFO head was a `large` (4 vCPU / 8192 MiB) that did not fit the
residual, so it reserved and the remainder underflowed. Queued work that fit the
residual exactly was refused along with everything else, every tick.

This is the same distinction already drawn for a resource-infeasible *macOS*
head -- "nothing is live to drain and make room for it, so it is NOT waiting on
drainable work" -- left unhandled for the Linux head.

## Decision

When the reserved vector does not fit the free envelope, exact admission
proceeds inside the full residual instead of the empty remainder.

The reservation contract is preserved by **ordering, not by idleness**. The
reserved head is re-checked first on every later tick, in `planLinux`'s
`reservedDemand` branch, so it wins the first vector large enough for it.
Backfill may use stranded capacity; it may never take the head's turn.

Every other constraint is unchanged and still applies to backfilled work: exact
CPU, memory, and slot vectors; the four-slot Linux ceiling; repository
concurrency caps; profile `MaxActive`; aged-before-young ordering; the
deterministic round-robin cursor; and fresh-observation and ownership gates.
Backfill never drains and never emits a spawn on a non-ready plan.

An earlier draft of this decision gated the relaxation on *why* the reservation
did not fit, keeping the strict remainder when the blocker was Linux occupancy
rather than a foreign cohort or host pressure. That distinction was dropped. A
head blocked by live Linux work is equally unable to start until that work
releases, so the extra branch bought no protection the ordering rule does not
already provide, and it cost a predicate plus its own edge cases.

## Consequences

A bounded host keeps working instead of stranding every free vCPU behind one
infeasible reservation. Admission stays inside the exact measured residual, so
combined consumption cannot exceed the envelope, the host observation, or the
slot ceiling.

The reserved job's wait can be extended by at most the runtime of one backfill
wave, in the window where the blocking instance releases while backfilled work
is still live and holding capacity the reservation needs. That is the trade
ADR 0005 already accepted, and it is not described as duration-bounded: GitHub
exposes no safe remaining-runtime contract, and the controller does not
terminate a legitimately busy runner to meet an internal scheduling deadline.

Because anything admitted in this path is by construction too small to fit the
reserved vector, no equal-or-larger job can jump the queue.

## Evidence

- `internal/scheduler`: the infeasible-Linux-head admission cases, and the
  reserved head winning the vector once the blocker clears.
- `internal/scheduler/simulation_test.go`: the wedged-drain model -- an instance
  held in `Draining` forever while still consuming the host -- plus a 200-tick
  liveness property asserting the feasible residual keeps being admitted and the
  reserved head wins the vector when the wedge clears. Every drain-safety,
  capacity, macOS-cohort, and go-forward host-bound oracle runs on every tick,
  so liveness is not bought by overcommitting the host or draining a busy VM.
- `tests/replay`: a three-tick replay proving the residual is put to work and
  the reservation is admitted immediately once the blocking instance exits.

## Not addressed here

This decision repairs stranded residual admission inside a given envelope. It
does not make the envelope itself elastic; that is
[ADR 0018](0018-second-pilot-elastic-host-envelope.md), which sizes the envelope
from the observed physical host and its measured idle CPU. The two compose: 0018
decides how wide the envelope is, and this decision decides that a reservation
which cannot fit it must not strand what remains.

The premature event drain that wedged the builder is a lifecycle defect, not a
scheduling one, and is repaired separately by aborting drains whose runner
GitHub reports busy.
