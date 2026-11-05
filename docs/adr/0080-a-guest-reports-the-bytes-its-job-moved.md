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
   - Linux guests (podman containers, and Tart Linux VMs before the amendment
     below) run `cat /proc/net/dev`.
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

## Amendment 2026-11-05: a Tart guest is read from the host, never by exec (issues #414, #478)

The first week measured about 3% of Tart jobs (macmini 17 of 672, macbook 19
of 523 in 24 hours; podman about 99%), nearly all `exec_timeout` and
`exec_failed`.

**Cause.** It is not the 5-second deadline. A read-only `tart exec` against a
healthy trf- guest answered in 0.8 s, and against a stopped VM it fails in
20 ms. ADR 0010 makes a Tart guest power itself off the moment its ephemeral
runner exits, which is before the drain has deregistered the runner and
confirmed its deletion, so the first-stop read finds a guest that is shutting
down (the exec hangs: `exec_timeout`) or stopped (`exec_failed`). A podman
container stays up until the daemon removes it, which is why those nodes
measure.

**The constraint.** `tart exec` rides the guest's vsock device, and repeated
vsock connections are what trap Virtualization.framework's VM helper
(`EXC_BREAKPOINT` on `com.apple.virtualization.virtio.vsock-device:0`, issues
#264 and #478). A probe every 5 s killed a scratch guest in 23 minutes, while
an identical guest with no vsock traffic lived past 45. The Mini and MacBook
run with `guestLivenessRefusals: -1` for that reason. So a fix that reads every
live guest every few seconds with `tart exec` (the first version of this
amendment) is rejected: it is new periodic vsock traffic on the hosts that
cannot take it.

**What was evaluated, in order.**

1. **A host-side counter: yes, and it is the decision.** Virtualization.framework
   puts each running guest on its own `vmenetN` interface on the host's NAT
   bridge (`bridge100`), and the host keeps byte counters for it. Read-only
   checks on macmini (macOS 26.5, tart 2.32.1) and the MacBook (macOS 27.2)
   showed it. `ifconfig bridge100` lists each guest's MAC in its address cache
   against the `vmenet` port it was learned on, and each running guest's
   `macAddress` in its `config.json` matched one entry. `netstat -ibn` has
   one `<Link#N>` row per `vmenet`, and its `Obytes` (host to guest) was
   the large number (1.8 GB against 32 MB of `Ibytes` on a 14-minute-old guest),
   as expected of a runner that mostly downloads. No command
   touched a guest.
2. **The guest reports its counters as it goes down: not built.** The shutdown
   is the bootstrap helper's supervisor script (`internal/guestbootstrap`),
   which is compiled into `/usr/local/libexec/tart-runner-fleet-bootstrap` in
   the immutable base image (ADR 0010): printing counters to the console before
   `shutdown` needs a base image rebuild on both platforms. The fleet could
   inject a watcher at launch with the one bootstrap `exec` it already makes,
   but its output could go only to the serial console, which is opt-in
   (`linuxSerialLogDirectory`, ADR 0046), exists only for Linux guests (the tart
   adapter passes `--serial-path` for them alone), and would have to be parsed
   back out of a log file racing a guest that is powering off. macOS guests
   have no such channel. It is more machinery for a worse answer than (1).
3. **Keep the old behaviour and report: not needed**, since (1) works.

**Decision.** A Tart guest is measured from the host's counters for its virtual
NIC. The daemon never execs into a Tart guest for traffic.

- Every 5 seconds the daemon makes one host read for all live guests: `ifconfig
  -l`, `ifconfig <bridge>` for each `bridge*`, and `netstat -ibn`, no guest
  involved. A guest's MAC comes from its `config.json`, the file the fleet
  already reads for its power reading. The guest's counters are the `vmenet` row
  seen from the guest: receive is the host's `Obytes`, transmit its `Ibytes`.
  Parsers are pure (`domain.ParseBridgeGuests`, `domain.ParseVirtualNICs`).
- The latest reading per instance is kept in memory. When the drain records the
  job it stores that reading as measured, with `sampledAt` the time of the
  reading. The drain runs no command and reads no guest, so it can neither fail
  nor wait on this. The interface disappears with the powered-off guest, so the
  last reading is a lower bound that misses at most the final five seconds.
- A host that cannot be read keeps the previous readings and is warned about
  once, when it starts to fail and when it recovers. A guest the host never saw
  (the job ended before the first read, or the daemon restarted mid-job) is
  recorded as unmeasured with the new closed reason `no_reading`, never as zero.
  No migration: the column is free text, and `fleet.v1` only gains a value.
- Podman nodes are unchanged: a container is up until the daemon removes it, and
  `podman exec` is not vsock, so the one end-of-job exec read stays.
- Draining guests are read too (the interface may still be up); the drain's
  skips (escalated stop, guest proved unresponsive) are unchanged and still leave
  no row.

**Limits.**

- Counters are what crossed the NIC, so ARP, DHCP and bridge-flooded broadcasts
  and the Ethernet headers are counted. That is a few kilobytes against jobs of
  megabytes, and the guest's own `/proc/net/dev` counts headers too.
- A job shorter than the first read, and a job running across a daemon restart,
  are unmeasured. Readings are not persisted.
- The mapping relies on `ifconfig`/`netstat` output and on the guest being on a
  bridge. If either changes, the parsers refuse the output and every job is
  unmeasured (one warning), never zero.
- macOS Tart guests use the same NAT attachment and should appear the same way;
  none was running on macmini when this was checked, so that is read from the
  code, not observed.
- Nothing about the guest image changes; no base image needs a rebuild.

Not done: any periodic `tart exec`; lengthening the deadline (the guest is gone);
a guest-image change to print counters before power-off (see above).

## Not built

- Host-side eBPF or nftables accounting.
- Per-instance gate attribution.
- Retention pruning of `guest_traffic`. It is one small row per job, like the
  instance rows it sits beside.
