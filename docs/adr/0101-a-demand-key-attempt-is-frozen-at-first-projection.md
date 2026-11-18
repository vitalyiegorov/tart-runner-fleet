# ADR 0101: A demand's key attempt is frozen at first projection

## Status

Accepted, #400.

## Context

A demand's scheduling key is `{repo, run, attempt, runner request}`, derived by
`DemandRecord.DemandKey()`. A VM is bound to that key, and the scheduler treats a
demand as served only when a live instance names the same key byte for byte.

The attempt was derived from the mutable `run_attempt` column, with 0 (unknown)
normalized to 1. A rerun request is delivered by the broker before REST
correlates it, so its attempt is unknown (#525's episode reset), or before #525
it was inherited from the finished attempt. Either way the key read attempt 1,
and a VM was booted bound to it. When REST then reported attempt 2, the same
row's key moved, nothing incarnated the new key, and a second VM booted.

Production, 2026-11-17, Mac mini: two maestro VMs for one pony runner request,
13 seconds apart. The second idled until the 15-minute assignment deadline. The
store's reservation check caught it only at JobStarted, as the
`demand_commit_conflict reservation_duplicate` failure class of #502.

## Decision

- `runner_demands.key_attempt` is written once, on insert, as `MAX(run_attempt,1)`,
  and never updated (migration 17).
- `DemandKey()` uses it.
- `run_attempt` keeps tracking GitHub's real attempt for everything that needs
  it: #524 queue age, orphan requeue, completed-run expiry of the run.
- Completed-run expiry's live-instance guard compares a bound instance against
  the same frozen attempt, not the learned one, so there is one key derivation
  and not two.

There is no backfill. `key_attempt = 0` means "derive as before", which covers
rows from before the migration and rows a rolled-back binary writes; those keep
today's behaviour. An older binary ignores the extra column, so rollback is safe.

Rejected:
- **Drop the attempt from the key.** The request id already differs between
  attempts, but this is a wide change to instance metadata, ownership resource
  ids and drain identity, and it would mismatch instances already running at the
  upgrade.
- **Re-key bound instances when the attempt changes.** It rewrites live bindings.

## Consequences

A rerun's key no longer moves under a bound VM, so it cannot be admitted twice.
For a rerun delivered before REST correlation, the key's attempt may read 1 while
GitHub's real attempt is 2. That is cosmetic in logs and identity only: every
consumer that needs the real attempt reads `run_attempt`.

Evidence: `TestIssue400RerunDemandKeyIsStableWhenRESTLearnsTheAttempt`. It is
red before the change, both for the moving key and for the expiry guard.
