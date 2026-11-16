# ADR 0099: Opt-in recovery of runs GitHub never assigned

## Status

Accepted for implementation, #520; no operational promotion. Amends ADR 0098's
operator-only remedy. ADR 0034's independent node boundary remains intact.

## Decision and evidence

Four observed cancel/rerun recoveries delivered jobs in about 60 seconds, whereas
GitHub's own requeue took 74 minutes to 2.5 hours. Reuse ADR 0098's exact candidate
predicate, audit freshness, routing and durable demand correlation. Do not add a
standing offer: #297 showed busy-node assignment can strand work for 11 hours.

`github.orphanRequeue.mode` is `off` by default. `observe` performs fresh REST
safety checks and reports the intended action without mutation. `act` runs only
in daemon authority mode, additionally requiring the installation token's granted
`actions: write` permission. Shadow, canary and daemon observe gain no authority.

Candidates must be queued for max(queue SLO, configured threshold), default
15 minutes. Every matching local and sibling scale set must have fresh audited
assigned=0 evidence and the local bindings must have no correlated demand.
Elect the node owning the lowest matching scale-set ID across the run's queued
jobs in the scope, including separate job label sets. Other nodes only report.
This relies on each set having one authority owner; no peer protocol is added.

Before acting, re-read the run and all latest-attempt job pages without ETags.
Require the candidate still queued in that attempt, run queued/in_progress,
and every job queued/waiting/completed (unknown states fail closed). Exclude
repository/workflow/job-name patterns against the fresh run and all its jobs.
Repeat the run/attempt check before cancel, then poll at five-second intervals
for at most one minute before rerun-failed-jobs. Changed attempts, completed
runs and cancel/rerun 409/422 conflicts become no-ops. Other failures stop the
attempt; response bodies and credentials are never recorded.

**Residual race:** REST reads and run cancellation are not atomic. GitHub has no
conditional cancel endpoint. A job can start after the last jobs read. These
gates prevent cancelling an observed running job; they cannot prove no live job
is interrupted. REST also cannot identify concurrency-group holds. Budgie's
`ios-simulator-farm` and `android-emulator-farm` with `queue: max` are the canonical
exclusion examples. Default exclusions are empty, requiring operator review.

SQLite migration 15 adds an identifier-only reservation/outcome ledger. Commit
the reservation before HTTP. Failed, held and uncertain attempts consume the
same conservative limits: default one attempt per run per 30 minutes, at most
three runs per rolling hour across all scopes on this node. Act on at most one
run per REST refresh, re-entering through fresh inventory and audit before another. A restart retains
these limits. A permission denial pauses the entire scope for one hour and
reports "grant Actions: write to the GitHub App"; it cannot retry every poll.
An interrupted reservation remains uncertain until the cooldown expires; we do
not automatically resume a half-completed cancel/rerun. History retains outcomes
and the doctor reports the last ten per scope with fresh inventory findings.

## Simplicity and proof

1. Remove overengineering: reuse the existing orphan predicate and doctor ingest
   row, transport and token source. No coordinator service or new demand exists.
2. Reduce complexity: one reservation is both durable dedupe and the rate-limit
   accounting entry. Off defaults and existing daemon authority gate bound rollout.
3. Testability: deterministic coordinator, HTTP and SQLite replays cover the
   gates, election, ordering, restart and limits. The existing all-gated broker
   simulation characterizes the triggering state; recovery replay models a new
   offer after rerun. This is not evidence of GitHub's upstream contract.

No existing fleet.v1 field or enum changes. Doctor detail is additive text;
no scheduler/capacity policy, deployment or authority promotion changes.
