# ADR 0064: Audited foreign selectors do not poison local inventory

## Status

Accepted for issue #375. Operational deployment remains subject to normal gates.

## Context

On 2026-10-16 MacBook was running CI but its Budgie REST observation reported
`queue_reconcile_failed`. Queued job 116848493289 explicitly requested
`trf-budgie-builder-2`, the Mini's builder set name. MacBook's builder set was
`trf-budgie-builder-2-macbook`. Strict inventory correctly refused to execute the
job locally, but also rejected the entire scope snapshot, including valid local
jobs. The existing foreign architecture proof cannot describe two sets on the
same platform. This is a fleet defect; the previous inventory world omitted
same-platform, explicitly selected peers (a world-model blind spot).

## Decision

Reuse the existing completed scale-set audit. A job with no local binding may be
excluded from this node's inventory only when it explicitly requests a set name
that the audit reports as parked in the same configured scope. The set ID must
be positive and absent from every local binding in that scope; its name must not
be a label advertised by a local binding. A binding in that scope must accept
the job's repository. Names and scope matching follow existing case-insensitive
routing rules. No naming prefix or suffix is evidence.

Both the completed audit timestamp and the individual row's observation must
be nonzero, no later than the reconciliation clock, and no older than twice the
larger of the existing audit and REST polling intervals. No new configuration
knob or network pass is introduced. An unsuccessful audit does not manufacture
fresh evidence: the previous success expires normally. Missing, stale, future,
wrong-scope or ambiguous metadata retains the original fail-closed error.

The proof only classifies work as outside this node. It does not prove that a
sibling is healthy, that its runner exists, or that its job can run. In
particular it grants no session adoption, admission, recreation, cancellation,
rerun or VM authority. Foreign jobs are never persisted as this node's queued
work. Unknown unmatched jobs still reject the snapshot, preserving prior local
inventory instead of replacing unavailable observations with an empty queue.

## Consequences and evidence

No schema or public fleet.v1 change; no mode enablement or live configuration
change. Authority, observe and shadow execution limits remain intact. Daemon
replay uses the real REST observer, coordinator and SQLite store. The fresh
same-platform sibling case was red before production code, while refusal cases
already passed. Tests cover missing/stale/future/wrong-scope/local-ID/unknown
selector/old-row evidence. The deterministic simulation executes real
reconciliation and SQLite across proof arrival, loss and return, retaining local
work and never acquiring the foreign job. The simulation assumes audit metadata
is authoritative, as the existing audit contract does; it makes no claims about
cross-node session takeover or redistribution.

## Owner's three questions

1. Remove overengineering: reuse the existing audit and ingestion path; do not
   add a sibling watcher or a new controller.
2. Reduce complexity: carry a scope-qualified selector proof into the existing
   unmatched-job predicate. Platform-only classification cannot express a
   same-platform peer; a generic "ignore unmatched" rule would erase uncertainty.
3. Test this: the deterministic replay and evidence-loss world exercise the
   actual reconciliation/store path, including failure that preserves old work.
