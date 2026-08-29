# ADR 0050: A container is shown the vector it was charged

## Status

Accepted. Completes
[ADR 0034](0034-a-node-serves-the-scale-sets-it-owns.md)'s container execution and the
telling half delivered for issue #296: the container was told its vector in the
environment, and is now also shown it in `/proc`.

## Context

A cgroup quota bounds what a process may **use**. It does not change what the
process can **see**.

Inside a container charged 2 CPUs on this fleet's Linux node, `/proc/cpuinfo`
lists all 24 host threads and `/proc/stat` carries a line for each. Every tool
that sizes a worker pool by counting CPUs therefore plans for a machine twelve
times larger than the one it has, and the quota it then runs into is not a
scheduler hint — it is a wall.

Production, 2026-08 (issue #291): three of four Playwright e2e shards timed out
in cascade, 12–17 minutes against 17 seconds for the lucky shard. The log said
`Running ... tests using 12 workers`. Playwright's default is
`os.cpus().length / 2`, `os.cpus()` counted 24, and twelve Chromium workers
against a 2-CPU quota is a self-inflicted stampede: every worker runs at a sixth
of the speed its event-driven assertions were written for, and `filechooser` and
visibility waits blow their budgets. The affected jobs were reverted to the
macOS labels and are still there.

Issue #296 was the same defect on the memory axis, and was answered by telling
the container its vector: `TRF_CPUS`, `GOMAXPROCS`, `JAVA_TOOL_OPTIONS`. That
fixed Go and the JVM and could not fix this, because a fact in the environment is
one a tool has to be **configured** to read, and the tools that broke are exactly
the ones nobody configured.

### Four mechanisms, measured

The issue proposed three fixes. Measured on node B against the production image,
in a container created with `--cpus 2` on a 24-thread host:

| mechanism | `nproc` | `availableParallelism()` | `os.cpus().length` |
|---|---|---|---|
| baseline | 24 | **2** | 24 |
| `--cpuset-cpus 0,1` | *fails outright* | | |
| `--cgroupns=private` | 24 | 2 | 24 |
| `taskset -c 0-1` | **2** | 2 | 24 |
| narrowed `/proc/cpuinfo` | 24 | 2 | 24 |
| narrowed `/proc/stat` | 24 | 2 | **2** |

Three of the assumptions this fleet was carrying turned out to be false, and each
would have cost a wrong build:

- **`availableParallelism()` was never broken.** libuv reads the CFS quota, so
  `--cpus 2` has always been honoured there. The proposal to fix it was a fix for
  nothing.
- **`taskset` does not fix the reported failure.** It corrects `nproc` and every
  other `sched_getaffinity` reader, which is real, and leaves `os.cpus()` at 24 —
  so the Playwright cascade would have survived a per-node disjoint-set cpuset
  allocator, which is what that option costs.
- **`/proc/cpuinfo` is not where the count comes from.** libuv counts the `cpuN`
  lines of `/proc/stat`. It is the file nobody would think of, and it is the only
  one that moves `os.cpus()`.

`--cpuset-cpus` remains impossible for a different reason: the cpuset controller
is not delegated to the rootless user slice, so `runc` cannot write it
(`cgroup.controllers` inside the container reads `cpu memory pids`).

## Decision

At create time the adapter generates two files from the host's own `/proc`,
truncated to the container's vector, and bind-mounts them read-only:

- `/proc/cpuinfo` — the first N processor blocks, unmodified. They are already
  numbered from zero in host order, so a prefix needs no renumbering and every
  field a tool reads stays true of cores the container is really running on.
- `/proc/stat` — the `cpuN` lines above N removed, and **everything else kept**:
  the aggregate `cpu` line, `btime`, `ctxt`, `intr`, `processes`,
  `procs_running`. None of those is a CPU count, and dropping any would break a
  reader for no gain.

The pair is keyed by width alone, because its content depends on nothing else.
Two containers of one profile share one pair, a restarted controller rewrites
what it already wrote, and nothing is per-instance — so nothing has to be cleaned
up when an instance ends.

**Every failure narrows nothing and reports no reason.** An unreadable host
`/proc`, a state directory that cannot hold a directory, a host with fewer cores
than the vector asks for: each returns no mounts, and the container starts seeing
the host exactly as it did for this fleet's whole life. That is a performance
defect. A container that will not start is a failed job. The trade is not close.

It is **both files or neither**. A container told two CPUs by one file and
twenty-four by the other is a worse place to debug from than one told
twenty-four twice.

A host smaller than the vector is **refused rather than padded**. Inventing a
core is a lie a tool would act on.

## Consequences

Playwright's default worker count on a `linux-2x4` container becomes 1 instead of
12, verified end to end in a real container against the production image. Every
`os.cpus()`-based autodetector — Jest's default `maxWorkers`, nx, anything using
`physicalCpuCount` — is corrected the same way, without the consumer changing a
line.

**The cost, stated plainly: the files never update.** Anything computing CPU
*utilisation* from two samples of `/proc/stat` inside the container reads zero.
`top`, `uptime` and `vmstat` were each run against a narrowed container and all
three work; they report idle.

This is a smaller loss than it looks, and the reason is worth writing down. Those
counters were the **host's**, aggregated across every tenant and every other job
on the box. They were never about this container, no fleet decision has ever read
them, and a job that trusted them was already being misled. The change replaces a
live host-shaped lie with a static container-shaped one — wrong in a way an
operator can predict, rather than wrong in a way that looks right.

The narrowing is disabled by an empty `VectorViewDir`, which is what every node
without a known state directory gets, and what every unit test that wires a
backend from a bare configuration gets. Those containers produce byte-identical
argument vectors to the ones they produced before this record.

`config.Config` gains `StateDir`, populated by the daemon from the directory the
configuration was loaded from and never serialised. It describes where a file was
found, not anything an operator writes down, so `Encode` has no counterpart and a
decoded configuration always carries the empty string.

## Not addressed here

**`nproc` still reports 24**, because it reads `sched_getaffinity` and no mount
changes that. `make -j$(nproc)` therefore still oversubscribes a container. Two
things would fix it and neither is free: `taskset` with a per-node disjoint-set
allocator (two containers pinned to the same cores contend while the rest idle,
so the allocator is not optional), or delegating the cpuset controller, which
needs root on every node and then makes `--cpuset-cpus` a one-line change. The
second is strictly better if the ops step is acceptable, and it also supersedes
this record's static files with real cgroup enforcement.

**lxcfs is not adopted.** It is the only option that keeps the counters live, and
it is an external dependency on every node whose `/proc/cpuinfo` masking keys off
`cpuset.cpus` — which is exactly what is not delegated here, so it is not
obviously a drop-in either. If cpuset delegation ever lands, lxcfs becomes worth
revisiting and this record's mounts become removable.

**Memory is not narrowed.** `/proc/meminfo` still reports the host's, and issue
#296's answer there was `JAVA_TOOL_OPTIONS` rather than a mount. The same
technique would work; no incident has asked for it, and `MemTotal` is read by
more things than a CPU count is.
