# ADR 0011: Atomic production updates are part of fleet authority

## Status

Accepted. Amended by
[ADR 0021](0021-dischargeable-dead-letters.md), which narrows rule 5 below: a
dead-lettered operation, and an instance the daemon can prove is parked on one, no
longer defer activation. Retrying operations, queued jobs, and every instance not
provably parked — above all a running VM — still do. Also amended by
[ADR 0019](0019-single-fleet-binary.md), which merges the `fleetd` daemon and the
`fleetctl` operator interface into one `fleet` executable. Every mention of
`fleetd` or `fleetctl` below now denotes a subcommand of that single binary:
`fleetctl update` is `fleet update`, the candidate `fleetctl` that validates
configuration is `fleet config validate`, and the updater plist is rewritten to
the new immutable `fleet`. The transaction, quiescence, readiness, commit, and
rollback rules are unchanged, as is the rule that an updater must never `bootout`
its own launchd job. Releases from before that merge still ship both executables
and remain valid rollback targets.

## Context

The Mac mini rebooted with a healthy Go authority having been started from a
versioned candidate, while the persistent LaunchAgent still referenced an old
observe-only binary. The persisted configuration had also advanced beyond that
binary's strict schema. launchd therefore retried the stale process, the manual
authority disappeared, and no controller could serve queued work.

## Decision

`fleetctl update` owns the complete installed generation: binary directory,
configuration path, controller mode, admin endpoint, daemon LaunchAgent, and
automatic-updater LaunchAgent.

Preparation materializes the candidate configuration path into the daemon's
exact `ProgramArguments` before activation. The release template keeps its
backward-compatible default configuration token so an older installed updater
can install the fixed release; the fixed updater replaces that token exactly
once so config-only rollouts change the daemon's actual `--config` argument as
part of the atomic boot tuple.

An operator adopts the first already-running generation once. A five-minute
launchd timer then checks GitHub's latest normal production release. The update
path:

1. rejects drafts, prereleases, malformed identities, and non-forward versions;
2. downloads the deterministic archive and external checksum manifest;
3. rejects traversal, links, duplicate members, oversized members, and checksum
   or `RELEASE_VERSION` mismatches;
4. validates the persisted config with the candidate `fleetctl`;
5. defers activation while any queue, live VM, or retrying operation exists (see
   ADR 0021 for the parked exception);
6. durably stages the prior and candidate boot generations before launchd is
   touched;
7. restarts in exactly the existing mode and requires exact version, mode, and
   readiness evidence; and
8. atomically commits or restores the previous binary/config/mode/plist tuple.

The updater plist is rewritten to the new immutable `fleetctl` on every commit.
An updater must never `bootout` its own launchd job: launchd terminates the
caller before it can bootstrap the replacement. When the updater is already
loaded, the commit first bootstraps a distinct retrying handoff LaunchAgent.
That job waits until the candidate manifest is durable and the update journal
is cleared, then unloads the updater, bootstraps the rewritten plist, and
verifies launchd's loaded program names the exact candidate release. A failed
commit or rollback never satisfies that gate. The handoff retries failed
replacement attempts and is safely replaced by the next update.

An operator may also select a different absolute, versioned configuration path
while keeping the exact installed version, release directory, mode, and
endpoint. This config-only rollout uses the same quiescence, validation,
activation, readiness, commit, and rollback transaction as a forward binary
update. It does not permit a same-version binary substitution or mode/endpoint
change.

This keeps launchd's cached program, the durable plist, and the installed
manifest on the same generation without allowing a process to terminate its
own commit. Rewriting the file alone is not sufficient.
A transactional `current` link gives humans and agents a stable CLI path while
launchd continues to execute exact immutable paths. The prior release remains
available for local rollback.

## Consequences

- A release cannot interrupt two Maestro VMs, an exclusive builder, Linux work,
  or queued work; it retries after the fleet is quiescent.
  **Amended 2026-08-29 by [ADR 0048](0048-a-node-arranges-the-quiescence-its-update-needs.md):**
  queued work no longer defers activation — nothing is running it, and the
  successor re-observes the same durable queue — and a node with a generation
  waiting may refuse *new* admission to reach quiescence. Running instances and
  retrying operations still defer activation, so no release interrupts a job.
  The original clause made the gate unreachable on the nodes that most needed
  the release: one refused 1011 consecutive times while 26 releases behind
  ([#230](https://github.com/vitalyiegorov/tart-runner-fleet/issues/230),
  [#282](https://github.com/vitalyiegorov/tart-runner-fleet/issues/282)).
- Deleting or reordering GitHub releases cannot automatically downgrade the
  controller.
- The launchd user must have authenticated `gh` access to the release repository.
- The updater handoff is a separate, credential-free one-shot job; it receives
  only validated local generation paths and exact confirmation arguments.
- Update failures are visible and fail closed; they never silently switch
  authority back to observe mode.
