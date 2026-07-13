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

## Consequences

Fleet repairs receive the next available compatible quantum while young.
Standard work cannot starve: at `linuxReservationAgeSeconds` it moves ahead of
all young control-plane work under the existing reservation and global FIFO
rules. Linux/macOS mutual exclusion, exact CPU/memory/slot envelopes,
determinism, and observe/shadow authority are unchanged.
