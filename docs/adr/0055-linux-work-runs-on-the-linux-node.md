# ADR 0055: Linux work runs on the Linux node

## Status

Accepted 2026-09-21 (operator directive). Amended 2026-09-22 (issue #343): the
retirement procedure below is now executable — `internal/config` accepts a
macOS-only node, which it did not when this record was written. See
[ADR 0058](0058-a-node-declares-the-execution-technology-it-has.md) for the
rule change and for the planner wedge the change exposed. Amends ADR 0034's shared-label
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

- A job that does not need macOS runs on the Linux node. This is the target
  policy: consumer workflows and mobile-ci's reusable-workflow defaults name
  `trf-linux-amd64-2x4` / `trf-linux-amd64-4x8`, never a Mac's Linux label.
  Consumers migrate under this decision one by one (`docs/MULTI_NODE_PLAN.md`
  tracks the remaining ones, suuudokuuu's Android lane among them).
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

## Amendment 2026-09-22: retiring Linux from the Macs, the procedure

This record said the Macs' Linux scale sets "are retired (recreate-or-delete
per #336's guarded path)". That overstated the command surface. `fleet
scale-sets` has exactly three subcommands — `provision`, `audit`, and
`recreate` — and `recreate` is a delete-**then**-create: it always leaves a new
GitHub object behind and rewrites the node's config with the new id. It also
refuses a set the config carries no id for. **There is no command that retires
a scale set**, and `recreate` is the wrong tool for a parked one (`USAGE.md`,
`docs/OPERATIONS.md`, and `docs/AGENT_RUNBOOK.md` all say so).

The procedure below is therefore what exists. Run it per Mac, in order, and do
not skip the waiting step: every step before the last is reversible by a git
revert and a provisioning run, and the last one is not.

**Preconditions.** No consumer workflow names this Mac's Linux labels, and
`fleet queues --output json` on this node has shown zero jobs for every
`linux-*` profile, and `fleet instances --output json` zero instances, for a
full day. Exit 4 is unavailable and exit 5 is degraded; neither is evidence of
an empty queue (AGENTS.md rule 4).

1. **Drain the sets.** Remove every Linux scale set from every scope in the
   node's config, leave `linuxProfiles` in place for now, re-render, and
   re-provision. The node stops polling them; the GitHub objects still exist,
   so nothing GitHub has already assigned is stranded.

2. **Watch.** `fleet queues` and `fleet instances` on this node show no
   `linux-*` row with work. `fleet doctor` still passes. Give it a day.

3. **Delete the GitHub objects by hand.** There is no `fleet` command for
   this. Delete each retired runner scale set through GitHub's UI or REST API,
   in the scope it was registered in. Until this is done the sets are parked,
   and `fleet scale-sets audit` will keep reporting them — which is the
   correct behaviour (ADR 0054: a parked set is audited, not trusted), not a
   fault to silence.

4. **Retire the declarations.** Delete `linuxProfiles`, `baseVm`, and
   `baseImageCapabilities` from the node's config.

   **Do not delete `maxLinuxCpu`, `maxLinuxMemoryMb`, or
   `maxLinuxWhenMacosIdle`.** Despite their names they are the node's shared
   cross-platform admission envelope (ADR 0012), and a macOS guest is charged
   against all three. A node that loses them admits nothing at all while
   reporting healthy. `fleet config validate` refuses a zero envelope for
   exactly this reason; keep the values the node already had.

   **Do not delete `vmPrefix`.** It stays required on every node.

5. **Validate before restarting.** `fleet config validate --mode authority`
   must pass. If a Linux scale set was missed in step 1 it fails here, naming
   the set, its scope, and its profile — which is the whole point of the check
   (ADR 0058): a node that polls a set it cannot place work from strands that
   work forever (#164).

6. **Restart and verify.** After the restart:
   - `fleet instances --output json` shows no `linux-*` profile.
   - `fleet queues --output json` shows no `linux-*` profile.
   - `fleet doctor --output json` passes, with `guest console` reading
     `no Linux guests booted on this node` and `runner version` naming the
     macOS image alone.
   - `fleet config policy` publishes a new digest. That is expected and is how
     a peer comparison sees the retirement (ADR 0053).

7. **Reclaim the image, last.** Only once steps 1–6 hold on this node:
   `tart delete linux-runner-base-go`. This is the irreversible step and buys
   back roughly 8 GiB. Rebuilding it is a documented but slow procedure
   (`docs/LINUX_BASE_IMAGE.md`).

**Rollback** at any point before step 7: revert the config commit,
re-provision, restart. The GitHub objects deleted in step 3 are recreated by
`fleet scale-sets provision`.

**The exception stands.** The `tart-fleet-canary` Linux VM on the mini is kept
for release promotion until the canary set moves too, so the mini is the
second Mac to complete this procedure, not the first.

### Proposed, not built: `fleet scale-sets retire`

Step 3 is the only step that leaves the fleet. The minimal command that would
close it, offered here as a proposal and deliberately **not** implemented in
the change that wrote this amendment:

```
fleet scale-sets retire NAME --config PATH [--scope SCOPE]     --confirm retire-scale-set --reason "<why>" --apply
```

It would reuse `provision.Recreater`'s existing `Delete` port and its
capability isolation, resolve the name exactly as `recreate` does, and differ
from `recreate` in three ways: it does not re-create the object, it refuses
unless the set is absent from every scope in the config (so a set still being
polled cannot be deleted under a running daemon), and it requires the set to
have reported no queued jobs. It is not built because the retirement is a
two-node, once-ever operation, and a destructive command that exists forever
to serve it is a worse trade than a documented manual step — the judgement
ADR 0054 already made when it refused to delete a scale set on park.

## Not addressed

Arch-floating aliases and the consumer label policy (#199) are unchanged;
this decision is about which node, not which label family.
