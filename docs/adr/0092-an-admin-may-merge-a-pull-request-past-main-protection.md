# ADR 0092: An admin may merge a pull request past `main` protection

## Status

Accepted. Changes how `main` is protected, in
[`.github/rulesets/main.json`](../../.github/rulesets/main.json) and the repository's
branch protection. Changes no fleet behavior.

## Context

Nobody can merge to `main` until `Required CI` passes: the ruleset has no bypass
actors, and the branch protection sets `enforce_admins`. The owner wants to be able
to speed up fleet changes when one cannot wait for CI.

## Decision

The repository admin role bypasses the `main` ruleset in `pull_request` mode, and
`enforce_admins` is off. A bypass still goes through a pull request, so it stays
reviewable and reversible.

## What this does not do

Direct pushes to `main` stay blocked. `Required CI` stays required for everyone else
and remains the default for every merge. Version tags stay immutable, with no bypass.

## Consequences

An admin can merge a change that CI would have rejected, so a bypassed merge can
reach release without proof. `Required CI` no longer guards against an admin mistake.
