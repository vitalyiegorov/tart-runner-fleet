# ADR 0098: Unassigned REST jobs are an ingest finding

## Status

Accepted, issue #520. Amends ADR 0084's accepted all-gated trade-off and
ADR 0056's ingest diagnosis. ADR 0034's node boundary remains intact.

## Evidence and limits

Three incidents on 2026-11-15 had old queued REST jobs, assigned=0 on all
matching sets, and no local runner demand. Cancel and rerun recovered them.
Session withdrawal is correlated, not a proven upstream cause. The deterministic
broker model reproduces the failure by leaving an unassigned arrival untouched
when capacity returns; it does not establish GitHub's retry contract.

A standing session alone cannot establish prevention: ADR 0096 advertises zero
on a gated idle set. The existing scale-set listing/statistics interface exposes
counts, not live-session ownership. Registered runners are not proof of an
active session. Listings do not atomically arbitrate simultaneous withdrawals.
The proposed static sibling-count fallback protects singleton labels but still
lets all identically labelled siblings withdraw together, the reported topology.
We therefore defer prevention rather than claim that fallback fixes #520.
A safe prevention design needs evidence about broker assignment at zero capacity
and an arbitration rule that survives simultaneous gating, before changing yield
or the standing offer. No new peer coordination or capacity policy is added.

## Decision

The existing REST ingest pass diagnoses jobs from its complete snapshot before
shared-label attribution hides jobs assigned nowhere. It reuses the queue SLO
(default ten minutes), the existing routing predicates, broker statistics store,
and durable-demand correlation. A finding requires:

- A queued REST job with an exact queue timestamp, aged at least the queue SLO,
  matching at least one binding this node serves.
- Every matching local binding has valid, fresh, non-future broker statistics
  with assigned=0 and available=0. Missing/stale counters are unknown, not zero.
- No matching binding's active demand names the numeric job or its existing
  repository/run/display-name correlation tuple.

One finding per job names repository, run, job, display name and age, reports
local evidence, and warns that a sibling may be serving it. It fails the existing
`ingest delivery` row in doctor and its existing versioned JSON check reasons.
It recommends operator verification followed by `gh run cancel` and
`gh run rerun --failed`. It never cancels, reruns or creates demand.

Findings are replaced per REST scope only on successful refresh/reconcile and
expire with that scope's observation TTL; failed inventory never means empty.
No additional GitHub requests or rate budget are introduced. No schema migration,
new JSON field, scheduler rule, promotion, or authority change is needed.

## Simplicity and proof

We reuse the existing ingest check instead of adding a second doctor mechanism.
The only new retained state is per-scope diagnostic text, necessary because REST
own-share attribution intentionally drops the evidence before queue summaries.
The all-gated recovery simulation characterizes the broker blind spot; the
coordinator replay and doctor tests prove the new alarm and its freshness bounds.
