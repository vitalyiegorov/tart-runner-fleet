# ADR 0021: A dead letter is parked, not busy, and an operator can discharge it

## Status

Accepted. Amends [ADR 0011](0011-atomic-production-updates.md) (the quiescence
rule) and completes [ADR 0020](0020-diagnosable-drain-failures.md) (which made a
wedge nameable and terminable but not remediable). It does **not** reverse
[ADR 0007](0007-durable-runner-cleanup.md): cleanup still retries for as long as a
refusal can legitimately be caused by a running job.

## Context

On 2026-07-25 a scale-set runner registration in the `budgie-at` organization
reached a contradictory state — `status=offline`, `busy=True`, `labels=[]` — and
GitHub refused to remove it:

```
DELETE /orgs/budgie-at/actions/runners/3175
-> HTTP 422 {"message":"Bad request - Runner trf-maestro-096ffcb3a52d8624 is
    currently running a job and cannot be deleted."}
```

A token with `admin:org` failed identically, so this is GitHub-side state and not
a permissions problem. GitHub's own six-hour maximum job duration elapsed without
the registration being released. No workflow run held it: sixty recent runs' jobs
were swept for `runner_name` with zero hits, `--status in_progress` was empty, and
cancelling the two candidate stuck runs changed nothing. The documented remedy for
a busy runner — cancel the owning run — therefore did not exist for this case. The
registration can never be deregistered.

ADR 0020 handled that correctly. The deregister operation was classified
`deregister:runner_busy`, escalated to the 720-attempt ceiling, and dead-lettered
with its reason published. `fleet operations` reported
`{"retrying":0,"dead":1,"failures":[{"kind":"deregister","code":"deregister:runner_busy","count":1,"attempts":835}]}`.

Two defects then surfaced.

**The fleet could not self-update out of the state.** `ensureQuiescent`
(`internal/autoupdate/host.go`) treated `Dead != 0` and any non-zero instance
count as "busy". The phantom's un-completable drain and its durable instance row
therefore made the fleet permanently non-quiescent. The automatic updater logged
`apply production release: prepare update: autoupdate: fleet is not quiescent` on
every 300s tick for hours and refused to install v0.1.281 — the release containing
the fix that bounds the phantom. The defect blocked deployment of its own fix, and
a human had to swap the canonical plist and restart the daemon to break it.
Generalized: **any permanently stuck cleanup disabled automatic updates forever.**

**There was no sanctioned operator remedy.** The CLI exposed read-only
observation plus guarded `scale-sets provision` and `update adopt|apply-latest`.
No verb discharged a dead letter or reaped a phantom row, and AGENTS.md forbids
opening `fleet.db` while the daemon runs. The only escape was database surgery the
contributor contract prohibits.

## Decision

**1. Quiescence means "nothing in flight", not "nothing wrong".** A dead letter
holds no lease, `Claim` cannot select it, and a generation swap cannot interrupt
it. It no longer defers a release. Retrying operations and queued jobs still do.

**2. An instance may be parked, and a parked instance does not defer a release.**
Parked requires two facts at once, and only the daemon's tick can see both: the
durable side proves no operation for the resource is pending or claimed, and the
inventory side proves the owned VM is observed **stopped**. Running and unknown
power states are never parked, so a real VM always defers activation, and a fleet
that cannot see clearly keeps deferring. The judgement is published per dead
letter as `operations.deadLetters[].parked`; `fleet update` deduplicates parked
resource IDs and compares with `> 0`, so an inconsistent daemon cannot invent a
new permanent block — which would recreate the very bug this ADR removes.

An alternative was to keep instance rows blocking unconditionally and rely on the
operator command alone. It was rejected: the incident's row was live, so automatic
updates would still have been disabled until a human intervened, and the liveness
defect would only have been half fixed.

**3. `discharged` is a distinct terminal operation status.** It is not
`completed`, which would tell dependents a prerequisite succeeded; and no longer
`dead`, which would keep the resource parked forever. It leaves both the
`retrying` and `dead` counts, is not claimable, and is not published as a failure
or a dead letter.

**4. One guarded, audited mutation:** `POST /v1/operations/discharge`, surfaced as
`fleet operations discharge`. It follows the existing convention exactly — an
exact `--confirm discharge-dead-letter` token, a non-empty `--reason`, and
fail-closed refusal — and adds authority-mode gating and a structured audit record
of every attempt with both identities, the requested scope, the reason, and the
applied effects. The route is registered only on the private Unix socket, so the
loopback health listener has no mutating route by construction. All guards live in
the daemon: a direct socket caller meets the same bar as the CLI.

The durable transaction refuses unless the operation exists, belongs to the named
instance, is dead-lettered, and no other operation for the resource is pending or
claimed. `--reap-instance` additionally requires a cleanup or terminal instance
state and fresh Tart evidence that the VM is not running.

**5. Ordering: the durable row first, the VM second.** `internal/app/inventory.go`
turns the entire instance observation `Unavailable` when a live row's owned VM is
absent, blocking planning host-wide with no VM left to prove anything. It also
blocks on an untracked `trf-` VM with no row — but that state is repairable by
re-running the same command, because the VM still exists. So the discharge retires
the row inside the transaction and removes the VM afterwards, reports a failed
removal as `vm_delete_failed` with the durable half applied, and is idempotent so
the operator simply retries.

**6. `Reap` is a new, narrow Tart path,** not a relaxed `Delete`. It keeps the
valid-name check, the fresh durable ownership match, absent-VM idempotency, and
the re-observation on a failed delete. It drops only
`Confirmation.ConfirmDeletion`, whose runner-inactive half is exactly what a
leaked registration withholds forever, and it never stops a running guest. The
operator supplies that judgement, with a recorded reason.

## Consequences

- A permanently stuck cleanup can no longer disable automatic updates. The fix
  for the next such wedge can deploy itself.
- An operator has a complete, permitted remedy and never needs `fleet.db` or a
  manual `tart delete`.
- The fleet can now delete a VM on an operator's explicit authority. That is a
  real widening of the destructive surface, bounded by authority mode, an exact
  confirmation token, fresh ownership, a stopped-power requirement, a single named
  instance per invocation, and an audit record.
- Cleanup does not give up sooner. ADR 0007's unbounded retry and ADR 0020's
  720-attempt ceiling are untouched, and migrations 6, 7, and 8 — three repairs of
  wedges caused by drains dead-lettering too early — remain valid; nothing here
  changes when a drain dead-letters, only what happens afterwards.
- `fleet.v1` changes are additive: `operations.deadLetters` and one new endpoint.
- `fleet_operations_parked` is the alertable gauge for capacity nothing will
  reclaim without an operator.
