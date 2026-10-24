# Agent runbook

This is the shortest safe path for a coding or operations agent to inspect and
support a production Tart Runner Fleet. The local versioned API is the source of
truth. SQLite, log scraping, process presence, and an unavailable API are not
substitutes for a coherent snapshot.

## Establish the installed identity

```sh
ROOT="$HOME/Library/Application Support/tart-runner-fleet"
STATE="$ROOT/state"
FLEET="$ROOT/current/fleet"
ENDPOINT="unix://$STATE/fleetd.sock"

test -x "$FLEET"
readlink "$ROOT/current"
cat "$STATE/installed-generation.json"
```

`current` is atomically updated only after a candidate becomes ready. It is a
convenience path for operators and agents; launchd remains pinned to an exact
immutable release. The link target and `releaseDir` in
`installed-generation.json` must agree. Never rewrite either by hand.

## Capture one coherent snapshot

```sh
"$FLEET" status --endpoint "$ENDPOINT" --require-ready --output json
"$FLEET" doctor --endpoint "$ENDPOINT" --output json
"$FLEET" queues --endpoint "$ENDPOINT" --output json
"$FLEET" instances --endpoint "$ENDPOINT" --output json
"$FLEET" operations --endpoint "$ENDPOINT" --output json
"$FLEET" observations --endpoint "$ENDPOINT" --output json
```

A command exit status 0 is healthy. Exit 4 means unavailable. Exit 5 means a coherent but
degraded snapshot. Exit 4 or 5 never means zero demand and never authorizes VM
cleanup. Preserve stdout as JSON and diagnostics as stderr.

## Monitor active work

During a rollout, incident, or real-load proof, sample every two minutes and
report only transitions or anomalies. Compare the current snapshot with the
previous snapshot rather than emitting repeated healthy messages. Track:

