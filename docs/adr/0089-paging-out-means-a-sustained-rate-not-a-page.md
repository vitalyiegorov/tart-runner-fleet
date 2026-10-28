# ADR 0089: Paging out means a sustained rate, not a page

## Status

Accepted. Issue [#451](https://github.com/vitalyiegorov/tart-runner-fleet/issues/451).
Amends [ADR 0018](0018-second-pilot-elastic-host-envelope.md)'s "Swap pressure
is measured as a rate, not a level". The swap ceiling (`maxSwapUsedMb`) and the
fail-closed handling of an unmeasured rate do not change. This changes how
sensitive an admission guard is, so deploying it needs the owner's explicit OK.

## Context

ADR 0018 made the swap guard a conjunction: admission is refused with
`swap pressure` when swap used exceeds `maxSwapUsedMb` **and** the host is
paging out. "Paging out" was any non-zero swap-out rate between two
observations.

On 2026-10-28 (16:00–16:10Z, v0.1.796), ubuntu-vitalii had these readings:

- `swapUsedMiB` 3907 against a ceiling of 1024.
- `availableMemoryMiB` 23189.
- `swapOutRatePerSecond` 0.046, about one page every twenty seconds. `vmstat 5`
  printed `so=0` and `si` 40–60, so the host was reading swap back in, not
  pushing memory out.

The level clause was true all day, so the guard reduced to "did the kernel
write any page to swap since the last tick". The kernel's background swap-out
does that every few ticks. The gate log showed `closed openFor=26s`, then
`opened closedFor=2m16s`, then `closed openFor=7s`, then `opened
closedFor=1m32s`, and so on all day. Ubuntu, the largest amd64 node (24 cores,
60 GB), was gated for most of the day. Meanwhile 27 `trf-linux-amd64-2x4` jobs
that only Ubuntu and omarchy can serve queued for over 1.5 hours. omarchy
showed the same pattern with 3–16 GB of swap and plenty of memory available.

OPERATIONS.md already warned about this case ("the guard degenerates into 'did
this host page out at all since the last tick'"). Its advice, resizing the
ceiling per host, cannot help here. A ceiling of 4 GiB would have opened the
gate on 2026-10-28 and closed it again at the next burst of residue. The
defect is the rate test, not the level.

## Decision

1. **A host is paging out when its swap-out rate is at or above a threshold.**
   `executor.DefaultSwapOutPagesPerSecond` is 10 pages a second. The rate is
   already the average over the interval between two observations, so it is
   sustained over that window by construction: 10 pages/s across the default
   20-second poll is 200 pages, not one.
2. **Why 10.** It is about 200 times the 0.046 pages/s noise floor seen on
   2026-10-28. It is still far below a thrashing host, which pages out
   thousands of pages a second. 10 pages/s is 40 KiB/s with Linux's 4 KiB
   pages and 160 KiB/s with Apple Silicon's 16 KiB pages, sustained for a whole
   poll. Every refusal case in the existing tests (25 pages/s and up) still
   refuses.
3. **`guards.swapOutPagesPerSecond` overrides it.** The key is optional. When it
   is unset, or holds the default, it is omitted from a rewritten `fleet.json`,
   so an older strict release can still decode the file. It must be positive
   and finite. Zero is rejected, because it would round-trip back to the
   default, as with `minFreeInodePercent`. An operator who wants the old
   any-page behaviour can set a tiny value such as `0.001`. The published policy
   gains `hostPressureFloors.swapOutPagesPerSecond`.
4. **An unmeasured rate still fails closed.** With no prior sample, a clock that
   did not advance, or a counter reset by a reboot, the level blocks on its own,
   however much memory is available. This is ADR 0018's contract unchanged.

## Consequences

- A host carrying swap residue, with a background trickle of page-outs, admits
  work. A host paging out in earnest over its ceiling still refuses it.
- The guard does not consult available memory. That is deliberate: the
  `memory reserve` guard already refuses whenever available memory minus the
  request falls below `minAvailableMemoryMb`, and it runs before the swap
  guard. Making the swap guard also require low available memory would only
  ever weaken it: a host paging out hard while still above the floor would
  admit. Corroborating it with "abundant memory" in the other direction would
  add a second threshold for the same question.
- The deterministic simulation generates two new faults. `swap_residue` is
  ubuntu-vitalii's reading (four times the ceiling at 0.046 pages/s), and
  `swap_thrash` is the same level at 2000 pages/s. Property (v) asserts that no
  spawn is planned while a thrash lasts, reading the world's fault state rather
  than the guard's arithmetic. `TestIssue451ASwapTrickleDoesNotCloseTheGate`
  asserts that work starts during residue.
- Every node's policy digest changes once, because `hostPressureFloors` gained
  a field. During a rolling update, nodes on different releases report policy
  drift until all of them are upgraded (as with ADR 0083).

## Rollback

Revert the change. Alternatively, on a single node, set
`guards.swapOutPagesPerSecond` to a tiny positive value such as `0.001`. That
restores the any-page behaviour with no release.

## Not built

- **Memory PSI (`/proc/pressure/memory`) as a corroborator.** It is cheap to
  read, but it would add a snapshot field that must be plumbed through the
  domain, `fleet.v1` and telemetry. It is Linux-only, so the two probes would
  judge the same guard by different rules, and macOS has no equivalent file.
  The rate threshold alone separates the two readings the issue records by
  about two orders of magnitude. If a host is ever found thrashing below
  10 pages/s, PSI is the next signal to add, under its own ADR.
- **Hysteresis on the gate.** Requiring N consecutive refusals before closing,
  or N clean ticks before opening, adds state to a pure guard. The flapping in
  #451 came from judging noise as pressure, and the threshold removes that
  cause.
- **A rate in MiB/s.** The probes and `fleet_host_swapout_rate_pages_per_second`
  report pages. A byte rate would need the page size threaded through both
  probes, and it would rename a published figure.
