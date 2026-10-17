# ADR 0070: A projection failure does not block its neighbors

## Status

Accepted.

## Context

Issue #134 describes a bad broker event amplifying into a whole-batch failure.
The current implementation commits every accepted event and the inbox cursor in
`ApplyDemandBatch` before attempting lifecycle projection. A deterministic
real-store replay proves the narrower fleet defect: an unprojectable started
event prevents a later valid completion from moving its runner to draining,
although both demand records persist. The historical claim of total demand loss
is not supported by this path; the reported missing queued job needs separate
evidence and is not explained by this reproduction. This is a fleet defect,
not an oracle or world-model correction.

## Decision

After the batch commits, attempt every accepted event's lifecycle projection in
message order. Collect projection errors and return them after the pass. Any
error still fails the handler, withholding acknowledgment under ADR 0002.
Successful neighboring effects remain committed and idempotent. An inbox
duplicate must still attempt lifecycle projection, as it already did before this
change: redelivery retries failures using each request's highest durable rank.

Use the existing durable inbox, demand records, and message retry contract.
There is no new queue, quarantine, expiration, retry limit, acknowledgment rule,
or schema. A failure does not authorize dropping an event or weakening runner
identity correlation (ADR 0016). Existing context deadlines still bound work.
Errors remain internal; no new payload or broker-error logging is introduced.

## Consequences

A later valid lifecycle event in the same delivered message progresses even
while its neighbor cannot project. The binding can still remain degraded and
unacknowledged until the bad event becomes projectable. This does not promise
progress for subsequent broker messages, repair REST/session recovery, or
establish the cause of the historical missing queued job.

`TestIssue134ProjectionFailureDoesNotBlockNeighbors` extends the deterministic
simulation with a retained message naming an unavailable local runner beside
valid queued/started/completed events. It varies the bad event's position, checks all
demand rows persist, repeats the same message while the failure remains, then
makes the missing runner available and verifies acknowledgment and a single
drain operation. A direct coordinator test checks every error remains observable
and every projection starts after batch commit under the durable binding key.

No API, migration, deployment, or authority promotion is required. Observe and
shadow limits are unchanged. Rollback restores the earlier short-circuit
behavior without a data conversion.
