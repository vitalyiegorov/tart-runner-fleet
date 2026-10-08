# ADR 0059: Foreign-platform jobs do not poison node inventory

## Status

Accepted for the focused fix in issue #356. Operational activation remains gated by ADR 0015; this record does not authorize a production cutover.

## Context

On 2026-10-08, all three Macs advertised shared macOS labels while canonical job inventory was disabled and queue-lookahead capacity was inflated. Declaring shared labels alone correctly failed configuration validation. A quiescent MacBook canary with the supported truthful capacities could fetch complete repository snapshots, but reconciliation reported `queue_reconcile_failed` for repositories also queuing Linux AMD64 jobs. The canary was rolled back; Studio and Mini were not changed.

The REST API observes a repository, whereas a node serves only its configured execution platforms (ADR 0034 and ADR 0055). Strict routing incorrectly treated a canonical request for a sibling-only platform as an unavailable local route. The simulator's repository snapshots previously contained only jobs served by its local broker, so they could not reach this production state. This was a world-model blind spot exposing a fleet defect, not an oracle defect.

## Decision

When no binding matches a queued self-hosted job, a single well-formed canonical label for a platform absent from this repository's local bindings proves that job is outside this node's inventory. Ignore it for local attribution. Allow only the canonical route, `self-hosted`, and a consistent OS label in this proof. Unknown labels, malformed vectors, contradictory or multiple canonical routes, an unaccepted repository, and an unknown shape on a platform this node serves still fail closed. Ambiguous matching bindings remain errors.

The foreign job is not assigned, acquired, deleted, expired, or counted as local demand. Complete local observations continue to reconcile normally. Unavailable GitHub snapshots, statistics, and storage still remain unavailable. Broker assignments remain the lifecycle authority, and shared-label claims remain bounded by each set's own statistics.

No knob, process command, backend dependency, database migration, or `fleet.v1` field is introduced. The invariant is one named pure predicate beside existing label matching. Changing the production inventory flag still requires permission, observe/shadow/canary evidence, truthful capacity, fresh quiescence, readiness and rollback gates from ADR 0015. Already assigned work is preserved.

## Evidence

A failing unit replay reproduces the MacBook mixed-platform snapshot. Unit cases retain unknown/contradictory routing errors. Deterministic simulation captures repository-wide foreign jobs beside local broker jobs on macOS-only and Linux-only nodes across 24 seeds each, and verifies one local attribution and no foreign attribution.
