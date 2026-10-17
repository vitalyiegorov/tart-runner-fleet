# 0066: KVM runners share host emulator slot locks

Status: accepted for implementation; production enablement pending.

## Context

Issue #369 and mobile-ci #169 require a host-wide Android emulator cap.
Lockfiles inside each disposable runner are different inodes and cannot enforce
that cap. The existing KVM profile grant already identifies the only containers
that need emulator coordination; the scheduler's resource admission is unchanged.

## Decision

Add optional `executor.emulatorSlotsDir`, a dedicated host directory mounted
writable at `/var/lib/mobile-ci/emulator-slots` only for KVM-granted profiles.
Use one Podman bind-mount argument, without shell interpolation, automatic
creation, recursive ownership changes, privileged containers, or arbitrary guest
mount destinations. Reject relative, unclean, root, comma-containing, newline-containing and NUL
paths, and refuse the setting without a Podman executor and KVM grant.
An absent source is a runtime creation failure, never an isolated fallback.

Operators provision a fixed set of `slot-*` files and enable the consumer's slot
limit explicitly. Every runner on the host must see the same files; never unlink
or replace an occupied lockfile. Configuration policy exposes only the enabled
capability, not the host path. Leave the directory unset to retain existing
behaviour. Older binaries reject the new configuration key, so remove it before
rolling back to a binary that predates this change.

## Evidence and limits

Deterministic tests cover config validation/round-trip, policy projection,
profile-scoped argument vectors, and daemon wiring. The existing scheduler
simulation continues to exercise resource admission without a new scheduling
axis. Kernel advisory locks and mount namespaces are outside its world model:
the opt-in real Podman test starts two containers and proves mutual exclusion
and release on holder exit. It must pass on the target host before enablement.
Unit or smoke-test evidence does not authorize authority promotion or a service
restart. No incumbent container or controller mode changes in this PR.
