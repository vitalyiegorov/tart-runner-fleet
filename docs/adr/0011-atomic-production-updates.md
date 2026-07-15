# ADR 0011: Atomic production updates are part of fleet authority

## Status

Accepted.

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

An operator adopts the first already-running generation once. A five-minute
launchd timer then checks GitHub's latest normal production release. The update
path:

1. rejects drafts, prereleases, malformed identities, and non-forward versions;
2. downloads the deterministic archive and external checksum manifest;
3. rejects traversal, links, duplicate members, oversized members, and checksum
   or `RELEASE_VERSION` mismatches;
4. validates the persisted config with the candidate `fleetctl`;
5. defers activation while any queue, VM, retry, or dead operation exists;
6. durably stages the prior and candidate boot generations before launchd is
   touched;
7. restarts in exactly the existing mode and requires exact version, mode, and
   readiness evidence; and
8. atomically commits or restores the previous binary/config/mode/plist tuple.

The updater plist is rewritten to the new immutable `fleetctl` on every commit,
so reboot and subsequent checks use the installed generation. A transactional
`current` link gives humans and agents a stable CLI path while launchd continues
to execute exact immutable paths. The prior release remains available for local
rollback.

## Consequences

- A release cannot interrupt two Maestro VMs, an exclusive builder, Linux work,
  or queued work; it retries after the fleet is quiescent.
- Deleting or reordering GitHub releases cannot automatically downgrade the
  controller.
- The launchd user must have authenticated `gh` access to the release repository.
- Update failures are visible and fail closed; they never silently switch
  authority back to observe mode.
