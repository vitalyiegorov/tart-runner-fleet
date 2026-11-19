# ADR 0102: Swap pressure below a hard rate is corroborated by memory stall

## Status

Accepted, 2026-11-19. Issue [#480](https://github.com/vitalyiegorov/tart-runner-fleet/issues/480).
Amends [ADR 0089](0089-paging-out-means-a-sustained-rate-not-a-page.md): on a
host that reports a memory stall, a swap-out rate between the paging threshold
and a new hard rate is pressure only when the stall corroborates it. Narrows
[ADR 0018](0018-second-pilot-elastic-host-envelope.md)'s fail-closed contract in
exactly one case: an unmeasured swap-out rate beside a memory stall *measured*
below the threshold. The swap ceiling, the paging threshold, every rate at or
above the hard rate, and every host that reports no stall keep ADR 0089's rule
byte for byte. This changes how sensitive an admission guard is, so deploying
it needs the owner's explicit OK.

## Context

ADR 0089 refuses admission with `swap pressure` when swap used exceeds
`maxSwapUsedMb` **and** the swap-out rate is at or above
`swapOutPagesPerSecond` (default 10), or is unmeasured.

### Cold residue closes an idle node

At 2026-11-19 08:56Z, ubuntu-vitalii (ceiling 1024 MiB) read:

- `swapUsedMiB` 8191 of 8191: swap completely full.
- `swapOutRatePerSecond` about 50, five times the threshold.
- `availableMemoryMiB` 38402, load 13.
- `/proc/pressure/memory` `some avg10=0.54` (Omarchy, in the same state, read
  0.18).

Both clauses held, so the gate stayed closed and the node ran no guest. The
same shape closed Omarchy for more than 80 minutes on 2026-11-17 and flapped
Ubuntu on 2026-11-16 (see the issue).

Neither clause measures what the guard is for, on a host whose swap is full of
cold pages:

- **Swap used** never drains. Linux keeps a page in swap until something faults
  it back in, so a burst from non-fleet work leaves residue indefinitely.
- **The swap-out rate** does not stop either. With swap full, the kernel keeps
  rotating cold anonymous pages through it as reclaim ages the LRU, at tens of
  pages a second, while no task waits on any of it.

Memory pressure stall information (PSI, Linux 4.20+) measures the waiting
directly. The `some` line of `/proc/pressure/memory` is the share of wall time
in which at least one runnable task was stalled on memory: reclaim, swap-in,
refaulting its working set. `avg10` is that share over the last ten seconds.
At 0.54%, almost nothing waited.

### Real paging can also read a low stall

At 2026-11-19 10:26Z, Omarchy (ceiling 2048 MiB) read `swapUsedMiB` 6321,
`swapOutRatePerSecond` **9563**, `availableMemoryMiB` 12077, load 8.8, while a
7.8 GB process trained a model. PSI read `some avg10=1.67 avg60=1.20`,
`full avg10=0.98`. The host was paging out in earnest, and yet the stall was
low: the trainer streamed through its memory and the kernel kept up, so few
tasks stalled long. A rule that let any stall under 10% exempt the guard would
have admitted guests onto that host. A stall is evidence about the trickle,
not a veto over a high rate.

Other recorded thrash that day ran at ~15000 pages/s (Omarchy) and ~5700
pages/s (MacBook). Cold residue rotated out at about 50 pages/s.

On 2026-11-19 at about 10:14Z the owner set `swapOutPagesPerSecond: 1000` on
ubuntu-vitalii and Omarchy as a stopgap. ADR 0089 deferred PSI as "the next
signal to add, under its own ADR" if the rate alone ever misjudged a host. It
has, in both directions.

## Decision

1. **A new host fact, advisory.** The snapshot carries `MemoryStallPercent` and
   `MemoryStallObserved`, end to end, exactly as the swap-out rate is carried.
   The Linux probe reads the `some` line's `avg10` from `/proc/pressure/memory`.
   A missing, unreadable or garbled file (no PSI in the kernel, `psi=0`,
   anything outside 0–100) is `observed=false`. It never degrades or fails the
   observation, and it never carries an earlier reading forward. macOS has no
   equivalent and leaves it unobserved.
2. **The guard.** With `soft` = `swapOutPagesPerSecond`, `hard` =
   `swapOutHardPagesPerSecond` and `T` = `memoryStallPercent`:

   ```
   corroborated ⇔ ¬stallObserved ∨ stall ≥ T

   swap pressure ⇔ swapUsed > maxSwapUsedMb ∧ (
         rateObserved ∧ rate ≥ soft ∧ (rate ≥ hard ∨ corroborated)
       ∨ ¬rateObserved ∧ corroborated )
   ```

   - **At or above the hard rate**, the stall is not consulted: the host is
     paging out in earnest (Omarchy, 9563 pages/s at 1.67%).
   - **Between soft and hard**, the trickle band, an observed stall decides.
     Cold residue (Ubuntu, 50 pages/s at 0.54%) admits; the same rate at a
     stall of `T` or more refuses.
   - **Unmeasured rate**: the first tick after a daemon restart. A stall
     measured below `T` is positive evidence the level is residue, which an
     absent rate never was, so it admits. If the restart lands inside real
     thrash with a low stall, the next tick measures the rate and refuses: one
     poll of exposure, against latching a recovered host off on every restart.
   - **Unobserved stall**: exactly ADR 0089.
3. **`swapOutHardPagesPerSecond`** is a new optional top-level key beside
   `maxSwapUsedMb` and `swapOutPagesPerSecond`. It defaults to 1000, must be
   finite and at least `swapOutPagesPerSecond`, and is omitted from a rewritten
   `fleet.json` while it holds the default. Equal to `swapOutPagesPerSecond`
   means the stall never exempts a measured rate.
4. **`memoryStallPercent`** configures `T`, the same way. It defaults to 10 and
   must be in (0, 100]. The published policy gains
   `hostPressureFloors.swapOutHardPagesPerSecond` and
   `hostPressureFloors.memoryStallPercent`.
5. **Why 1000 and 10.** 1000 pages/s is twenty times the cold-residue rotation
   and below every recorded thrash (5700, 9563, 15000), and it is the line the
   owner's stopgap already trusted. The cold-residue stalls were 0.54% and
   0.18%. Android's `lmkd` treats roughly 7% `some` stall as medium pressure,
   and systemd-oomd acts at 60% sustained for 30 s; 10% sits well above residue
   and below either. Neither figure is calibrated against a thrash *inside* the
   band: none has been recorded there yet.
6. **Plumbing.** `fleet.v1` `hostPressure` gains `memoryStallPercent` and
   `memoryStallObserved` (additive). Metrics gain
   `fleet_host_memory_stall_percent` and `fleet_host_memory_stall_observed`.
   `fleet status` prints `swap 8191 MiB (paging 50/s, stall 0.54%)` and prints
   no stall on a host that does not report one. A gate closure logs
   `memoryStall=<n>%` or `memoryStall=unobserved`.

### What stays closed

- Over the ceiling at or above the hard rate, whatever the stall reads
  (Omarchy's 9563 pages/s at 1.67%, the 15000 pages/s thrash).
- Over the ceiling in the trickle band at a stall of `T` or more.
- Any host that does not report a stall: macOS (the MacBook's 5700 pages/s),
  and Linux without PSI. On such a host 8191 MiB at 50 pages/s still refuses,
  and an unmeasured rate still refuses on the level alone.
- An unmeasured rate beside a stall at or above `T`.

The stall never closes the gate by itself: under the ceiling, or below the
paging threshold, admission is allowed whatever the stall reads. The `memory
reserve` guard is untouched and still runs first.

## Consequences

- Ubuntu and Omarchy admit work while carrying cold swap without the stopgap.
  Once this ships, both nodes should drop `swapOutPagesPerSecond: 1000` and go
  back to the defaults (10, with the hard rate at 1000). Keeping the stopgap
  would set soft equal to hard and switch the stall off for them.
- The two probes now judge the trickle band with different evidence on Linux
  with PSI and on macOS. That is the cost ADR 0089 named. It is bounded: an
  unobserved stall reduces to ADR 0089 exactly, so macOS behaviour is unchanged.
- Every node's policy digest changes once, because `hostPressureFloors` gained
  two fields. Mixed-release nodes report policy drift until all are upgraded.
- A node whose file sets `swapOutPagesPerSecond` above 1000 without a hard rate
  fails validation after the upgrade and must state
  `swapOutHardPagesPerSecond` too. No deployed node does today.
- Evidence: `TestSwapGuardAdmitsColdResidueTheKernelIsNotStalledOn`,
  `TestSwapGuardAdmitsColdResidueBeforeTheRateIsMeasured`,
  `TestSwapGuardStillRefusesWhatStallCorroborates`,
  `TestSwapGuardALowStallNeverOverridesAHardRate` (the Omarchy sample),
  `TestSwapGuardStallDecidesTheTrickleBand`, the replay
  `TestIssue480ColdSwapResidueKeepsTheGateOpen` (the real Linux probe over five
  ticks, the first without a rate) and `TestIssue480WithoutPSITheRuleIsUnchanged`,
  and the simulation `TestIssue480ColdSwapResidueKeepsTheNodesOffer`: under
  the new `swap_cold_residue` fault no refusal episode begins, no job goes to
  the sibling, and every job starts inside the window. The `swap_thrash` fault
  now pages out at 2000 pages/s at Omarchy's 1.67% stall, so property (v)
  proves the hard rate alone keeps a thrashing host closed.

## Rollback

Revert the change. On one node, setting `swapOutHardPagesPerSecond` equal to
`swapOutPagesPerSecond` restores ADR 0089's rule for every measured rate with no
release; adding `memoryStallPercent: 0.001` restores it for an unmeasured rate
too.

## Not built

- **PSI alone as the gate**, without the swap level and rate. Omarchy's
  9563 pages/s at 1.67% shows the stall can miss real paging, and gating on it
  alone would also refuse a host stalled on memory with no swap configured.
  Deferred until a stall threshold is calibrated against real thrash.
- **A stall that exempts any rate.** That was this ADR's first draft, and the
  Omarchy sample refutes it.
- **`full` instead of `some`.** `full` (every non-idle task stalled) reacts
  only once the host is already unusable; Omarchy's training run read 0.98.
- **`avg60` or `avg300`.** The guard is evaluated every poll, and the swap-out
  rate is already an average over that interval. A longer stall window would
  keep the gate closed for minutes after a burst ended, which is the latching
  this ADR removes.
- **A macOS stall.** `kern.memorystatus_vm_pressure_level` is a coarse level,
  not a share of time, and every #480 misfire was on a Linux node.
