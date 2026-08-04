# ADR 0015: Canonical job inventory and truthful capacity

## Status

Accepted with staged production activation.

## Context

The official GitHub Actions Scale Set message stream is authoritative for
acquisition and runner lifecycle, but it is not a complete repository backlog.
Requests can be hidden behind scale-set capacity, redelivered under new request
identities, or observed only after a long-running job releases capacity. An
inflated `maxCapacity` exposed one successor at the cost of advertising runner
capacity the host could not execute. It still did not provide stable workflow
job identity or complete queue-SLO evidence.

Generation-specific updater LaunchAgents also couple release activation to
plist replacement and an updater handoff even though the installed-generation
manifest is already the durable selection point.

## Decision

Adopt the architecture in `docs/FLEET_ARCHITECTURE_PLAN.md` in independently
releasable phases:

1. Reconcile bounded active workflow jobs through the GitHub REST API and keep
   that replaceable snapshot as observation data. REST supplies stable workflow
   job identity and queue health; it never grants runner acquisition, JIT,
   deregistration, or deletion authority. API failure retains the last durable
   snapshot and is reported as stale or unavailable rather than as an empty
   queue. Profile routing mirrors GitHub's label-subset match against the
   effective scale-set labels, including the automatically advertised scale-set
   name and configured aliases. An unmatched or multiply matched self-hosted
   job makes the observation unavailable. All profiles in one scope replace
   their prior REST snapshot in a single transaction.
2. Keep the official scale-set protocol as the mutation authority, with its
   commit-before-ack cursor, at-least-once delivery, complete aggregate
   statistics, in-place session recovery, and secret boundaries.
3. Set each scale set's `maxCapacity` to the executable maximum derived from
   its scope, repository cap, profile vector, and host limits. Capacity must
   not be inflated solely to reveal queued work.
4. In the later updater phase, replace generation-specific updater handoff with
   a small stable launcher that verifies and executes the immutable manifest.
   Activation may preserve running VMs when database and operation-state
   compatibility is proven; ambiguous host mutations still block activation.

This decision narrowly supersedes:

- ADR 0002 only where REST was limited to compatibility use and the scale-set
  stream was assumed to expose the complete scheduling backlog;
- ADR 0009 only where a delivery-only slot required
  `maxCapacity > runtimeCapacity`; and
- ADR 0011 only where activation requires an empty fleet and replacement of a
  generation-specific updater through a handoff job.

All other safety decisions in those ADRs remain accepted. The updater
supersession is architectural direction, not permission to remove the current
handoff before Phase 4 rollback and reboot proofs pass.

## Rollout gates

The implementation may merge dormant because an omitted or false
`github.canonicalJobInventory` preserves the proven scale-set protocol and its
inflated delivery lookahead. Production activation sets the flag to `true` and
may happen only after:

- the GitHub App has read-only Actions permission for every installation;
- canonical observe proves complete queue visibility beside the incumbent
  without opening official scale-set sessions or owning lifecycle effects;
- shadow runs only after the incumbent no longer consumes the same scale-set
  sessions;
- the exact-scope canary proves queue, acquisition, job execution, runner
  absence, and VM deletion; and
- a versioned candidate configuration sets `canonicalJobInventory: true` and
  validates truthful capacities.

The flag is the atomic contract boundary: when true, REST inventory health is
readiness-critical and validation rejects both inflated and under-advertised
capacity. The running authority configuration and live scale sets remain
unchanged until those gates pass. Capacity migration is performed only while
the affected scale set owns no active job.

## Consequences

Queue-SLO health no longer depends on unused delivery capacity or guessed job
identity. A REST-only job can degrade queue health without becoming executable,
while protocol-only work remains executable and visibly provisional when REST
is stale. Same-name matrix jobs may share canonical age but never receive a
guessed numeric job ID. Official statistics bound all acquisition.

The REST observer requires only read-only Actions permission in this phase; it
does not fetch repository runner inventory. Official lifecycle events correlate
terminal cleanup to the authoritative runner name as specified by ADR 0016.
Workflow-concurrency classification, ETag caching, scheduler v2, and stable
launcher activation remain separately gated work.

## Amendment: a shared label bounds the lane by the set's own share

Two decisions above assume a scale set is alone on its labels within its scope:
"profile routing mirrors GitHub's label-subset match", and "an unmatched or
multiply matched self-hosted job makes the observation unavailable". Both were
right while ownership was partitioned.
[ADR 0034](0034-a-node-serves-the-scale-sets-it-owns.md) now permits two nodes
to own two scale sets in one scope carrying identical labels, because GitHub
places the work between them itself. Under that topology the REST scope
observation returns the whole scope's queue, every job in it matches both sets
by label, and this record as written has both nodes record all of it — so one
job becomes two nodes' demand, and each spawns a guest for it. Issue #153.

What changes, and only for a scale set whose labels are declared shared or
observed to be:

1. **A binding claims at most its own share of the scope's queue.** The share is
   the scale set's own statistics — work GitHub has offered it, plus work GitHub
   has assigned it that has not started — which is the same expression this
   record already uses to bound acquisition, and which equals
   `statistics.totalAssignedJobs` in the shape ADR 0034's spike observed. A job
   the binding's own durable demand already names is never surrendered to that
   bound; the broker is the mutation authority and its word on ownership
   outranks a counter. Statistics that are absent, stale, or ahead of the clock
   bound the claim to that vouched set alone, because on a shared label an
   unbounded claim is the defect being fixed; an *unreadable* statistics store
   is different and still makes the whole observation unavailable.
2. **A multiply matched job is no longer automatically unavailable.** Two
   bindings serving one scope with one profile are interchangeable servers of
   the job, and the bound above is what makes recording it on both safe. A job
   matching two *profiles* names two different guest shapes and a job matching
   two *scopes* names a repository routed twice; both remain the conflict this
   record specifies, because no capacity number disambiguates them.

The cost is stated plainly: on a shared-label scope the queue this node reports
is its own share, not the scope's backlog, so the queue lookahead past a set's
own advertised capacity is no longer available there. That is the honest number
for that node — the peer is reporting the rest — and it is the price of not
being able to see the peer at all. Scopes without a shared label are unaffected,
in behaviour and in reported queue depth.

The declaration is per scale set (`sharedLabels`) and requires
`canonicalJobInventory`, which ADR 0034 already states as a precondition: GitHub
splits a shared queue by advertised capacity, so two nodes both inflating that
number for lookahead would have it split by fiction. It cannot be inferred,
because ADR 0034's first decision is that no node is aware of another.
