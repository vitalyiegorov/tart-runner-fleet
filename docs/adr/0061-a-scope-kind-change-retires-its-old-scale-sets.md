# ADR 0061: A scope kind change retires its old scale sets under guard

## Status

Accepted, 2026-11-03. Closes issue #475. Extends
[ADR 0056](0056-a-stranded-bound-scale-set-is-recreated.md), whose
`Recreater` port and guard style it reuses.

## Context

The owner asked to move `rnw-community` from a repository scope
(`configUrl: https://github.com/rnw-community/rnw-community`) to an
organization scope, so every rnw repository runs on the fleet. A staged dry run
showed that `scale-sets provision` plans `create` for the org-level sets and
nothing retires the repo-level ones (ids 23/24 on Omarchy, plus siblings on the
other nodes). Those stay registered at the old URL with the same labels and no
listener. GitHub can assign that repository's jobs to them and never reassigns a
job, so the jobs strand. The CLI could create (`provision`) and replace
(`recreate`) a set, but had no supported way to delete one the configuration no
longer names, so onboarding was additive only.

## Decision

**`fleet scale-sets retire NAME --config PATH --scope SCOPE --from OLD_URL
--confirm retire-scale-set --reason TEXT` deletes one set at a scope's FORMER
configUrl, and only when that is provably safe.** It is implemented once, in
`provision.Retire`, and every guard fails closed before the next client opens:

1. **The bound set can never be named.** `--from` must be a valid configUrl and
   its `config.ScopeURLKey` — the same spelling-insensitive identity the config
   validator uses to reject duplicate scopes — must differ from the scope's
   current one.
2. **The replacement exists and is bound.** The scope must still carry `NAME`
   with an id, and the scope's CURRENT configUrl must hold that exact id. That
   check uses the read-only audit port (`scalesetaudit.Client`), so the bound
   scope is never opened with a client that can delete.
3. **GitHub holds nothing on the old set.** Its live statistics, read by id,
   must show zero assigned jobs, running jobs, busy runners and registered
   runners. Unreadable statistics are `ErrUncertain`, never zero (contributor
   rule 4).

The delete reaches `--from` through `provision.Retirer`: the audit listing and
counters plus `Delete(id)`. It holds no `Ensure`, so the retire path cannot
create or repair anything at a scope this node no longer binds. The adapter
(`githubscaleset.Retirer`) is the `Auditor` and the existing
`Provisioner.Delete`, on one official client.

The command prints `scope\tname\tfrom\tid retired`, never writes the
configuration, and never restarts anything: the config already names the new
set, and a restart is a service action with its own evidence (as in ADR 0056).

### Migration runbook: repository scope to organization scope

1. Stage the kind change (`kind: organization`, the org `configUrl`, the
   `runnerGroup`, the added targets) and run
   `fleet scale-sets provision --config PATH --apply --write --confirm
   provision-scale-sets --reason TEXT` on each node.
2. Restart each node's daemon **while it is idle**, so it binds the org-level
   ids and stops listening on the repo-level ones.
3. Wait until `fleet scale-sets audit` (or GitHub) shows each old set with zero
   assigned jobs and zero registered runners. Jobs already assigned to an old
   set will not move; let them finish or re-run them.
4. On the node that owned each old set, run
   `fleet scale-sets retire NAME --config PATH --scope SCOPE --from OLD_URL
   --confirm retire-scale-set --reason TEXT` once per old set.

## Consequences

- A kind change no longer leaves listener-less sets registered, provided the
  operator follows the runbook; the command refuses every step taken out of
  order rather than guessing.
- Not addressed: retiring automatically during `provision`. Deleting a GitHub
  object on the provisioning path is exactly the authority ADR 0056 kept out of
  it, and the drain in step 3 is time-dependent evidence an operator observes.
- Not addressed: a scope whose name also changed. `--scope` is the scope's
  current name; the old set is looked up by the name the scope still carries.
- API cost: two listings and one read per invocation.
