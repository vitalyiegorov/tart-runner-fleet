# ADR 0080: A guest reports the bytes its job moved

## Status

Accepted. Issue [#414](https://github.com/vitalyiegorov/tart-runner-fleet/issues/414).
It complements the declared tool-drift check proposed in PR #418 (ADR 0079 on
that branch). That check catches drift the fleet's own configuration can
describe. This one measures what actually crossed each guest's network
interface, so a consumer's pin bump that nobody mirrored into config still
shows up as bytes.

## Context

On 2026-10-24 the home uplink was saturated by CI. Finding out why took
`podman exec` into three containers and a five-second `podman stats` sample by
hand. The causes were a 1.1 GB pnpm store restored from GitHub's cache, a 57 MB
Node archive per job, and 315 MB of Maestro per macOS shard. The fleet had no
record of how many bytes any job had moved, and no way to say which repository
or workflow moved the most.

The obvious host-side sources do not cover the whole fleet. `podman stats`
covers containers but not Tart VMs. Tart has no per-VM network counter.
eBPF or nftables accounting would be a privileged host subsystem on each OS.
However, every guest already answers `exec <instance> …`: readiness, liveness
and bootstrap all go through that one neutral argument vector on both
backends.

## Decision

**Read the guest's own interface counters once, at the end of its job, over the
existing `exec` path. Store at most one row per instance and report it by repository,
workflow and profile.**

1. **Source: the guest's own kernel.**
   - Linux guests (podman containers and Tart Linux VMs) run `cat /proc/net/dev`.
   - macOS guests run `netstat -ibn`.
   - The result is the sum of receive and transmit bytes over every interface
     except loopback. On macOS only the `<Link#N>` rows count, because the
     per-address rows repeat the same counters. The byte columns are found
     from the right by their position in the header, because the Address
     column can be blank.
   - Both parsers are pure (`domain.ParseGuestNetwork`). Output they cannot
     read is an error, never zero.
2. **When: once.** The drain samples at the start of its first stop of the
   guest:
   - For an ordinary drain, that is after the runner is deregistered and its
     deletion confirmed, so the job is over.
   - For an occupancy-budget reclaim, it is before the stop that ends the job.
   - The sample has a 5-second deadline. It returns nothing and cannot fail the
     drain.
   - It is skipped on an escalated stop (a guest that would not power down is
     not asked again) and on a guest the fleet has already proved
     unresponsive.
   - Each guest boots for exactly one job, so its counters since boot are that
     job's traffic. See the amendment below for why this read alone is not enough
     on Tart.
3. **What is recorded.** Migration 14 adds `guest_traffic`, with at most one
   row per instance. An instance that was never sampled has no row: its guest
   died first, its drain only escalated, or it was reclaimed as unresponsive
   (see Limits). Each row has:
   - Identity: repository, platform, profile, start time and sample time,
     copied from the instance at sample time.
   - The counters, or `measured=0` with a closed reason (`exec_failed`,
     `exec_timeout`, `unreadable`, `no_command`).
   - The first measured sample wins. A drain retry cannot double count, and a
     later real reading replaces an earlier failed one.
   - At report time each row is joined to `runner_demands` by runner name. That
     table already holds the scale set, run ID, workflow ref and job name from
     the scale-set messages, and it describes the job GitHub actually brokered
     to this runner, not the demand it was spawned for. No GitHub API call is
     added.
4. **Where it is read.**
   - `fleet traffic --since 24h [--output json]`, through a new read-only
     admin-socket route `/v1/traffic`. It returns per-job rows and totals
     grouped by repository, workflow (the workflow file without its `@ref`,
     so every pull request of one workflow adds up) and profile, largest
     first.
   - Unmeasured jobs are counted and their byte fields are absent. An
     unreadable store answers 503, never an empty list.
   - A response holds at most 1500 jobs and says `truncated` when the window
     had more.
   - Prometheus counters on the daemon's metrics endpoint:
     `fleet_guest_network_receive_bytes_total`, `…_transmit_bytes_total` and
     `fleet_guest_network_jobs_total{measured}`, labelled
     `{platform, profile, repo}`. There are at most 512 series. Repositories
     come from the configured targets, so a real node is far below that.
