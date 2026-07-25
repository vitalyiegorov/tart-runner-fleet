# ADR 0017: Backfill the residual when a Linux reservation is externally blocked

## Status

Accepted. Amends [ADR 0005](0005-bounded-drain-backfill.md) and the residual
admission rules in [ADR 0012](0012-shared-cross-platform-capacity.md); both
remain accepted everywhere else.

## Context

Aged Linux work leaves throughput optimization and joins absolute global FIFO,
where the head may reserve its exact resource vector. Backfill then admits work
only inside the component-wise remainder, `free - reservation`, so that backfill
can never delay the reserved job once its non-resource blocker clears.

That remainder is computed with a subtraction that fails when the reserved
vector does not fit the current residual at all. The implementation treated the
failure as "no backfill is safe" and admitted nothing.

On a host that also serves an interactive tenant, that state is not rare, it is
the normal steady state. The 2026-07-25 incident on the production Mac mini
(Mac16,10: 10 cores, 24 GiB) made it concrete. A live 7-vCPU macOS builder
inside the shared 8-vCPU/16-GiB envelope left a residual of 1 vCPU / 4096 MiB.
The aged Linux head was a `large` (4 vCPU / 8192 MiB), which cannot fit that
residual, so it reserved and the remainder underflowed. For 37 minutes the
planner emitted zero operations per tick while:

- 12 jobs sat queued and `queue_slo_breached` was reported;
- the daemon's own probe measured 71.44% CPU idle and 11550 MiB available;
- `fleet_host_admission_allowed` was `1`.

Queued work that fit the residual exactly was refused along with everything
else. The decisive observation is that the reserved `large` was blocked by the
macOS builder, not by Linux capacity. No amount of Linux drain could release it,
and it had to wait for that VM to finish regardless. Refusing backfill therefore
bought the reserved job nothing and cost the host every free vCPU it had.

## Decision

When the reserved vector does not fit the residual, the scheduler distinguishes
why before refusing admission. It recomputes the envelope the reservation would
see with every live Linux instance gone, clamped by the fresh host observation:

- If the vector fits that envelope, the reservation is blocked by Linux
  occupancy. It becomes feasible again as soon as that Linux work drains, so the
  strict `free - reservation` remainder is retained unchanged. ADR 0005 applies
  as before.
- If the vector still does not fit, the reservation is *externally blocked*: a
  live macOS cohort holds the shared envelope, or host pressure has shrunk the
  observation below the vector. Only foreign exit or host recovery can release
  it, so exact admission proceeds inside the full residual instead.

A vector that does not fit even the bare configured envelope is deliberately not
treated as externally blocked. That is a misconfigured profile rather than
contention, and it keeps the strict remainder.

Every other constraint is unchanged and still applies to backfilled work: exact
CPU, memory, and slot vectors; the four-slot Linux ceiling; repository
concurrency caps; profile `MaxActive`; aged-before-young ordering; the
deterministic round-robin cursor; and fresh-observation and ownership gates.
Backfill never drains and never emits a spawn on a non-ready plan.

The relaxation needs no durable one-shot budget because the predicate is
self-limiting. The moment the foreign instance exits, the reservation is no
longer externally blocked, the strict remainder governs again, and no newly
arrived work can be admitted ahead of the reserved job.

## Consequences

A bounded host keeps working for its own tenant instead of stranding every free
vCPU behind one infeasible reservation. Admission stays inside the exact
measured residual, so combined consumption cannot exceed the envelope, the host
observation, or the slot ceiling.

The reserved job's wait is not extended by the relaxation in the blocking case,
because it is already waiting on foreign occupancy. It can be extended by at
most the runtime of one backfill wave in the narrow window where the foreign
instance exits while backfilled work is still live and holding capacity the
reservation needs. That is the same trade ADR 0005 already accepted, and it is
not described as duration-bounded: GitHub exposes no safe remaining-runtime
contract, and the controller does not terminate a legitimately busy runner to
meet an internal scheduling deadline.

Because anything admitted in this path is by construction too small to fit the
reserved vector's residual, no equal-or-larger job can jump the queue.

## Evidence

- `internal/scheduler`: unit coverage of the blocking and non-blocking cases,
  the residual bound, the misconfiguration guard, and the over-subscribed
  foreign cohort.
- `tests/replay`: a multi-tick replay of the 2026-07-25 incident proving the
  residual is put to work and the reservation is admitted immediately once the
  builder exits.

## Not addressed here

This decision repairs stranded residual admission inside a given envelope. It
does not make the envelope itself elastic; that is
[ADR 0018](0018-second-pilot-elastic-host-envelope.md), which sizes the envelope
from the observed physical host and its measured idle CPU. The two compose: 0018
decides how wide the envelope is, and this decision decides that a reservation
which cannot fit it must not strand what remains.
