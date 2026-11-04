# ADR 0091: The macOS 4x7 profile is `compact`

## Status

Accepted. Amends the profile vocabulary of
[ADR 0032](0032-resource-explicit-runner-labels.md) and the recipe in
[`BASE_IMAGE.md`](../BASE_IMAGE.md). Changes no admission rule: the scheduler reads
the configured vector, never a name.

## Context

`rnw-community/mobile-ci` v5 removes Maestro. The profile named `maestro` stays: it
is the 4 vCPU / 7 GiB macOS shape, and iOS builds and the native Swift lane still run
on it. Its name now describes a tool nothing uses.

The name lives in places with different costs to change:

- the Go field and the on-disk key `macosBurst.maestro`, which every node's file
  carries and every release decodes with `DisallowUnknownFields`;
- the `fleet.v1` policy key, whose field names the contract preserves and whose bytes
  the policy digest covers;
- macmini's live profile ID `maestro`, which instance names and durable rows carry;
- the runner label `macos-maestro` and scale-set names such as `trf-sudoku-maestro`,
  which are live GitHub objects.

## Decision

1. The profile is `compact`, beside `builder`. The Go field is `MacOS.Compact`.
2. **On disk, a release reads both `compact` and `maestro` and writes `maestro`.** A
   file naming both fails closed. `scale-sets provision --write` re-encodes a live
   file, and a rollback target would refuse `compact`, so writes keep the old key until
   no release in rollback reach refuses the new one. A later change then flips the
   writer and drops the old key.
3. **The `fleet.v1` policy key stays `maestro`.** The contract outranks the rename, and
   a changed key would move every node's policy digest.
4. Consumers route on `trf-macos-arm64-4x7`, the canonical label of ADR 0032. No new
   alias is introduced. `macos-maestro` stays an alias until no consumer uses it.
5. New images carry no Maestro, and the `maestro-cli` capability leaves the
   documentation and example configuration.

## What this does not do

No live state changes. Node files, scale-set labels and names, and macmini's profile ID
are operational steps with their own plan, rollback and approval. Adding a label to a
live set may need `fleet scale-sets recreate` (issue #135), which loses the jobs assigned
to that set. No image is built or promoted. Authority limits are unchanged.

## Consequences

Until those operational steps run, `maestro` survives in node files, the fleet.v1 key,
macmini's profile ID, scale-set names and simulation fixtures. In those fixtures it is
an opaque test ID; earlier ADRs keep it as history.
