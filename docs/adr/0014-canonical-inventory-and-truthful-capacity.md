# ADR 0014: Canonical job inventory and truthful capacity

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
   queue.
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
- observe/shadow proves complete queue visibility without owning lifecycle
  effects;
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
does not fetch repository runner inventory. Terminal runner/VM correlation,
workflow-concurrency classification, ETag caching, scheduler v2, and stable
launcher activation remain separately gated work.
