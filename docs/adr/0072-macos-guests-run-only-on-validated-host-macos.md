# ADR 0072: macOS guests run only on a validated host macOS

## Status

Accepted 2026-10-21. Closes the fleet-side half of
[issue #363](https://github.com/vitalyiegorov/tart-runner-fleet/issues/363).
Extends the local-host check of
[ADR 0069](0069-local-macos-shares-are-checked-before-startup.md) by one fact.

## Context

From 2026-10-10, iOS jobs on every consumer (`rnw-community`, `budgie`,
`suuudokuuu`) began failing mid-job with:

```
An error occurred trying to start process
'/Users/admin/actions-runner/externals/node24/bin/node' ... Exec format error
```

Issue #363 established the mechanism, with the guest still alive:

- The bundled Node starts, runs a JavaScript action, and exits. Later in the
  same job the next start of the same file gets `ENOEXEC`.
- At that moment `file` reports `data`, the first bytes read as `0x00`, and
  the SHA-256 is `f7b04090…` instead of the image's `ee6fb0e0…`. Size, inode
  and mtime are unchanged, so nothing in the guest wrote the file.
- A `cp` of the file inherits the zeros. After `sudo purge` (which drops the
  guest's unified buffer cache) the same path reads `ee6fb0e0…`, is
  `Mach-O 64-bit executable arm64`, and executes. **The virtual disk is
  intact. The guest's in-memory page-cache copy is zero-filled.**
- Every one of these guests ran on the MacBook, the only host on
  **macOS 27.0.1**. The Mac mini (26.5) and the Mac Studio (15.7.4) launch
  clones of the same base, with identical `tart run` arguments, and have never
  shown it across hundreds of guests.
- Throwaway clones on the MacBook reproduce it with no fleet job running.
  Changing the disk caching mode does not prevent it, so this is not a Tart
  disk option.

mobile-ci #168 added the in-job diagnostic that captured the same signature
from GitHub's side. From 2026-10-20 it ran on every failure and recorded the
zeroed header, `fresh copy exec exit status: 126`, and a passing exec after
`purge` in 7 of 7 jobs.

Issue #363 mitigated by configuration: on 2026-10-15 the MacBook dropped to one
macOS guest at a time, then to one VM of any kind. **That did not stop it.**
From 2026-10-16 to 2026-10-21, 27 more jobs failed the same way. Their
start/end intervals never overlap: they are one serial lane, which is the
MacBook's one-VM configuration. In the same windows the Mac mini's
`trf-maestro-*` guests and the Mac Studio's `trf-macos-4x7-*` guests ran
concurrently and passed. Concurrency was a trigger, not a necessary condition.
Host memory reclaim of guest RAM is the leading explanation, and it is
Apple's to fix.

## Decision

**A node whose host macOS major is 27 or later does not start with macOS
guests enabled.** `app.ValidateLocalHost` is the one place this rule lives.
After the share check of ADR 0069, it reads the host's `kern.osproductversion`
through an injected port and refuses when the major is at or above
`FirstUnvalidatedGuestHostMacOSMajor` (27). The error names
`macosBurst.enabled`, the host version, issue #363, and the repair: set
`macosBurst.enabled` to `false` on that node.

It runs where ADR 0069's check already runs: before the daemon opens durable
state in every mode, and in single-file `fleet config validate` on the owning
Mac. Linux nodes and nodes with macOS guests disabled read nothing.

An unreadable or unparsable host version fails closed. An unknown host is not
a validated one.

## What was deliberately NOT built

- **No knob.** Lifting the bar to a fixed 27.x is a one-constant code change,
  made with issue #363's two-clone reproduction passing on that host. A
  configuration override would let a node re-enable the defect with nothing
  but an edit, which is how the MacBook went back to failing after each
  partial mitigation.
- **No runner preflight that executes the bundled Node before the job.** In
  every captured case Node was intact at boot and ran several actions before
  its pages went bad. A preflight would pass on exactly the guests that
  later fail.
- **No architecture or checksum gate on the runner payload in the daemon.**
  The payload in the base was byte-identical across hosts (`ee6fb0e0…`, arm64)
  and the purge proof shows the disk held it intact. `docs/BASE_IMAGE.md` now
  verifies the tarball's SHA-256 and the bundled Node's arch and execution at
  seal time. That is image hygiene, not this fix.
- **No admission gate per platform.** Refusing startup is what ADR 0069 does
  for a configuration that cannot work on its host. The repair keeps the
  node's Linux guests by disabling only its macOS guests.
- **No simulation fault.** Like ADR 0069's check, this runs before the daemon
  loop the simulation drives. #385 removed a startup "simulation" for exactly
  that reason.

## Consequences

A Mac on macOS 27 with `macosBurst.enabled: true` refuses to start, including
in observe mode, and `fleet config validate` on it exits 1 with the repair.
Disable macOS guests on that node before it takes this release. Otherwise the
candidate's own `fleet config validate` refuses the node's configuration, the
update does not activate (ADR 0011), and the node stays on its previous
release, which still runs macOS guests.

macOS iOS demand moves to the nodes on validated host macOS through shared
labels (ADR 0034). The fleet loses the MacBook's macOS slot, which was failing
the jobs it ran.

There is no schema, fleet.v1, migration, or scheduler change. Reverting
restores the mid-job failures.

**Do not upgrade the Mac mini or the Mac Studio to macOS 27** until a fixed
version has passed the reproduction. This check would then take their macOS
guests off the air, which is the intended outcome and also the whole fleet's
iOS capacity.
