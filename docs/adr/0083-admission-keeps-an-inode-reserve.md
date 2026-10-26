# ADR 0083: Admission keeps an inode reserve

## Status

Accepted. Issue [#423](https://github.com/vitalyiegorov/tart-runner-fleet/issues/423).
Adds one guard beside `minFreeDiskGb`. It changes no existing guard.

## Context

On 2026-10-25, from about 15:20Z to 15:40Z, ubuntu-vitalii's root filesystem had
60,850,176 of 60,850,176 inodes in use. About 16.6M small files in the host
owner's own scratch directory had consumed them. The filesystem still had
29 GiB of free bytes, above the node's 20 GiB `minFreeDiskGb`.

Both host probes read free space in bytes only, and admission compared nothing
else. The node therefore reported `admissionAllowed: true`, READY and
"capacity available", and kept starting guests. Every guest then failed with
`No space left on device`:

- A `clone` operation failed eleven times.
- Three deregister `stop` operations were dead-lettered.
- CI jobs failed in four repositories.
- The actions-cache podman layer store became unreadable.

Cleaning the scratch data was an operator action. The fleet's defect was that it
could not see the condition: on ext4, free bytes say nothing about whether a
file can be created.

## Decision

1. **Both probes report inodes for the guest-storage filesystem.** The Linux
   probe runs `df -P -i <path>` beside `df -P -k <path>` and reads `Inodes` and
   `IFree` by header name. The macOS probe adds `-i` to its existing
   `df -k <path>` call and reads `iused` and `ifree` by header name. Their sum
   is the total.
2. **Inodes are a hard capacity fact, like bytes.** If the inode count cannot be
   read or parsed, the whole observation degrades (stale with a prior reading,
   unavailable without one), as an unreadable byte count already does. This
   includes GNU df's `-` for a count statfs does not know. An unread value never
   passes as a healthy one (AGENTS.md §4).
3. **A zero total means the filesystem has no fixed inode limit.** btrfs
   allocates inodes on demand, so `df -i` prints `0 0 0 -` for it, as it does
   for Omarchy's root. A zero total is reported as `totalInodes: 0`, and the
   inode floor does not apply to it. The probe reports a zero total only when
   df prints a numeric zero. It is never a stand-in for an unread value.
4. **`minFreeInodePercent` (default 5) is the floor.** Admission is refused with
   reason `inode reserve` when free inodes are below that percentage of the
   total. The guard runs after `disk reserve`, so a node out of both bytes and
   inodes is still reported as `disk reserve`. Existing runbooks read that name.
   A percentage, not an absolute count, because filesystems size their inode
   tables in proportion to their capacity. On ubuntu-vitalii, 5% is about 3.0M
   inodes. An ordinary ext4 root uses well under that share. The key is omitted
   from a rewritten `fleet.json` while it holds the default, so an older strict
   release can still decode the file.
5. **Operators see it by name.** `fleet status` prints `inodes N% free` (or
   `inodes n/a` when there is no limit) beside the free bytes. `fleet doctor`'s
   admission check reads `node is admitting no work: inode reserve (inodes 0
   free of 60850176 (0.0%), floor 5%)`. An admitting node includes the inode
   floor in its `nearest floor` margin. `fleet.v1` `hostPressure` gains
   `freeInodes` and `totalInodes`; no field is renamed. Prometheus gains
   `fleet_host_free_inodes` and `fleet_host_total_inodes`. The published policy
   gains `hostPressureFloors.minFreeInodePercent`.

## Consequences

- A node out of inodes stops admitting new work. Its session yield (ADR 0047)
  then hands its labels to siblings, as for any other refusal. Running guests
  are left alone. Pressure stops admission, never an active job.
- Every node's policy digest changes once, because `hostPressureFloors` gained a
  field. During a rolling update, nodes on different releases report policy
  drift until all of them are upgraded.
- One more `df` call per Linux tick, under the probe's existing deadline.
- The deterministic simulation generates an `inode_exhausted` fault. Property
  (u) asserts that no spawn is planned while the fault lasts. It reads the
  world's fault state, not the guard's arithmetic.

## Not built

- **An absolute inode minimum beside the percentage.** It would be a second
  setting for the same question. The percentage already scales with the
  filesystem.
- **Treating a zero total as exhausted.** That would refuse admission on every
  btrfs node permanently, for a limit those filesystems do not have.
