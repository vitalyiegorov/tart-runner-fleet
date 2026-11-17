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
`actions: write` permission. Shadow, canary and daemon observe gain no authority. Only configured observe/act
recovery that cannot execute in this daemon publishes an inactive finding; unset
and off remain silent.

Candidates must be queued for max(queue SLO, configured threshold), default
15 minutes. Every matching local and sibling scale set must have fresh audited
assigned=0 evidence and the local bindings must have no correlated demand.
Rank matching scale-set IDs across the run. Rank k may act at threshold + k ×
cooldown; the lowest goes first, but an offline/off owner cannot block forever.
Recovery audit evidence must be within twice the REST interval or after the job
crossed the threshold. Configured thresholds must cover the audit interval.

Before cancel, read the run and all latest-attempt jobs without ETags, then
repeat both reads. Any observed transition vetoes cancellation. Every queued or
waiting job must be older than the threshold, including GitHub-hosted siblings.
Unknown states and live jobs fail closed. Exclusions match workflow display
names and job names with case-insensitive whole-string * / ? globs, where slash
is ordinary (no character classes or escapes). Budgie's verified workflow names
are Pull Request, Media smoke and Store screenshots; concurrency group names
cannot match these names.

Observe and act use one claim/finish path. All outcomes retain per-run cooldown;
held, excluded and not_rerunnable do not consume the three-per-hour node
mutation budget. Two successful or uncertain mutations exhaust a run's local
lifetime budget; reserved and cancelled_not_rerun count conservatively. An
observed attempt advance retains the rerun charge. GitHub run_attempt >= 3 independently vetoes
new mutations on every node, including resumes. Doctor reports gave_up for
recovered runs at the cap. A permission-only 403 pauses the scope for an hour, while
Retry-After defers REST via the existing pacing rule.

SQLite migration 16 adds the original run attempt to migration 15's durable
reservation ledger. After accepted cancel, polling/rerun and outcome persistence
ignore caller cancellation and have one-minute bounds. Every unresolved
post-cancel failure or uncertain cancel response becomes cancelled_not_rerun.
Only cancelled_not_rerun records resume automatically, independently of queued
inventory after cooldown and across restart. Each resume must first commit a
fresh hourly admission; the previous row becomes noop while the new row carries
the unfinished lifetime reservation. A permission-denied resume records
permission_denied at its admission time and atomically restores the original
cancelled_not_rerun reservation. The scope pauses for one hour; after that pause
the cancelled run remains resumable without queued inventory, even if exclusions
have since changed. A denied attempt never counts as a rerun. All hourly charges
remain durable. Stale
reserved rows require the doctor remedy: the daemon cannot prove it sent cancel.
Only the original completed/cancelled attempt can be rerun; a newer attempt counts
as recovered without mutation. A still-active original attempt or a deleted run
(NotFound) terminates as noop. Pre-migration records without attempt evidence
require the doctor command, not a guessed rerun. Doctor prints
gh run rerun <id> --failed -R <repo> for these outcomes and stale reservations.

**Residual race:** GitHub offers no atomic cancel-if-idle API. The final run
read, all final job pages and cancel POST share one 15-second context deadline,
regardless of page count. This bounds the client request window, including local
processing; it supplies no server-side atomicity or finite acceptance-time
guarantee. All observed live jobs veto; an unobserved start in that window can
still be interrupted. These changes cannot satisfy a literal guarantee of never
interrupting a live job.

## Simplicity and proof

1. Remove overengineering: reuse the existing orphan predicate and doctor ingest
   row, transport and token source. No coordinator service or new demand exists.
2. Reduce complexity: one reservation is both durable dedupe and the rate-limit
   accounting entry. Off defaults and existing daemon authority gate bound rollout.
3. Testability: deterministic coordinator, HTTP and SQLite replays cover the
   gates, stagger, ordering, restart and limits. The existing all-gated broker
   simulation characterizes the triggering state; recovery replay models a new
   offer after rerun. This is not evidence of GitHub's upstream contract.

No existing fleet.v1 field or enum changes. Doctor detail is additive text;
no scheduler/capacity policy, deployment or authority promotion changes.

## Issue #526: recovery class safety and audit latency

GitHub-managed runs with event `dynamic` cannot be retried. Decode the run event
on the uncached safety read and return `not_rerunnable` before cancellation in
observe and act mode, including legacy resumes. Immediately before a potential
cancel (or observe verdict), read the workflow without cached evidence and require
state `active`; disabled or unknown states also return `not_rerunnable`. Doctor
names the run and this outcome. It consumes no mutation budget and retains per-run cooldown; the durable
reservation still precedes any possible mutation. No schema migration is needed.

A threshold-aged ADR 0098 candidate whose otherwise fresh, zero-assigned audit
rows fail recovery's stricter age check requests one out-of-cycle audit. The
daemon signals the periodic auditor through a coalescing one-slot wake channel,
and admits extra attempts at most once per max(1 minute, audit interval / 3),
including failed attempts. The ingester owns audits and their health errors;
recovery never waits for network I/O. The periodic cadence remains intact.
Recovery waits for the next reconcile to consume the new evidence; failed audits preserve the
last completed observation and never authorize action. No new configuration,
capacity offer, or authority is introduced.

Ownership returns its blocking reason: `awaiting fresh audit`, `assigned to set N`,
or `sibling owner (rank N)`. Missing readings stay unknown. All existing run/job
checks, stagger, exclusions, cooldown, caps and permission pause remain enforced.

The three review questions: reuse the auditor rather than add another polling
mechanism; one callback and shared rate limit express the latency fix without a
new knob; deterministic HTTP, ledger, coordinator and daemon tests plus the
all-gated broker simulation exercise the safety and recovery paths. These are
fleet defects, not simulation oracle or world-model defects. Observe/shadow
authority limits and fleet.v1 fields remain unchanged.