5. **The LAN share.** A guest's receive counter includes what it read from the
   node's LAN Actions cache (ADR 0078). `fleet cache-gate` now counts the bytes
   it proxies, as `fleet_cache_gate_bytes_total{upstream=cache|results,
   direction=to_guest|from_guest}`. The counters are served only when
   `--metrics-listen` names a loopback address, never on the address guests
   reach. An estimate of WAN bytes is guest receive minus gate
   `cache/to_guest` over the same window.

## Limits, stated rather than hidden

- **Counters reset when a guest reboots.** A job that reboots its guest is
  under-counted to the bytes since that boot.
- **A guest that dies takes its counters with it.** If the guest dies before the
  sample, or the fleet reclaims it as unresponsive, the job has no row, or an
  unmeasured row. Its bytes are lost, not zero.
- **Exec traffic is not on the guest's network interface**, so the sample
  does not measure itself. Everything the guest does on its interfaces is
  counted, including traffic to the host and the LAN, which is why the gate
  share exists.
- **On macOS, bridges and tunnels inside the guest could count bytes twice**
  (for example an in-guest VPN on top of `en0`). The stock images run none.
- **Gate bytes are not attributed per instance.** Rootless pasta presents every
  container from the host's own address, so a source IP does not identify a
  guest. Attribution would need a per-guest token or port. The gate totals are
  node-wide, so WAN = receive − cache is a node-level estimate per window, not
  per job.
- The deploy unit for the gate passes `--metrics-listen 127.0.0.1:3019`. A node
  installs that unit only after its `fleet` release understands every flag in it,
  or the gate exits with its usage text and systemd restarts it in a loop.

## Testability

The parsers (with real `/proc/net/dev` and `netstat -ibn` shapes, address-less
rows, per-address duplicates, and every unreadable shape), the drain's sample
point and its skips, the recorder's outcome classification including the
timeout, the store's first-measured-wins rule and job join, the endpoint's
window and failure handling, the summarizer, the counters and their bounds, the
CLI in both modes, and the gate's counters and loopback-only listener all have
unit tests. The simulation is not extended. This change adds no admission rule,
state or axis to `internal/scheduler`, so the corpus digests must not move.

## Amendment 2026-11-05: a live guest is read while its job runs (issue #414)

The first week measured about 3% of Tart jobs (macmini 17 of 672, macbook 19
of 523 in 24 hours; podman about 99%), nearly all `exec_timeout` and
`exec_failed`.

**Cause.** It is not the 5-second deadline. A read-only `tart exec` against a
healthy trf- guest answered in 0.8 s, and against a stopped VM it fails in
20 ms. ADR 0010 makes a Tart guest power itself off the moment its ephemeral
runner exits, which is before the drain has deregistered the runner and
confirmed its deletion, so the first-stop read finds a guest that is shutting
down (the exec hangs: `exec_timeout`) or stopped (`exec_failed`). Observed on
macmini: one guest's sample began at 04:36:56.5Z and timed out, and `tart
list` showed that guest `stopped` at about 04:36:58Z. A podman container stays
up until the daemon removes it, which is why those nodes measure.

**Decision.** Keep the end-of-job read and its 5-second deadline, and add a
best-effort sweep while the job runs.

- Every 15 seconds the daemon reads each live guest in `online_idle`,
  `assigned` or `running` through the same `exec` path and keeps the latest
  successful reading per instance in memory. Draining guests are not read.
- At the end of the job, a successful read is the job's. If it fails, the last
  kept reading is recorded as measured, with `sampledAt` the time of that
  reading. Counters only grow, so it is a lower bound that misses at most the
  traffic of the final interval, such as a closing cache upload.
- The sweep runs in its own goroutine, bounded by the same deadline per read,
  and touches neither the store nor the metrics. It cannot fail or hold a drain.
- Readings are not persisted: a daemon restart forgets them, and those jobs are
  recorded as before. They are dropped when the instance is recorded or is no
  longer live.
- Not done: lengthening the deadline (the guest is gone, so a longer wait only
  holds teardown), and changing the guest image to delay its power-off.

## Not built

- Host-side eBPF or nftables accounting.
- Per-instance gate attribution.
- Retention pruning of `guest_traffic`. It is one small row per job, like the
  instance rows it sits beside.
