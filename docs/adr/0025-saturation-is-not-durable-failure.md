# ADR 0025: A saturated host is not durable evidence of failure

## Status

Accepted. Refines the authority lease introduced with the authority mode cutover
and the closed scheduler-tick vocabulary
([ADR 0020](0020-diagnosable-drain-failures.md) established the pattern for
runner cleanup).

## Context

The fleet is a second pilot on a machine whose other tenant is CI
([ADR 0018](0018-second-pilot-elastic-host-envelope.md)). When that tenant
saturates the host, every fleet operation still has to reach one SQLite
connection shared by the scheduler's plan commit, the operations worker, and the
authority lease. Two control-plane decisions read the resulting latency as
durable failure.

**The authority lease.** The guard renews every ten seconds inside a thirty
second TTL, with a five second deadline per attempt. Any error at all ended the
process. On 2026-08-01, with load 26 and CPU idle 1.6%, the production daemon
exited three times inside seventy minutes:

```
fleet daemon failed: controller authority lost: renew lease: context deadline exceeded
```

At each of those exits the durable lease row still named this owner for roughly
twenty more seconds. The daemon inferred that authority had moved from an I/O
attempt that merely failed to complete. Every exit then cost a launchd restart
plus the successor's wait for its own abandoned lease to drain — the fleet
stopped scheduling for tens of seconds to repair a blip that had already passed.
The same line has been recorded roughly 118 times historically.

**The scheduler tick.** `reconcile.Controller.Commit` reports four different
incidents through one error value: a lost compare-and-set against
`scheduler_state.version` or an instance row, a plan the durable layer refuses as
malformed, an unavailable store, and an invalid request. All four logged
`component=scheduler reason=plan_commit_failed`, and all four cost the loop a
full five second error backoff on a five second tick interval.

Between 18:15:48Z and 18:23:58Z on 2026-08-01 that warning appeared once a
minute for eight consecutive minutes — the reporter's rate limit, so the real
rate was per tick — while `scheduler_state.version` never left 2000 and six
profiles sat queued past the SLO. The log could not say whether the fleet was
losing a harmless race or being refused by the durable layer, which are opposite
operator responses. Because the rate limiter keys on component and reason
together, one frequent harmless cause also suppressed every other for a minute
at a time — the exact failure mode the vocabulary exists to prevent.

## Decision

**Patience is bounded by the durable lease, never by a retry budget.** A renewal
that fails for a transient reason is retried every two seconds while the lease is
still provably held. No attempt may outlive `ExpiresAt`: the per-attempt deadline
is the smaller of five seconds and the remaining validity, an expired lease is
reported as lost rather than renewed, and the guard surrenders as soon as no
further attempt could land before expiry. A fencing loss (`ErrLeaseLost` — the
row is gone or another owner holds it) or a malformed request (`ErrInvalid`)
still surrenders on the first attempt, because those are proof rather than
latency.

This only ever narrows the window in which a process claims authority. It never
extends a lease, never steals one, and never lets two controllers overlap: the
successor's acquisition rule (absent or expired row) is untouched.

**A commit failure names which of the four incidents it is.** The closed
vocabulary gains `plan_commit_contended` for an optimistic-concurrency loss and
`plan_commit_rejected` for a plan the durable layer refuses; `plan_commit_failed`
keeps its original meaning of an unavailable or refusing store. Classification is
by the sentinel the durable layer already returns and is additive — every
`errors.Is` check callers already perform keeps working.

**Contention is paced as the self-healing condition it is.** Only
`plan_commit_contended` may declare itself transient. Its retry starts at 250 ms
and doubles per consecutive conflict, saturating at the ordinary error backoff,
so a conflict that is genuinely not clearing degrades to today's behaviour
instead of spinning against the store it contends with. Reporting is unchanged:
every failure, transient or not, still reaches the bounded failure hook.

## Consequences

A transient store timeout no longer terminates the control plane, and a lost race
no longer costs a scheduling round. An operator reading
`reason=plan_commit_rejected` now knows the condition repeats until inputs change
and warrants investigation, while `reason=plan_commit_contended` is expected
under load and warrants nothing.

Fail-closed behaviour is preserved in both places. Authority is still surrendered
the moment it cannot be proven, and no admission, drain-safety, or ownership
guard is relaxed. The change is to how long the control plane waits for evidence,
not to what counts as evidence.