- controller version, mode, readiness, and observation freshness;
- queued jobs by profile and age, plus admission reasons. On a node serving
  more than one GitHub scope, read age BESIDE occupancy: a scope whose queue is
  ageing while every instance in `instances` belongs to another scope is the
  2026-09-21/22 shape, and ADR 0057 says the next freed slot is that scope's.
  Three conditions qualify that before you report anything: the two demands
  must ask for the **same profile**, neither may be separated from the other by
  a demand of the **other platform** (the aged band's barrier, ADR 0049), and
  the incumbent must not hold a higher **effective tier** (ADR 0037, declared
  tier plus escalation). With all three satisfied, a wait longer than one job on
  that node is a defect to report with the queue rows and the instance list, not
  a queue to clear. **Read occupancy from the VECTOR, not from what looks busy:
  a scope whose only instance is `online-idle`, `draining`, `deregistering` or
  `stopping` is still on that core and still holds its share of the node (ADR
  0057's amendment, issue #350). `fleet instances` shows the state; a guest in
  any of them counts;**
- instances by lifecycle state and owned Tart identity;
- retrying or dead operations and their bounded, redacted error class, read
  from `operations.failures` (`kind`, `code`, `count`, `attempts`) rather than
  from the counts alone; a cleanup stuck on `deregister:runner_busy` is GitHub
  refusing to release a runner it still considers busy, while
  `deregister:runner_forbidden` or `deregister:runner_scope_unresolved` is a
  fleet-side permission or configuration fault. Owned runner cleanup retries
  indefinitely by design (ADR 0007), so a high `attempts` value is a signal to
  read the code, not evidence that retrying is broken. Once an operation is dead,
  `operations.deadLetters` names its ID, owning instance, closed failure code,
  attempt count, and whether the resource is parked;
- CPU, memory, swap, disk floor, and Linux/macOS exclusivity;
- updater generation, last exit, and whether cleanup reached zero.

Use `tart list`, GitHub runner/job state, launchd, and bounded logs only to
cross-check a specific anomaly. Never infer idle from a failed GitHub query or
an unavailable daemon.

## When a guest dies, look at the host's crash reports first

`instance guest silent` / `instance guest unresponsive` /
`instance reclaimed because its guest stopped answering` mean the node's
liveness probe was refused repeatedly and the fleet reclaimed the vector. Since
issue #264 the first reflex is not the guest — it is Apple's per-VM host
process. On every episode between 2026-08-17 and 2026-08-24,
`com.apple.Virtualization.VirtualMachine` crashed (SIGTRAP,
`EXC_BREAKPOINT`, identical offset `0x3a75f8`) seconds before the probes began
refusing, killing the VM with zero guest-side output.

```sh
ls -lt ~/Library/Logs/DiagnosticReports/com.apple.Virtualization.VirtualMachine-*.ips | head
```

A `.ips` whose timestamp falls inside the silence window is the root cause;
attach it to the incident and stop investigating the guest. The serial console
(`linuxSerialLogDirectory`, ADR 0046) is what exonerates the guest: a kernel
panic or OOM prints to `hvc0`, and an absent trace plus a matching crash report
closes the question. The fleet's reclaim itself needs no post-mortem — it acted
on genuine evidence within one liveness window.

## Read the three power lines

The daemon's stderr log names every destructive recovery it plans and every
premise it withdraws. Three lines belong to one class and are read together:

```sh
grep -E 'recovery drain planned|power premise retracted|power state unreadable'   "$STATE/fleet-authority.stderr.log"
```

- **`instance recovery drain planned`** with `cause="vm powered off"` is a
  destructive decision resting on one backend reading. Never rate limited: a
  storm of them is the artifact, not noise (ADR 0042).
- **`instance power premise retracted by its own drain`** is the fleet catching
  itself. `stoppedReadings` climbing without ever resetting means the reading is
  sustained rather than flickering, and a sustained reading is not something a
  corroboration window can filter.
- **`instance power state unreadable`** is the one that names a cause. `reason`
  is the errno class the backend hit reading the VM's configuration and
  `readLatency` is how long it took; nothing is planned on such a reading, and
  the instance goes on charging the host until a bound the fleet owns expires
  (ADR 0044). A refused open fails in microseconds; a starved one takes as long
  as the host is busy. **Capture both numbers with the host's load and open-file
  counts (`sysctl kern.num_files kern.maxfiles`) while the condition is live** —
  the trigger has survived three investigations and this is the evidence that
  ends it.

A fourth line covers every other withdrawn decision:

```sh
grep 'instance drain aborted by fresh evidence' "$STATE/fleet-authority.stderr.log"
```

- **`instance drain aborted by fresh evidence`** names the `phase` that planned
  the drain and the `evidence` that stopped it. `evidence=runner_busy` means a
  job is executing on this instance's own runner. It is never the spawning
  demand running on a sibling VM, because drain guards check the runner
  (ADR 0074). If one instance keeps repeating a plan and abort pair, report it.
  Never deregister the runner by hand.

None of the three authorizes cleanup by hand.

## Understand the updater

The updater runs once every five minutes and exits. `launchctl` showing the
updater as `not running` is expected when the last exit status is 0 and its
program path belongs to the committed generation. The updater waits for a
quiescent fleet, verifies release identity and checksums, validates config,
uses bounded launchd bootstrap recovery, and gives production activation a
five-minute readiness budget. It atomically commits or restores the complete
generation.

Treat the updater as current only when `installed-generation.json`, the updater
plist, and `launchctl print`'s loaded `program` all name the same immutable
release. Last exit 0 is necessary but not sufficient if launchd still caches an
older executable. A committed update reloads the updater job so this parity is
automatic. Reload is delegated to the separate
`com.vitalyiegorov.tart-runner-fleet.updater-handoff` one-shot; the updater must
never boot out itself. The handoff waits for a cleared transaction, retries
failures, and verifies the exact loaded program before succeeding.

```sh
launchctl print gui/"$(id -u)"/com.vitalyiegorov.tart-runner-fleet.updater
launchctl print gui/"$(id -u)"/com.vitalyiegorov.tart-runner-fleet.updater-handoff
tail -n 100 "$STATE/update.stdout.log"
tail -n 100 "$STATE/update.stderr.log"
```

Do not force an update through busy, checksum, mode, or readiness failures.

`autoupdate: fleet is not quiescent` on every tick for hours is a defect symptom,
not patience. Read `operations.deadLetters` from the status document: a parked dead
letter no longer defers a release, so a persistent stall means either real work is
running, or the daemon cannot prove a row is parked. If a cleanup has genuinely
stopped retrying and can never complete, discharge it with the guarded command in
[`OPERATIONS.md`](OPERATIONS.md#dead-letters) — never by swapping the plist or
editing `fleet.db`.

## A queue that GitHub says is busy and the node says is empty

`FAIL  ingest delivery` naming a scale set, or `fleet status -o json | jq
'.data.strandedScaleSets'` returning a row, means a set this node SERVES is
holding work on GitHub that is never delivered: `assigned>0 busy>0
registered=0`, no instance here, **this node's queue for the set empty**, for
longer than `timeouts.boot`. It happened three times on 2026-09-21 with jobs
queued for hours while every check passed (issue #336,
[ADR 0056](adr/0056-a-stranded-bound-scale-set-is-recreated.md)).

The empty local queue is part of the finding, not a detail. A set this node
holds **delivered** work for has a listener by proof and is waiting on capacity
— every slot busy — however stale GitHub's counters look. Cross-read it before
acting; a row and a non-empty queue for the same set is a detector defect to
report, never a licence to recreate.

```sh
"$FLEET" doctor --endpoint "$ENDPOINT" --output json | jq '.checks[] | select(.name == "ingest delivery")'
"$FLEET" status --endpoint "$ENDPOINT" --output json | jq '.data.strandedScaleSets'
# cross-read the node's own queue for the set the row names. A scale-set id is
# SCOPE-LOCAL, so both terms come from the finding: strandedScaleSets[].scope
# and strandedScaleSets[].id.
SCOPE=budgie; SET_ID=9
"$FLEET" status --endpoint "$ENDPOINT" --output json | \
  jq --arg scope "$SCOPE" --argjson id "$SET_ID" \
  '.data.scopeQueues[] | select(.scope == $scope and .scaleSetId == $id)'
"$FLEET" scale-sets audit --config "$ROOT/state/fleet.json"   # exits 5 on a bound finding
```

The remedy is to recreate the set — the counters belong to the GitHub object and
no update clears them — and then to restart the daemon:

```sh
"$FLEET" scale-sets recreate NAME --config "$ROOT/state/fleet.json" \
  --confirm recreate-scale-set --reason "stranded bound set, #336"
```

Never run this against a set the audit calls **parked**: that row is evidence, a
sibling node may be serving the set, and deleting it takes its assigned jobs
with it (ADR 0054). The jobs on the recreated set are lost too and must be
re-run.

## Analyze CI traffic

To answer "which repo or workflow pulled the most bytes in the last 24 hours",
run this on each node. Each eligible guest's own interface counters are read
once at the end of its job ([ADR 0080](adr/0080-a-guest-reports-the-bytes-its-job-moved.md)).

```sh
"$FLEET" traffic --endpoint "$ENDPOINT" --since 24h                  # totals, largest first
"$FLEET" traffic --endpoint "$ENDPOINT" --since 24h --output json | \
  jq '.data.totals[:10]'                                             # repo, workflow, profile, bytes
"$FLEET" traffic --endpoint "$ENDPOINT" --since 24h --output json | \
  jq '[.data.jobs[] | select(.measured)] | sort_by(-.receiveBytes)[:10] |
      .[] | {instance, repo, workflowRef, job, runId, receiveBytes}'  # the heaviest single jobs
```

How to read the output:

- Bytes include the LAN Actions cache. On a podman node whose gate runs with
  `--metrics-listen 127.0.0.1:3019`, the WAN share over a window is guest
  receive minus the increase of
  `fleet_cache_gate_bytes_total{upstream="cache",direction="to_guest"}`
  (`curl -fsS http://127.0.0.1:3019/metrics`). That is a node-wide estimate,
  not a per-job one.
- An `unmeasured` job has no bytes, which is not the same as zero bytes. Its
  guest was sampled but could not be read (`exec_failed`, `exec_timeout`,
  `unreadable`, `no_command`).
- Some jobs have no row at all. A guest is sampled only on its drain's first
  stop: a drain that reached only escalated stops, a guest already reclaimed
  as unresponsive, and a guest that died first are never sampled. So the job
  count here can be lower than the jobs the node ran. Missing rows are
  unknown, not zero.
- `truncated: true` means only the newest 1500 jobs were summed. Shorten
  `--since`.
- `jobs[].workflowRef` empty means GitHub never assigned a recorded job to that
  runner, for example an idle runner reclaimed as lingering.
- An older daemon answers HTTP 404 here. Treat that as not measured, not as
  zero.

The same totals are on the daemon's metrics endpoint as
`fleet_guest_network_receive_bytes_total{platform,profile,repo}` (and
`…_transmit_…`, `fleet_guest_network_jobs_total{…,measured}`), for
`increase(...[24h])` in Prometheus. They reset when the daemon restarts; the
`fleet traffic` rows do not.

## Jobs that download tools the image should carry

When the uplink is saturated by CI, read the `image tools` row first. It names
each exact `tool@version` a scale set pins that its base image does not declare
([ADR 0079](adr/0079-a-pinned-tool-the-image-lacks-is-a-red-flag.md), issue
#414). Every such job downloads the tool on every run.

```sh
"$FLEET" doctor --endpoint "$ENDPOINT" --output json | jq '.checks[] | select(.name == "image tools")'
"$FLEET" status --endpoint "$ENDPOINT" --output json | jq '.data.imageTools'
```

Exit 5 here is degraded, not broken. Do not stop, drain or delete anything
because of it. `imageToolCheck` absent means the daemon did not report, which
is unknown and not covered. A passing row only proves the *declarations*
agree. A consumer pin bump that nobody mirrored into `requiresTools` stays
invisible, so when a job log shows a tool download on a green row, report the
missing declaration rather than calling the check wrong.

## A scale set this node excluded on purpose

On a node that declares `membership` ([ADR 0063](adr/0063-an-absent-nodes-work-is-adopted-a-failing-node-excludes-itself.md)), a wedged
set is healed, then **excluded**: its session is released so GitHub sends new
jobs to a sibling advertising the same labels. Read it from the versioned
document, never from the queues, which look idle either way:

```sh
"$FLEET" status --endpoint "$ENDPOINT" --output json | jq '.data.membership, .data.membershipCheck'
"$FLEET" doctor --endpoint "$ENDPOINT" --output json | jq '.checks[] | select(.name == "membership")'
```

`membership` absent means the node declared none, or the daemon predates it.
Treat that as unknown, never as healthy. `state` is the worst stage of any
bound set. `bindings[]` lists only the sets that are not serving. An excluded
set rejoins by itself, so do not resume it by hand. If it keeps failing
probation, follow the stranded-set procedure above (`fleet scale-sets
recreate`).

`session_held` on an observation means GitHub refused a session because
another session still holds the set. That is usually this node's own previous
process after a restart or a laptop waking, and the binding retries each poll.
It is not a reason to restart anything. If it lasts more than a few minutes,
check that no second daemon is configured with that scale set.

## Validate portable MacBook Linux overflow

Issue #358 and [ADR 0060](adr/0060-portable-linux-overflow.md) start with an
unregistered, isolated ARM64 guest. A mixed-architecture inventory fix does not
authorize production routing. Pin the consumer source, lockfile, tool versions,
image digest, and complete shard selection; retain every failed artifact. Test
the same selection on AMD64 and ARM64 before changing its workflow labels.
Missing fixture access or skipped required assertions leaves compatibility
unverified.

Before each stage, read fresh readiness, admission, ordinary resource envelope,
configured floors, allocations, actual disk, and host-use evidence. Distinguish
a Tart allocation from a competing host compiler or owned-device consumer.
Unknown use or insufficient capacity holds the stage; preserve healthy work.
For a reused stopped Linux candidate, follow the existing
[control-socket procedure](LINUX_BASE_IMAGE.md#remove-the-stale-control-socket-before-every-reboot)
only after confirming its exact ownership, stopped state, and unused socket.
Do not infer guest-agent failure from a DHCP lease or a failed `tart exec`.

Keep proposed configuration and workflow patches outside the live configuration
until compatibility, full controller CI, and ADR 0015 promotion evidence pass.
One exact-scope canary must prove the GitHub job and ephemeral runner lifecycle;
withdraw future overflow capacity and drain owned work for rollback. Preserve
the incumbent configuration, images, macOS capacity, and required proofs. Shared
labels alone neither move existing AMD64 jobs nor guarantee Omarchy-first
placement. Studio and Mini expansion requires separate evidence.

## Handle a reproducible defect

1. Preserve the coherent JSON, relevant runner/job state, bounded logs, and
   Tart inventory. Do not expose credentials or JIT configuration.
2. Write one deterministic regression test and record it failing.
3. Implement the smallest fix in the same focused PR.
4. Require at least 99% exact coverage plus race, lint, vet, CPD, deadcode,
   vulnerability, reproducible-build, and Required CI gates.
5. Merge only the exact fully-green head. Verify trusted `main`, production
   release notes, binaries, deterministic archive, CycloneDX SBOMs, and
   SHA256SUMS.
6. Let the updater install while idle, then replay representative work and
   confirm drain, deregistration, VM deletion, and host cleanup.

Do not delete ambiguous VMs, edit durable state, stop authority, or weaken a
safety gate merely to clear a queue. Promotion and rollback remain explicit
operator actions described in [`OPERATIONS.md`](OPERATIONS.md).
