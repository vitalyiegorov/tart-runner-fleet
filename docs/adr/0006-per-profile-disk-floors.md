# ADR 0006: Per-profile virtual-disk floors before first boot

## Status

Accepted.

## Context

Tart's official Linux images use a minimal 20 GB disk. A large CI job can hold
a full Git checkout, package cache, dependencies, multiple benchmark worktrees,
artifacts, and runner diagnostics simultaneously. CPU and memory may remain
healthy while that inherited guest disk fills, causing the GitHub runner itself
to lose its diagnostic log and terminate the job.

Host free-space guardrails do not solve guest exhaustion: the two capacities
are different resources, and a sparse virtual capacity does not reserve its
entire maximum on the APFS host.

## Decision

Each Linux profile declares a positive `diskGb` floor in authority mode. The
provisioning state machine passes the floor to the Tart adapter before boot.
The adapter observes CPU, memory, and disk together, then performs one bounded
`tart set` argument-vector call when reconciliation is needed:

- CPU and memory must equal their configured profile values;
- disk capacity must be at least the configured floor;
- an inherited disk above the floor is accepted and never shrunk;
- a lost command response is reconciled by a fresh `tart get` observation;
- a running VM with resource drift fails closed.

The default Linux floor is 50 GB, matching Tart's published quick-start
recommendation. Most cloud-ready Linux images grow their root filesystem on
boot. macOS profiles keep their immutable base capacity unless an operator
explicitly configures a larger floor, because shrinking is prohibited and APFS
layout changes are base-image concerns.

Legacy configurations without `diskGb` remain decodable for observe/shadow
inspection, but cannot start authority until every Linux profile is migrated.

## Consequences

Large toolchain and benchmark jobs no longer inherit an unsafe 20 GB ceiling.
Sparse copy-on-write disks consume host storage only as written, while the
existing host reserve still blocks new VMs when physical capacity is low.
Disk size becomes an explicit lifecycle invariant without incorrectly adding
virtual capacity to the additive CPU/memory scheduler envelope.
