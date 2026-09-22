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
  a queue to clear;
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
