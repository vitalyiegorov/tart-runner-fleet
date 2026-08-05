# ADR 0004: Bounded control-plane scheduling priority

## Status

Accepted.

## Context

Fleet controller CI repairs defects that can block every managed repository.
Treating those jobs exactly like application jobs extends an incident, while an
unbounded numeric priority could starve application delivery and make policy
hard to audit.

## Decision

The scheduler has two explicit classes: `control-plane` and `standard`.
Omitted configuration is normalized to `standard`; all other values are
rejected.

Scheduling order is:

1. aged work in global FIFO order,
2. young control-plane work,
3. young standard work.

Each young lane retains deterministic per-repository round robin and the
durable cursor. The ordering is shared by platform arbitration, Linux exact
admission and safe backfill, macOS profile arbitration, and macOS admission.

The production example assigns `vitalyiegorov/tart-runner-fleet` to the
control-plane class. Repository caps and resource vectors still bound its
concurrency.

### Amendment 2026-08-03: the order is exact admission's band vector

The list above is a total order over demands, and every pass that orders demands
honoured it. `exactSelect` does not order demands — it chooses a SET — and it
ranked candidate sets by a band vector of its own: control-plane first, standard
second, aging nowhere. Aging was left to the reservation head outside it, which
protects ONE demand, while `safeBackfill` and the bounded drain backfill hand
`exactSelect` aged and young candidates in the same slice. Band coverage is
compared before anything else, so a young control-plane demand took the residual
ahead of an aged standard one: rule 2 ahead of rule 1. Deterministic simulation
found it on 23 of 150 seeds (ADR 0031, finding 3).

The bands ARE this record's list, in this record's order: band 0 aged, band 1
young control-plane, band 2 young standard. A candidate list and its band vector
now state the same rule, and "aged work in global FIFO order" is again absolute
over both young lanes wherever admission is decided.

The aged band is class-blind, as global FIFO requires: an aged control-plane
demand and an aged standard demand share band 0 and are separated by their
position in the queue, not by their class.

A band is also compared by its MEMBERS, not merely by how many demands it
covers. Counting alone ties whenever two admissions serve the same NUMBER of
aged demands, and the tie was then settled by the young control-plane band —
which decides WHICH aged demand is served, and that is rule 1's question. In the
simulated counterexample an aged `large` heading the residual lost it to an aged
`small` plus a one-minute-old control-plane `medium`. Each band is now settled in
priority order, more admissions first and then the earlier queue positions, so
rule 1 is complete before rule 2 is read. Where every candidate shares one band
this is the previous count-then-position preference exactly, so throughput and
determinism are unchanged.

Admission stays work-conserving: the order decides who is served first, never
that capacity is held idle. Once the aged band has taken the vectors it can use,
the young lanes still take whatever is left.

### Amendment 2026-08-05: a reservation is not a priority over older work

Rule 1 is a FIFO, so it binds the reservation mechanism as much as it binds
`exactSelect`. A reservation is made for the oldest aged demand that does not
fit, and [ADR 0017](0017-infeasible-reservation-residual-backfill.md) and
[ADR 0029](0029-remainder-admission-behind-a-reservation.md) both promise it is
re-checked first on every later tick so it wins the first vector large enough for
it. That promise was written about work that is YOUNGER than the head, which is
the only work those records contemplate arriving while it waits.

The plannable queue is not frozen while a reservation is held.
[ADR 0027](0027-one-tick-admits-a-demand-once.md)'s queue seam keeps a
demand out of it for exactly as long as a live instance incarnates it, and a
recovery drain puts that demand back — carrying its GitHub queue time, which is
what `demandAged` and `demandLess` measure from. So the demand that re-enters the
queue can be OLDER than the reserved head, and `planLinux` returned straight out
of its reservation branch without consulting the queue it had just been handed,
although `priorityOrder` had already ranked the returning demand first. Rule 2
was never involved: a younger demand of the same class, repository, profile and
size took the vector purely because it held the reservation.

Deterministic simulation found it as a cycle rather than an unlucky tick (ADR
0031; issue #208, seed 55 of the container-node arm): an assignment stalls, is
recovered fifteen minutes later, releases the oldest demand on the very tick its
vector frees, and loses it to the standing reservation — four times over one run,
fifty ticks apart.

A held reservation is therefore re-derived rather than obeyed whenever an aged
demand older than it is plannable. The aged loop then admits the true head and
reserves whatever it cannot fit, so the demand that lost the reservation is first
in line behind the older work that displaced it, and nothing younger gains
anything either way. The protection ADR 0017 and ADR 0029 describe is unchanged
in the case they describe: younger work still may not take the head's turn.

## Consequences

Fleet repairs receive the next available compatible quantum while young.
Standard work cannot starve: at `linuxReservationAgeSeconds` it moves ahead of
all young control-plane work under the existing reservation and global FIFO
rules. Linux/macOS mutual exclusion, exact CPU/memory/slot envelopes,
determinism, and observe/shadow authority are unchanged.
