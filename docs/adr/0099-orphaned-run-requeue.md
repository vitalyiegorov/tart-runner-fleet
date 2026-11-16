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
held and excluded do not consume the three-per-hour node
mutation budget. Two successful reruns exhaust a run's lifetime budget; doctor
reports gave_up. A permission-only 403 pauses the scope for an hour, while
Retry-After defers REST via the existing pacing rule.

SQLite migration 16 adds the original run attempt to migration 15's durable
reservation ledger. After accepted cancel, polling/rerun and outcome persistence
ignore caller cancellation and have one-minute bounds. Every unresolved
post-cancel failure or uncertain cancel response becomes cancelled_not_rerun. Unfinished reserved and
cancelled_not_rerun records are reconciled independently of queued inventory
after cooldown, across restart. Only the original completed/cancelled attempt
can be rerun; a newer attempt or a still-active run on resume is a no-op. Pre-migration records without attempt
evidence require the doctor command, not a guessed rerun. Doctor prints
gh run rerun <id> --failed -R <repo> for these outcomes and stale reservations.

**Residual race:** GitHub offers no atomic cancel-if-idle API. For P job
pages, the oldest final job observation precedes up to P−1 further page GETs
and one cancel POST. The default per-request client deadline is 15 seconds:
one page leaves one POST (15 seconds of request deadline); P pages leave up to
15×P seconds of request deadlines, plus local scheduling and processing. There
is no server-side atomicity or finite acceptance-time guarantee. All observed
live jobs veto; an unobserved start in that window can still be interrupted.
These changes cannot satisfy a literal guarantee of never interrupting a live job.

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
