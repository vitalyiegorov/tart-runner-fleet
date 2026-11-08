# ADR 0095: A job a standalone runner serves is foreign

## Status

Accepted, 2026-11-08. Issue [#498](https://github.com/vitalyiegorov/tart-runner-fleet/issues/498).
It adds a third proof beside [ADR 0059](0059-foreign-platform-jobs-do-not-poison-node-inventory.md)
and [ADR 0064](0064-audited-foreign-selectors-do-not-poison-local-inventory.md),
and keeps #434's rule that an unknown shape on a served capability fails closed.

## Context

On 2026-11-08 the Mac mini reported `ready: false` (`critical_observation_unavailable`)
for minutes at a time while knee-doctor CI was busy, with 64
`rest queue reconcile failure ... stage=routing` lines for the knee-repo
observation. The `WebKit Memory` workflow runs on `[self-hosted, macOS, webkit]`,
which `macmini-webkit` serves: a standalone repository runner on the Mini's host
that the fleet does not manage (`self-hosted,ARM64,macOS,webkit`). When it was
busy, a second WebKit job waited in the queue.

That job matched no local set. With strict routing it was then uncertain unless
it named an unserved platform (ADR 0059) or a fresh audited sibling set
(ADR 0064). Neither applied: the Mini has a macOS set in that scope, and a
standalone runner is not a scale set. So the whole scope's REST snapshot was
rejected, and the node lost readiness during exactly the bursts that needed it.
MacBook, Omarchy and Ubuntu were unaffected because none has a macOS set there.

This is a fleet defect. The simulation's REST world had no repository runners
outside the fleet, so it could not generate the state (a world-model blind spot).

## Decision

A queued self-hosted job that no local set matches and no existing proof
excludes is foreign when an **online** runner in the job's own repository
advertises every label the job requests (case-insensitive, as GitHub routes).
The job must request at least one label besides `self-hosted`.

The runner must itself advertise `self-hosted`. GitHub's repository runner
listing shows scale-set runners, ours and any sibling's, with no labels, and a
standalone runner with `self-hosted` and its custom labels (measured on
knee-doctor the same day). That label is what keeps every fleet runner out of
the proof; no name prefix is evidence.

The listing is read on demand, not on every poll. The REST ingester gives the
coordinator its observer's `Runners` call, and the coordinator asks only for a
job that would otherwise fail the snapshot, at most once per repository per
reconcile. A poll whose jobs all route spends no request. Each request is
bounded by the observer's per-request timeout and paged like every other
listing. Nothing from the listing is logged.

A listing GitHub refuses (for example `403` without Administration read), a
listing that fails, an observer without the call, an offline runner, or a label
no runner advertises proves nothing: the job stays uncertain and the snapshot
fails at the routing stage, exactly as before.

Like ADR 0064, the proof only classifies work as outside this node. Such a job
is never persisted as this node's queued work, and the proof grants no
admission, adoption, cancellation or VM authority.

## Consequences and evidence

No schema, configuration or `fleet.v1` change, and no mode enablement. Observe,
shadow and authority limits are unchanged. In the incident's state a node costs
one extra GitHub request per affected repository per poll; in the steady state
it costs none.

- `internal/app`: `TestIssue498JobHeldByAStandaloneRunnerIsForeign` replays the
  Mini's bindings and runner listing (red before the fix);
  `TestStandaloneRunnerProofFailsClosed` keeps no listing, a refused or failed
  listing, an unadvertised label, an offline runner, an unlabelled runner and
  scale-set runners uncertain; `TestStandaloneRunnerListingIsNotConsultedForRoutedJobs`
  proves the steady path makes no request.
- `internal/daemon`: `TestIssue498StandaloneRunnerKeepsTheKneeInventoryFresh`
  runs the real observer, coordinator and SQLite store; the observation is fresh
  with the runner listed and `queue_reconcile_failed` when GitHub refuses the
  listing.
- `internal/adapters/githubscaleset`: `TestObserverRunnersListsOneRepositoryOnDemand`.
- `tests/simulation`: the world now models repository standalone runners;
  `TestIssue498StandaloneRunnerJobDoesNotPoisonAServedPlatform` injects the job
  into macOS-serving worlds, with and without the runner.

## Owner's three questions

1. Remove overengineering: the observer's existing runner listing is reused, and
   nothing is added to the steady poll or to configuration. A declared list of
   foreign labels was rejected: it would be a knob that drifts from the runners
   GitHub actually has.
2. Reduce complexity: one predicate joins the two existing ones in the single
   place an unmatched job is judged. Neither existing proof can express it:
   ADR 0059 judges platforms, which the Mini serves, and ADR 0064 judges audited
   scale sets, which a standalone runner is not.
3. Test this: the simulation could not generate the state, so its world now
   carries standalone runners, and the regression runs real reconciliation and
   SQLite with and without the proof.
