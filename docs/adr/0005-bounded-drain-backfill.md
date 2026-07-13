# ADR 0005: Cardinality-bounded Linux backfill during macOS handoff

## Status

Accepted.

## Context

A running Linux job can prevent an already-selected macOS handoff while leaving
enough host capacity for a small, old required gate. Refusing all admission in
that state wastes the residual envelope and can block both queues. Admitting
ordinary Linux work on every scheduler tick is also unsafe: repeated arrivals
could continuously move the macOS handoff behind new work.

The controller does not have a trustworthy remaining-runtime estimate for a
GitHub Actions job and does not terminate a legitimate busy runner merely to
meet an internal scheduling deadline. It therefore must not claim a temporal
bound it cannot enforce.

## Decision

While a non-idle Linux instance prevents a selected macOS demand from starting,
the scheduler may admit exactly one additional demand for that handoff. The
demand must:

- already satisfy the global aging threshold;
- use the smallest configured Linux resource vector;
- fit both the configured Linux envelope and the freshly observed host vector;
- satisfy the repository concurrency cap.

The selected macOS demand, handoff start time, and consumed one-shot budget are
durable scheduler state. Repeated ticks and newly arriving Linux work cannot
reset that budget. Replacing or cancelling the selected macOS demand creates a
new handoff. Once no Linux instance remains, the scheduler clears the handoff
state and may spawn macOS; it never emits Linux and macOS spawns in one plan.

## Consequences

Residual capacity can unblock one aged small gate without creating an unbounded
backfill stream. Admission is deterministically bounded by cardinality and by
the host resource vector. It is not described as duration-bounded: the admitted
job can outlive the original drain holder because GitHub does not expose a safe
remaining-runtime contract here. A future duration-aware policy requires an
explicit, enforceable job-runtime contract and separate replay evidence.
