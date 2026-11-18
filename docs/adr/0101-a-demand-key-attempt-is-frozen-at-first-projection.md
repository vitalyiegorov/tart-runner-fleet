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

Rows from before migration 17, and rows a rolled-back binary writes, carry
`key_attempt = 0`. Those rows are frozen at `MAX(run_attempt,1)` on every store
open, in the same self-heal pass that reseeds the scheduler state.
`DemandKey()` still falls back to `run_attempt` when the frozen value is 0, but
after an open no such row remains.

Freezing on every open, not once in the migration, is what makes version
switches safe in both directions. It rests on ADR 0011's gate:
- Every generation swap goes through `Prepare`, rollback included, and
  `Prepare` refuses unless the fleet is quiescent: no instance and no retrying
  operation.
- So when a binary opens the store, no VM is bound to any key yet. Freezing each
  row's current key is safe.
- That covers a demand that is still **queued** across the switch. The
  code council on #532 showed this case matters: without the freeze, the new binary
  binds a VM to the derived key, REST learns the real attempt, and the key moves
  exactly as in the incident.
- An older binary ignores the column. Rows it inserts are frozen by the next
  newer open.

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
