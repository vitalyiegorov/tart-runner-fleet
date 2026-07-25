# ADR 0020: Diagnosable and bounded owned runner cleanup failures

## Status

Accepted

## Context

ADR 0007 decided that owned runner cleanup retries indefinitely: a bounded
budget can expire while GitHub legitimately refuses to remove a runner that is
still executing a job, which strands an owned VM behind a dead operation.

On 2026-07-25 that deliberate patience became an untriageable incident. A
maestro instance sat in `draining` for 206 minutes while its deregister
operation burned 397 attempts, and an earlier operation for the same instance
had already burned 469. Every attempt persisted the same string, `runner
lifecycle failed at deregister`, because the drain executor discarded the
adapter error to keep credentials out of durable state. The versioned admin API
published only unlabelled `retrying` and `dead` counts, and the runbook forbids
opening the database while the daemon runs, so a busy-runner refusal, denied
runner administration, an unresolved registration scope, and a failed
pre-removal lookup were indistinguishable from the only evidence an operator was
allowed to read.

## Decision

Runner administration failures carry a closed vocabulary, exactly as scale-set
session failures do (ADR 0009): `runner_busy`, `runner_forbidden`,
`runner_admin_unavailable`, `runner_scope_unresolved`, `runner_lookup_failed`,
`runner_removal_failed`, and the total fallback `deregister_failed`. The
adapter marks which step failed, the router marks an unresolved registration
scope, and a total classifier reduces any failure to exactly one reason.

The drain executor persists `runner lifecycle failed at deregister (<reason>)`.
No upstream text, token, or runner credential ever enters the message, and a
failure the executors did not author is withheld as `unclassified`.

Telemetry publishes a bounded aggregate — operation kind, closed failure code,
how many operations share it, and the worst attempt count — through
`operations.failures` in `fleet.v1`, `fleet status`, `fleet operations`, and the
`fleet_operation_failures` / `fleet_operation_failure_attempts` metrics. An
unreadable aggregate degrades the `operations` observation and never publishes
as an empty failure set.

Cleanup retries gain an escalation ceiling rather than a budget. ADR 0007
rejected a budget because one could expire while GitHub legitimately refuses to
remove a runner that is still executing a job; GitHub's own maximum job duration
is six hours, which at the thirty-second backoff ceiling is 720 attempts. Past
that ceiling no refusal can still be legitimate, so the operation dead-letters:
it stops retrying invisibly and becomes a published dead letter carrying its
classified reason. This is the same bounded-escalation shape the broker session
recovery policy uses (ADR 0009) — keep trying while the failure could still be
transient, then surface it rather than hiding it.

Dead-lettering surfaces; it never forces. Nothing is deleted on a dead letter's
behalf, no drain-safety guard is relaxed, and the busy-runner refusal remains a
retry rather than an abort, because the runner-scoped busy evidence that aborts a
drain (ADR 0016, PR #96) is derived from durable demand records, not from a
refusal that also occurs when GitHub is merely slow to release a vanished
runner's job.

Deregistration also stays idempotent in both directions of the same proof: an
absent runner is a completed deregistration whether the pre-removal observation
returns no runner or the actions service answers that observation with its own
runner-not-found signal. Absence is never inferred from any other failure, so a
401 or 403 can never masquerade as successful cleanup.

## Consequences

A wedged cleanup is now identifiable from one read: the code names the cause and
the attempt count names its age. Alerting can distinguish "GitHub will not
release this runner yet" from "the fleet cannot administer runners in this
scope", which are opposite remediations.

The vocabulary is a compatibility surface. Adding a reason is additive within
`fleet.v1`; renaming one changes an operator contract and a metric label.

A permanently failing cleanup now terminates as a dead letter within six hours
instead of retrying forever, and it is published with its cause. It still holds
its instance's demand identity and its durable instance row until an operator
acts: releasing the host envelope automatically would require treating an
instance whose cleanup failed as consuming no CPU or memory, which is false
whenever the VM still exists and would over-commit the host. Reclaiming a
genuinely dead registration therefore remains an explicit operator action, now
taken with the cause in hand rather than from an unlabelled retry count.
