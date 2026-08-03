# ADR 0028: A repeated decision is a new attempt

## Status

Accepted. Extends the spawn generation rule
([ADR 0023](0023-repairable-scale-set-drift.md) covers the drift that makes a
decision recur) to drains, and closes the second half of the 2026-08-02 incident
on host `vitalii-mac-mini`. The first half — a single tick admitting one demand
twice — is a separate change.

## Context

Scheduler operations are content-addressed: `stableID("op", operation)` over the
decision itself. That is what makes a tick idempotent — a replayed plan produces
byte-identical identities and the durable layer collapses it.

Spawns already accept that a decision can legitimately recur. A completed or
failed incarnation leaves a terminal instance row and its clone operation behind,
so `SpawnGeneration` counts those tombstones and the controller salts a distinct
identity that supersedes them. Without it a respawn wedges on a UNIQUE collision.

Drains had no equivalent, and a drain decision recurs at least as easily.
`DrainExecutor.abort` exists precisely for that: when fresh evidence shows a
workflow job executing on a runner the fleet was about to deregister, the drain
is completed as an acknowledged no-op and the instance returns to `Running` — the
conservative busy state. Returning to `Running` restores exactly the state the
recovery was derived from.

On 2026-08-02, `trf-xl-25a374b60f46dafe` on host `vitalii-mac-mini` was handed a
sibling job by GitHub's scale-set broker: the demand the fleet bound it to stayed
`JobAvailable`, so `JobInactive` (keyed on the bound demand) reported no active
job while `RunnerBusy` (keyed on the runner name) reported one. The durable state
records the consequence:

| Operation | Recovery decision | Created | Status |
| --- | --- | --- | --- |
| `op-3bef38e0bcc57090138000db` | stalled assignment | 09:44:48Z | completed |
| `op-e31b41beec0e2f30cc3d30d0` | lingering runner | 09:59:51Z | completed |

`transition_history` shows both drains aborted — `draining -> running` at
09:44:49Z and 09:59:52Z. Recomputing the scheduler's own hash rule over
`{Kind, Instance, Profile, Route, Recovery, ConfirmedInactive, StalledAssignment,
LingeringRunner}` reproduces both identities byte-for-byte, which is how we know
the instance escaped a wedge only because the recovery *flags* differed between
the two attempts: `Assigned` made the first stalled, `Running` made the second
lingering.

No third flag combination is reachable from `Running`. `RunningSince` is the
instance's `updated_at`, so each abort restarts the idle-runner deadline and the
next expiry re-derives `op-e31b41beec0e2f30cc3d30d0` against a row that already
exists. `insertOperation` then fails the operations primary key, `ApplyPlan`
wraps that bare, and the tick reports `plan_commit_failed` — deterministically,
on every later tick, because the planner is pure.

## Decision

**Identity names the attempt; the effect key names the effect.**

`DrainGeneration` counts an instance's prior drain attempts that can no longer
act — completed, dead, or operator-discharged — and the controller scopes the
drain identity to that count through the same `attemptIdentity` rule spawns use.
A pending or claimed attempt is deliberately not counted: it can still act, so a
second drain of one attempt must keep colliding and failing closed rather than
enqueuing the deregistration twice.

The effect key is **not** salted. `operation_effects` is keyed on the physical
effect (`deregister:<instance>`) and is what makes deregistering the same runner
across attempts idempotent; the claim queries gate on `operation_id`, so a fresh
attempt is claimable while the recorded effect stays intact.

`spawnIdentity` is generalized to `attemptIdentity` rather than duplicated, so
spawn and drain supersession cannot drift apart.

## Consequences

An aborted recovery no longer poisons the control plane. The repeat is bounded by
the same evidence it always was — the executor still re-verifies that the runner
is idle and still aborts when it is not — so this changes what a repeat is
*called*, not what it is *allowed to do*.

Fail-closed behaviour is preserved in both directions: a live incarnation still
blocks a respawn, and an in-flight drain still blocks a second drain. Audit
history is untouched; superseded attempts keep their rows.

The underlying churn is not fixed here. `JobInactive` and `RunnerBusy` read two
different keys and disagree by construction whenever GitHub hands a runner a
sibling job from the same scale set, so a busy runner still attracts a recovery
drain every idle-runner deadline and still aborts it. That is a design question
about what evidence binds an instance to work, tracked separately.

It was answered by [ADR 0033](0033-a-runner-is-bound-to-the-job-github-gave-it.md):
the binding follows GitHub, so the two keys read one fact and no drain is planned
for a working runner at all. The generation rule here is unchanged and still
carries the aborts that remain legitimate.
