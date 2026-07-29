# ADR 0023: Scale-set drift is repairable, by explicit opt-in

## Status

Accepted, default off. Narrowly amends the provisioning invariant that an
existing scale set is never mutated.

## Context

`Provisioner.Ensure` reused an exactly matching scale set and failed closed on
anything else: `inspect` returned `ErrConflict` for an existing object whose
name, runner group, labels, or runner setting no longer matched configuration.
The provisioner offered exactly two actions, create and reuse.

Failing closed there is right as a default. Silently rewriting a live GitHub
object because configuration moved is how a fleet destroys routing it did not
intend to touch, and `scale-sets provision` exists mainly to create what is
missing.

But it left no repair path at all. A drifted scale set could not be fixed
through the fleet in any mode, with any confirmation. The only remaining option
was out-of-band surgery in the GitHub UI, or the Actions runner-admin API, which
the fleet does not expose and which is not reachable with an ordinary REST
token. So a one-field divergence became a permanent, operator-blocking condition
that the fleet could detect precisely and do nothing about.

That is a liveness gap, not a safety one, and it is the mirror image of the
problems ADR 0017 and ADR 0022 addressed: the controller correctly refusing to
act, and thereby wedging itself, because the only alternative it knew was to
refuse.

## Decision

Drift becomes a plannable, repairable action rather than a terminal error, gated
on an explicit opt-in.

`Provisioner.ReconcileDrift` defaults to false and preserves the previous
behavior byte-for-byte: drift returns `ErrConflict` from planning and nothing is
written. When enabled:

1. `Inspect` reports `update` for the drifted object, with its existing id. The
   plan stays read-only, so an operator sees exactly what would change before
   anything is written.
2. `Ensure` repairs the object **in place** through `UpdateRunnerScaleSet`. It
   does not delete and recreate. GitHub routes queued jobs to a scale-set id, so
   replacing the object would orphan work already assigned to it.
3. The write is verified, not trusted. GitHub may accept the call and still leave
   the object different; the result must satisfy the same exactness comparison
   used for creation, or the outcome is `ErrUncertain` and is never reported as
   provisioned.
4. An exactly matching object is still reused untouched. Enabling reconciliation
   does not start rewriting healthy scale sets.

The opt-in is a distinct CLI flag, `--reconcile-drift`, not an extra meaning
attached to the existing `--confirm provision-scale-sets` token. Repairing an
existing object is a strictly larger authority than creating a missing one, and
an operator who ran provisioning to create what is missing must not discover
afterwards that it also rewrote something live.

## Consequences

A detected divergence is now fixable by the fleet that detected it, with a
plan-first workflow and a verified write. Configuration remains the single
source of desired state.

The default is unchanged, so every existing invocation behaves exactly as
before, including the `ErrConflict` that callers and tests rely on.

Repair covers only what the Actions API models on a runner scale set: name,
runner group, labels, and runner setting. `RunnerScaleSet` has no capacity
field, so scale-set capacity is not a drift dimension and cannot be reconciled
here — a scale set that exists and matches on all four modelled fields is exact
as far as this decision is concerned, whatever else may be true of it
server-side.

This decision deliberately does **not** add deletion. Update-in-place repairs
drift; it cannot replace an object whose fault is not expressed in any modelled
field. Replacing a scale set is destructive, needs its own evidence that the set
holds no live runners and no assigned jobs, and is therefore a separate
decision.

## Evidence

- `internal/adapters/githubscaleset`: drift planned as `update` with the existing
  id and no write during planning; drift still failing closed by default; repair
  updating in place without creating a second object and preserving the id; an
  accepted-but-still-different result refused as uncertain; a refused write
  propagated; an exact object left untouched.
- `internal/provision`, `internal/cli`: `update` accepted as a valid planned
  action, the opt-in flag selecting a reconciling provisioner, and an action the
  provisioner never emits still failing closed.
