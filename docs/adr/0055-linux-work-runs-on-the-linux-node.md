# ADR 0055: Linux work runs on the Linux node

## Status

Accepted 2026-09-21 (operator directive). Amends ADR 0034's shared-label
federation for the Linux profiles and ADR 0036's "never remove a node's
capability" for one capability on one platform, for the reason below.

## Context

Apple's Virtualization framework allows **two virtual machines per Apple
silicon host**. Every job a Mac runs is a VM, so a Mac has exactly two job
slots regardless of its CPU and memory budget. The mini's Linux profiles
(`linux-small` … `linux-xl`, arm64 Linux VMs) therefore compete with macOS
jobs for the only two slots a Mac has, while node-b runs Linux work in
rootless podman containers with no such limit and KVM for emulators.

Measured 2026-09-21: the fleet's nightly simulation on the mini's `large`
Linux VM died five times in nine nights with Virtualization.framework
crashing on its vsock device thread (#264); moved to node-b (#337) it passed
first time. The same day the mini's macOS queue sat 100+ minutes deep while a
Linux VM held one of its two slots.

## Decision

- A job that does not need macOS runs on the Linux node. Consumer workflows
  and mobile-ci's reusable-workflow defaults name `trf-linux-amd64-2x4` /
  `trf-linux-amd64-4x8`, never a Mac's Linux label.
- The fleet's own workflows (CI verified builds, release build and publish,
  main release) move in the same change.
- Once every consumer has moved, the Macs' Linux scale sets are retired
  (recreate-or-delete per #336's guarded path) and the Linux base image is
  dropped from the Mac images. Until then the sets stay bound so nothing
  strands (#164).
- The macOS slots are spent on macOS: two maestro VMs, or a builder and a
  maestro, and the throughput lever is more work per VM (rnw-community/mobile-ci#147),
  not more VMs.

## Consequences

- node-b's queue carries all Linux work; its capacity (14 CPU / 22.5 GB
  budget on a box shared with the operator's sessions) is now the Linux
  ceiling, and the next Linux capacity is another Linux host, not a Mac.
- The fleet-canary lane (`tart-fleet-canary`, a Linux VM on the mini) is the
  one exception kept for release promotion until the canary set moves too.

## Not addressed

Arch-floating aliases and the consumer label policy (#199) are unchanged;
this decision is about which node, not which label family.
